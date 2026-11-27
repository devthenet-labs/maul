// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// A workspace (SPEC-S2 S2-08; DESIGN 2.4, 2.5) is the relay of an
// agents-namespace tool pod: no preview, one repository's head and base
// trees, each built and run in its own sandbox (O2), reached over that
// sandbox's own exec socket. The relay routes every call to a side by the
// socket it dials, never by anything a sandbox says, so a run is tagged base
// only when the base's shim ran it.

// The tool pod's places, as jobs.CreateToolPod mounts them (a test pins
// them): the pristine trees at DefaultTreesDir/<side>, each side's exec
// socket at DefaultExecDir/<side>/sock, the :8090 key beside the config at
// DefaultConfigDir/KeyFile, and the sandboxes' writable tree at WorkTree.
const (
	DefaultTreesDir = "/trees"
	DefaultExecDir  = "/run/maul-exec"
	KeyFile         = "tools-key"
	WorkTree        = toolexec.WorkDir + "/tree"
)

// HeadSandboxUID and BaseSandboxUID are the UIDs each side's shim runs as
// in the tool pod (jobs.CreateToolPod's, a test pins them): a side's client
// accepts only a shim served by its side's UID.
const (
	HeadSandboxUID = 65540
	BaseSandboxUID = 65541
)

// A workspace's bounds. The CRD's markers repeat them (a test pins them).
const (
	// MinBuildTimeoutSeconds and MaxBuildTimeoutSeconds bound a tree's
	// build (surfaces.cli.buildTimeout: 1m to 20m).
	MinBuildTimeoutSeconds = 60
	MaxBuildTimeoutSeconds = int(maulv1.MaxCLIBuildTimeout / time.Second)
	// MaxWorkspaceTimeoutSeconds bounds a workspace's wall clock: the
	// agents verification's 75m.
	MaxWorkspaceTimeoutSeconds = int(maulv1.MaxAgentsVerificationTimeout / time.Second)
	// maxBuilds is the builds the relay records: one of each tree.
	maxBuilds = 2
	// defaultReadyWait bounds the wait for a sandbox's shim to serve
	// before its build: a native sidecar is started, not ready, when tools
	// starts.
	defaultReadyWait = 2 * time.Minute
)

// Refusal reasons and codes of a workspace, the first words of a refusal's
// reason so a reader can tell them apart.
const (
	// RefusedKey is a call without the per-evaluation key (SPEC O7).
	RefusedKey = toolwire.RefusedKey
	// ReasonBuildFailed: the head did not build, so nothing runs (the
	// round can neither fail nor pass, httpstep.RoleBuild).
	ReasonBuildFailed = "build-failed"
	// ReasonBaseUnbuildable: the base did not build, so nothing runs
	// there: an exec or run at the base is refused.
	ReasonBaseUnbuildable = "base-unbuildable"
	// ReasonBaseBuildError: the base's build did not end in a failure maul
	// can tell is the tree's (a timeout, a signal, cut output, an
	// environmental failure), so nothing runs there either (review VC-1).
	ReasonBaseBuildError = "base-build-error"
	// ReasonVendoredTree, ReasonReplaceOutsideTree and ReasonNoModule are
	// a tree a run program cannot be built against (SPEC Q25): the run is
	// refused, never a wider network.
	ReasonVendoredTree       = "vendored-tree"
	ReasonReplaceOutsideTree = "replace-outside-tree"
	ReasonNoModule           = "no-module"
)

// Workspace is what an agents tool pod builds and runs: a cli (Build and
// Binary) or a library (Language), from intent-controller's resolved
// PreviewVerification workspace.
type Workspace struct {
	// Build is a cli's build argv, run in the writable tree; it writes
	// Binary under /work/bin.
	Build []string `json:"build,omitempty"`
	// Binary is a cli's built binary, what {{bin}} stands for.
	Binary string `json:"binary,omitempty"`
	// Language is a library's language (go); its build is libraryBuild.
	Language string `json:"language,omitempty"`
	// BuildTimeoutSeconds bounds each tree's build.
	BuildTimeoutSeconds int `json:"buildTimeoutSeconds"`
	// SearchPath is the repository image's sanitized PATH, sent as every
	// program's PATH: the shim finds programs only there.
	SearchPath string `json:"searchPath"`
	// Env is the sandboxes' environment as jobs renders it for the tool
	// pod: the build's network profile (GOPROXY, NPM_CONFIG_*, the proxy
	// variables, …) and, for a Go image without one, its GOMODCACHE. The
	// shim passes a program none of its own environment, so the relay sends
	// these with every request, less what the shim owns or refuses
	// (toolexec.AllowedEnv) and PATH (SearchPath).
	Env []toolexec.EnvVar `json:"env,omitempty"`
}

// libraryBuild is a Go library's build: every package of the tree.
var libraryBuild = []string{"go", "build", "./..."}

// envNamePattern is a variable's name.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// validateWorkspace holds a workspace config to what the relay runs.
func (c Config) validateWorkspace() error {
	switch {
	case c.Workspace == nil:
		return errors.New("a workspace config names its workspace")
	case len(c.Components) > 0 || c.Host != "":
		return errors.New("a workspace has no preview: no components and no host")
	case c.Browser || c.Sandbox:
		return errors.New("a workspace has no browser, and its two sandboxes are implied")
	case c.Viewer != "" && (len(c.Viewer) > 128 || !printableASCII(c.Viewer)):
		return errors.New("viewer must be at most 128 printable characters")
	case c.MaxToolCalls < maulv1.MinToolCalls || c.MaxToolCalls > maulv1.MaxToolCallsLimit:
		return fmt.Errorf("maxToolCalls must be %d to %d", maulv1.MinToolCalls, maulv1.MaxToolCallsLimit)
	case c.TimeoutSeconds < 1 || c.TimeoutSeconds > MaxWorkspaceTimeoutSeconds:
		return fmt.Errorf("timeoutSeconds must be 1 to %d", MaxWorkspaceTimeoutSeconds)
	}
	return c.Workspace.validate()
}

func (w *Workspace) validate() error {
	cli := len(w.Build) > 0 || w.Binary != ""
	switch {
	case cli && w.Language != "":
		return errors.New("a workspace is a cli or a library, not both")
	case cli && (!maulv1.ValidCLIBuild(w.Build) || !maulv1.ValidCLIBinary(w.Binary)):
		return errors.New("a cli workspace's build must write under /work/bin and its binary be a path there")
	case !cli && w.Language != toolwire.LanguageGo:
		return fmt.Errorf("a library workspace's language must be %s", toolwire.LanguageGo)
	case w.BuildTimeoutSeconds < MinBuildTimeoutSeconds || w.BuildTimeoutSeconds > MaxBuildTimeoutSeconds:
		return fmt.Errorf("buildTimeoutSeconds must be %d to %d", MinBuildTimeoutSeconds, MaxBuildTimeoutSeconds)
	}
	if err := validSearchPath(w.SearchPath); err != nil {
		return err
	}
	if len(w.Env) > toolexec.MaxEnv {
		return fmt.Errorf("more than %d variables", toolexec.MaxEnv)
	}
	seen := map[string]bool{}
	for _, e := range w.Env {
		if !envNamePattern.MatchString(e.Name) || seen[e.Name] {
			return fmt.Errorf("variable %q is not a new name", e.Name)
		}
		seen[e.Name] = true
		if len(e.Value) > toolexec.MaxEnvValueBytes || strings.ContainsRune(e.Value, 0) {
			return fmt.Errorf("variable %s is not a bounded value", e.Name)
		}
	}
	return nil
}

// validSearchPath admits ":"-joined absolute entries with no control
// character, as runnerimage.SanitizePath writes them.
func validSearchPath(p string) error {
	if p == "" || len(p) > 4096 {
		return errors.New("searchPath must be 1 to 4096 bytes")
	}
	for e := range strings.SplitSeq(p, ":") {
		if !filepath.IsAbs(e) || strings.IndexFunc(e, unicode.IsControl) >= 0 {
			return fmt.Errorf("searchPath entry %q is not an absolute path", e)
		}
	}
	return nil
}

// build is the workspace's build argv.
func (w *Workspace) build() []string {
	if w.Language != "" {
		return libraryBuild
	}
	return w.Build
}

// Sandbox is one side's workspace exec shim: toolexec.Client in a tool pod,
// a fake in tests.
type Sandbox interface {
	Do(ctx context.Context, req *toolexec.Request) (toolexec.Result, error)
	Ready(ctx context.Context) error
}

// NewSandboxes are the clients of the head's and the base's shims, at
// <execDir>/<side>/sock, served by headUID and baseUID. A client only dials
// its socket: the relay does nothing else under a socket's directory, which
// a sandbox can write. A sandbox can replace its socket there, so a client
// refuses a socket that is a link or is served by another UID than its
// side's (toolexec.NewSideClient): the call errs, never a result of the
// other side's shim.
func NewSandboxes(execDir string, headUID, baseUID int) (head, base Sandbox) {
	return toolexec.NewSideClient(filepath.Join(execDir, string(toolwire.SideHead), "sock"), headUID),
		toolexec.NewSideClient(filepath.Join(execDir, string(toolwire.SideBase), "sock"), baseUID)
}

// LoadKey reads the :8090 key from dir's KeyFile: one key
// (toolwire.IsKey), a trailing newline allowed.
func LoadKey(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, KeyFile))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", err
	}
	key := strings.TrimRight(string(b), "\r\n")
	if !toolwire.IsKey(key) {
		return "", errors.New("the key file does not hold one key")
	}
	return key, nil
}

// sandbox is side's sandbox: the base's for base, else the head's.
func (r *Relay) sandbox(side toolwire.Side) Sandbox {
	if side.Recorded() == toolwire.SideBase {
		return r.base
	}
	return r.head
}

// BuildArgs are a build entry's args (verb build, kind relay, its side the
// tree's): what ran and how it ended. The entry's exit is the build's exit
// code, its output the build's output's tail.
type BuildArgs struct {
	Argv       []string           `json:"argv"`
	TimeoutMs  int64              `json:"timeoutMs"`
	Result     maulv1.BuildResult `json:"result"`
	DurationMs int64              `json:"durationMs"`
}

// BuildPart is the head build's part fact (httpstep.RoleBuild): it built
// only when it terminated Completed with exit 0; a build that failed or
// timed out terminated otherwise, and one maul could not run has no
// termination, which acceptance reads as not built.
func BuildPart(a BuildArgs, exit int) httpstep.PartFact {
	p := httpstep.PartFact{Name: "build-head", Role: httpstep.RoleBuild}
	switch a.Result {
	case maulv1.BuildBuilt:
		p.LastTermination = &httpstep.Termination{Reason: "Completed"}
	case maulv1.BuildFailed:
		p.LastTermination = &httpstep.Termination{Reason: "Error", ExitCode: int32(nonZero(exit))}
	case maulv1.BuildTimeout:
		p.LastTermination = &httpstep.Termination{Reason: "DeadlineExceeded", ExitCode: int32(nonZero(exit))}
	}
	return p
}

// RefusalExit reports an exit a build can refuse its tree with: a normal
// exit of 1 to 125. -1 is a signal; a shell reports what ended its child by
// exit code alone, 126 and 127 a command it could not run and 128+N signal
// N, which is no evidence about the tree (review VC-1).
func RefusalExit(exit int) bool { return exit >= 1 && exit <= 125 }

// nonZero is exit, or 1 for an exit of 0 that did not build.
func nonZero(exit int) int {
	if exit == 0 {
		return 1
	}
	return exit
}

// built is one side's build outcome.
type built struct {
	args BuildArgs
	res  toolexec.Result
}

// buildTrees builds the head and the base in parallel and records the two
// builds, head first, as the ledger's first entries.
func (r *Relay) buildTrees(ctx context.Context) {
	var wg sync.WaitGroup
	var head, base built
	wg.Go(func() { head = r.buildSide(ctx, toolwire.SideHead) })
	wg.Go(func() { base = r.buildSide(ctx, toolwire.SideBase) })
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range []struct {
		side toolwire.Side
		b    built
	}{{toolwire.SideHead, head}, {toolwire.SideBase, base}} {
		_, _ = r.record(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbBuild,
			Side: b.side.Recorded(), Args: mustArgs(b.b.args), Exit: b.b.res.Exit, Output: ledgerTail(b.b.res.Output)})
	}
	r.headBuilt = head.args.Result == maulv1.BuildBuilt
	r.baseBuilt = base.args.Result == maulv1.BuildBuilt
	r.baseUnbuildable = base.args.Result == maulv1.BuildFailed
}

// ledgerTail is a build's output as its entry can hold it: a build's tail
// (toolexec.MaxBuildOutputBytes) may be longer than an entry's output
// (toolwire.MaxOutputBytes), which would leave the build unrecorded; its
// end, where a build says why it failed, is kept, valid UTF-8.
func ledgerTail(s string) string {
	if len(s) <= toolwire.MaxOutputBytes {
		return s
	}
	start := len(s) - (toolwire.MaxOutputBytes - len(buildCutMarker))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return buildCutMarker + s[start:]
}

// buildCutMarker opens a build output ledgerTail cut.
const buildCutMarker = "[maul: the build's output is cut to its end]\n"

// buildSide runs side's build once its shim serves. Only a build that ran
// and exited 0, leaving nothing behind, built. Only one that ran to its own
// end and refused the tree failed: a normal exit (RefusalExit), clean, its
// output whole, naming no bind or connection error (Result.NetError: head
// and base build at once on one loopback) and no environmental marker in it
// (environmentalFailure, the compile cause's screen). Anything else (left
// processes, a signal, output cut, a bind or connection error, an
// environmental failure, a build maul could not run) is an error, never
// read as the tree refusing itself (review VC-1).
func (r *Relay) buildSide(ctx context.Context, side toolwire.Side) built {
	w := r.cfg.Workspace
	args := BuildArgs{Argv: w.build(), TimeoutMs: int64(w.BuildTimeoutSeconds) * 1000, Result: maulv1.BuildError}
	sb := r.sandbox(side)
	if err := r.awaitSandbox(ctx, sb); err != nil {
		return built{args: args, res: toolexec.Result{Output: "maul: the sandbox did not come up"}}
	}
	start := time.Now()
	res, err := sb.Do(ctx, &toolexec.Request{Argv: args.Argv, Side: side.Recorded(), Kind: toolexec.KindBuild,
		Env: r.programEnv(), TimeoutMs: args.TimeoutMs})
	args.DurationMs = res.DurationMs
	if args.DurationMs == 0 {
		args.DurationMs = time.Since(start).Milliseconds()
	}
	switch {
	case err != nil || res.Refused != "":
		return built{args: args, res: toolexec.Result{Output: "maul: the sandbox did not run the build"}}
	case res.TimedOut:
		args.Result = maulv1.BuildTimeout
	case res.Unclean:
		res.Output += "\nmaul: processes the build started outlived it"
	case res.Exit == 0:
		args.Result = maulv1.BuildBuilt
	case RefusalExit(res.Exit) && !res.Truncated && !res.NetError && !environmentalFailure(res.Output):
		args.Result = maulv1.BuildFailed
	}
	return built{args: args, res: res}
}

// awaitSandbox waits, within the ready wait, for sb's shim to serve.
func (r *Relay) awaitSandbox(ctx context.Context, sb Sandbox) error {
	ctx, cancel := context.WithTimeout(ctx, r.readyWait)
	defer cancel()
	for {
		if err := sb.Ready(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// programEnv is every workspace request's environment: PATH the image's
// search path, then the configured environment the shim admits, in order.
func (r *Relay) programEnv() []toolexec.EnvVar {
	w := r.cfg.Workspace
	env := []toolexec.EnvVar{{Name: "PATH", Value: w.SearchPath}}
	for _, e := range w.Env {
		if e.Name != "PATH" && toolexec.AllowedEnv(e.Name) {
			env = append(env, e)
		}
	}
	return env
}

// compileEnv is a run program's compile environment (SPEC Q25, SP-8): the
// program environment with the module network off, the module files
// read-only and the user's go env file ignored, so a compile never widens
// the network nor rewrites go.mod or go.sum.
func (r *Relay) compileEnv() []toolexec.EnvVar {
	owned := map[string]string{"GOPROXY": "off", "GOFLAGS": "-mod=readonly", "GOENV": "off"}
	var env []toolexec.EnvVar
	for _, e := range r.programEnv() {
		if _, ok := owned[e.Name]; !ok {
			env = append(env, e)
		}
	}
	for _, name := range []string{"GOPROXY", "GOFLAGS", "GOENV"} {
		env = append(env, toolexec.EnvVar{Name: name, Value: owned[name]})
	}
	return env
}

// runnable reports why side cannot run a program now ("" when it can):
// the head did not build, or the base did not and side is the base.
func (r *Relay) runnable(side toolwire.Side) string {
	switch {
	case !r.headBuilt:
		return ReasonBuildFailed + ": the head tree did not build, so nothing runs"
	case side.Recorded() == toolwire.SideBase && r.baseUnbuildable:
		return ReasonBaseUnbuildable + ": the base tree did not build, so nothing runs there"
	case side.Recorded() == toolwire.SideBase && !r.baseBuilt:
		return ReasonBaseBuildError + ": the base tree's build did not finish, so nothing runs there"
	}
	return ""
}

// places are what a workspace's placeholders stand for: the cli's binary
// ("" in a library and in the slot) and the writable tree ("" in the slot).
type places struct{ bin, tree string }

// places are this relay's: none in the slot.
func (r *Relay) places() places {
	if !r.cfg.IsWorkspace() {
		return places{}
	}
	return places{bin: r.cfg.Workspace.Binary, tree: r.workTree}
}

// explore substitutes {{bin}} and {{tree}} in an agent's exec or run call;
// anything else in braces is the agent's own text and runs as written.
func (r *Relay) explore(s string) string {
	p := r.places()
	if p.bin != "" {
		s = strings.ReplaceAll(s, toolwire.BinPlaceholder, p.bin)
	}
	if p.tree != "" {
		s = strings.ReplaceAll(s, toolwire.TreePlaceholder, p.tree)
	}
	return s
}

// workspaceExec answers `maul-tool exec` in a workspace: argv run at the
// side asked for, in a fresh scratch directory of its own.
func (r *Relay) workspaceExec(ctx context.Context, req *toolwire.ExecRequest) result {
	side := req.Side.Recorded()
	if why := r.runnable(side); why != "" {
		return sideRefusal(side, toolwire.RefusedUnavailable, why)
	}
	wreq := &toolexec.Request{Side: side, Run: r.token(), Env: r.programEnv()}
	for _, a := range req.Argv {
		wreq.Argv = append(wreq.Argv, r.explore(a))
	}
	for _, f := range req.Files {
		wreq.Files = append(wreq.Files, toolwire.File{Name: f.Name, Content: []byte(r.explore(string(f.Content)))})
	}
	res, err := r.sandbox(side).Do(ctx, wreq)
	return withSide(side, programResult(res, err, "the sandbox refused the exec"))
}

// programResult is a program's run as a call's result: its output and exit,
// maul's notes on how it ended in the reason.
func programResult(res toolexec.Result, err error, refused string) result {
	switch {
	case err != nil:
		return refusal(toolwire.RefusedUnavailable, "the sandbox did not answer")
	case res.Refused != "":
		return refusal(sandboxRefused(res.Refused), refused)
	}
	exit := res.Exit
	out := result{output: res.Output, inline: res.Output, exit: exit, observed: &toolwire.Observed{Exit: &exit}}
	var notes []string
	if res.TimedOut {
		notes = append(notes, "the program was killed at its time limit")
	}
	if res.Unclean {
		notes = append(notes, "processes the program started outlived it")
	}
	if res.NetError {
		notes = append(notes, "the output names a bind or connection error")
	}
	out.reason = strings.Join(notes, "; ")
	return out
}

// sandboxRefused is a sandbox's refusal code as the ledger records it: as
// given, except a code only the relay's gate writes (no key, past
// maxToolCalls), which the index reads as "nothing ran" (toolledger's
// ranNothing). A sandbox has sent nothing under either; one that does is
// not maul's shim, and its refusal is recorded unavailable.
func sandboxRefused(code string) string {
	if code == toolwire.RefusedKey || code == toolwire.RefusedToolCalls {
		return toolwire.RefusedUnavailable
	}
	return code
}

func sideRefusal(side toolwire.Side, code, reason string) result {
	return withSide(side, refusal(code, reason))
}

func withSide(side toolwire.Side, res result) result {
	res.side = side.Recorded()
	return res
}

// reasonNoPreview is maul's words for a preview tool in a workspace.
const reasonNoPreview = "this tool pod has no preview: a workspace offers exec, files and run"
