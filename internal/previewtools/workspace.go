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
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
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
	// MaxBaseRuns bounds the check runs the relay makes at the base (its
	// replays and confirmations): status.base.runs keeps at most as many.
	// Past it a check gets no base run, which Decide reads as rule 5 not
	// met (unsure), never a pass.
	MaxBaseRuns = maulv1.MaxBaseRuns
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
	RefusedKey = "key-refused"
	// ReasonBuildFailed: the head did not build, so nothing runs (the
	// round can neither fail nor pass, acceptance.RoleBuild).
	ReasonBuildFailed = "build-failed"
	// ReasonBaseUnbuildable: the base did not build, so nothing runs
	// there; every base run is n/a (status.base.unbuildable).
	ReasonBaseUnbuildable = "base-unbuildable"
	// ReasonBaseBuildError: the base's build did not end in a failure maul
	// can tell is the tree's (a timeout, a signal, cut output, an
	// environmental failure), so nothing runs there and every base run is
	// an error with no cause: no base evidence, never n/a (review VC-1).
	ReasonBaseBuildError = "base-build-error"
	// ReasonVendoredTree, ReasonReplaceOutsideTree and ReasonNoModule are
	// a tree a run program cannot be built against (SPEC Q25): an error,
	// n/a, never a pass, and never a wider network.
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
	case c.ReplayOnly && len(c.Checks) == 0:
		return errors.New("a replay round needs at least one check")
	}
	if err := c.Workspace.validate(); err != nil {
		return err
	}
	return validateChecks(c.Checks)
}

func (w *Workspace) validate() error {
	cli := len(w.Build) > 0 || w.Binary != ""
	switch {
	case cli && w.Language != "":
		return errors.New("a workspace is a cli or a library, not both")
	case cli && (!maulv1.ValidCLIBuild(w.Build) || !maulv1.ValidCLIBinary(w.Binary)):
		return errors.New("a cli workspace's build must write under /work/bin and its binary be a path there")
	case !cli && w.Language != checks.LanguageGo:
		return fmt.Errorf("a library workspace's language must be %s", checks.LanguageGo)
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

// BuildPart is the head build's part fact (acceptance.RoleBuild): it built
// only when it terminated Completed with exit 0; a build that failed or
// timed out terminated otherwise, and one maul could not run has no
// termination, which acceptance reads as not built.
func BuildPart(a BuildArgs, exit int) acceptance.PartFact {
	p := acceptance.PartFact{Name: "build-head", Role: acceptance.RoleBuild}
	switch a.Result {
	case maulv1.BuildBuilt:
		p.LastTermination = &acceptance.Termination{Reason: "Completed"}
	case maulv1.BuildFailed:
		p.LastTermination = &acceptance.Termination{Reason: "Error", ExitCode: int32(nonZero(exit))}
	case maulv1.BuildTimeout:
		p.LastTermination = &acceptance.Termination{Reason: "DeadlineExceeded", ExitCode: int32(nonZero(exit))}
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
// output whole and no environmental marker in it (environmentalFailure, the
// compile cause's screen). Anything else (left processes, a signal, output
// cut, an environmental failure, a build maul could not run) is an error:
// a failed base is unbuildable, which waives rule 5 for every check, so a
// doubt must give no base evidence instead (review VC-1).
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
	case RefusalExit(res.Exit) && !res.Truncated && !environmentalFailure(res.Output):
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

// substitute is the one placeholder pass of every check path (replay,
// confirmation, a dry run, a red dry run; SPEC S2-04): the attempt's
// marker (a program file's only: marker "" elsewhere), then {{bin}}, then
// {{tree}}, each where p has it, then {{run}} through acceptance.Substitute,
// which refuses any other placeholder, those p lacks included. Values are
// maul's and never hold a placeholder.
func substitute(s, run, marker string, p places) (string, error) {
	if marker != "" {
		s = checks.ExecProgram(s, marker)
	}
	if p.bin != "" {
		s = strings.ReplaceAll(s, checks.BinPlaceholder, p.bin)
	}
	if p.tree != "" {
		s = strings.ReplaceAll(s, checks.TreePlaceholder, p.tree)
	}
	return acceptance.Substitute(s, run, nil)
}

// explore substitutes {{bin}} and {{tree}} in an agent's own exec or run
// call, so a check's text runs as it will be saved; {{run}} and {{marker}}
// are a check's alone and stay as written.
func (r *Relay) explore(s string) string {
	p := r.places()
	if p.bin != "" {
		s = strings.ReplaceAll(s, checks.BinPlaceholder, p.bin)
	}
	if p.tree != "" {
		s = strings.ReplaceAll(s, checks.TreePlaceholder, p.tree)
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
		return refusal(res.Refused, refused)
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

func sideRefusal(side toolwire.Side, code, reason string) result {
	return withSide(side, refusal(code, reason))
}

func withSide(side toolwire.Side, res result) result {
	res.side = side.Recorded()
	return res
}

// workspaceExecAttempt runs an exec check once at side: argv and files
// substituted (the marker in the files only), in the attempt's own
// scratch directory, on the tree and build output as the build left them
// (fresh, review VC-2), judged here. The check's timeout goes to the shim,
// which starts it with the program, after the restore: a restore of a large
// tree is never charged to the check.
func (r *Relay) workspaceExecAttempt(ctx context.Context, e checks.ExecCheck, run string,
	side toolwire.Side) []toolwire.StepRecord {
	marker := checks.ExecMarker(randomMarkerNonce())
	p := r.places()
	req := &toolexec.Request{Side: side, Run: run, Env: r.programEnv(), Fresh: true,
		TimeoutMs: timeoutMs(e.Timeout)}
	for _, a := range e.Argv {
		s, err := substitute(a, run, "", p)
		if err != nil {
			return errStep("the program could not be built: " + err.Error())
		}
		req.Argv = append(req.Argv, s)
	}
	for _, f := range e.Files {
		s, err := substitute(f.Content, run, marker, p)
		if err != nil {
			return errStep("the program could not be built: " + err.Error())
		}
		req.Files = append(req.Files, toolwire.File{Name: f.Path, Content: []byte(s)})
	}
	if err := req.Check(); err != nil {
		return errStep("the program could not be built: " + err.Error())
	}
	res, err := r.sandbox(side).Do(ctx, req)
	return judgeWorkspace(res, err, e.Expect, marker, run, p)
}

// timeoutMs is a check's timeout, which checks.Validate admitted, in whole
// milliseconds rounded up; 0, the shim's own bound, for none.
func timeoutMs(timeout string) int64 {
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// withCheckTimeout bounds ctx by a check's timeout, which checks.Validate
// admitted; none leaves the shim's own bound.
func withCheckTimeout(ctx context.Context, timeout string) (context.Context, context.CancelFunc) {
	if d, err := time.ParseDuration(timeout); err == nil && d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

func errStep(detail string) []toolwire.StepRecord {
	return []toolwire.StepRecord{{Result: toolwire.ResultError, Detail: detail}}
}

// causedStep is errStep recording cause (toolwire.StepCause), "" for none.
func causedStep(cause toolwire.StepCause, detail string) []toolwire.StepRecord {
	steps := errStep(detail)
	steps[0].Cause = cause
	return steps
}

// judgeWorkspace judges a workspace program's run against x (DESIGN 4.3,
// SPEC-S2 O4 and 7.10). How a run maps to a result, by the first that
// holds:
//
//   - the shim did not answer, refused, the run left processes behind
//     (Unclean), or was killed at its time limit: error, n/a; a hanging or
//     leaking program is no evidence about the outcome at either side;
//   - the program printed its attempt's marker: fail, by assertion when it
//     named the assertion's id (checks.ExecAssertion);
//   - the output was cut: error (a marker past the cut would be lost);
//   - the exit was not the expected one: fail, never by assertion: a
//     crashed, unbuilt or argument-refusing program shows that too (O4);
//   - with an expected exit met, stdout lacks the expected text, or a
//     stdoutJSON pointer resolves in a JSON stdout to another value: fail
//     by assertion ("stdout"); with no exit expected, or stdout not JSON or
//     lacking the pointer, the same miss fails without one;
//   - otherwise pass.
//
// Then a run that did not pass whose output names a bind or connection
// error (Result.NetError) is an error instead: on a pod whose loopback both
// sides share, the other side's leftover can cause it (SPEC 8a). A run that
// passes is never turned into an error by it, so a check expecting a
// "connection refused" message still passes.
func judgeWorkspace(res toolexec.Result, err error, x checks.ExecExpect, marker, run string,
	p places) []toolwire.StepRecord {
	switch {
	case err != nil:
		return errStep("the sandbox did not answer")
	case res.Refused != "":
		return errStep("the sandbox refused the program")
	case res.Unclean:
		return errStep("processes the program started outlived it, so maul cannot judge the run")
	case res.TimedOut:
		return errStep("the program was killed at its time limit, so maul cannot judge the run")
	}
	steps := judgeOutput(res, x, marker, run, p)
	if steps[0].Result == toolwire.ResultFail && res.NetError {
		return errStep("the output names a bind or connection error, which the other side's programs can cause")
	}
	return steps
}

// judgeOutput is judgeWorkspace's judgement of a run that ended on its own.
func judgeOutput(res toolexec.Result, x checks.ExecExpect, marker, run string, p places) []toolwire.StepRecord {
	fail := func(detail, id string) []toolwire.StepRecord {
		return []toolwire.StepRecord{{Result: toolwire.ResultFail, Detail: detail, Assertion: id != "",
			AssertionID: toolwireID(id)}}
	}
	if checks.ExecMarked(res.Output, marker) {
		id, asserted := checks.ExecAssertion(res.Output, marker)
		if !asserted {
			return fail(markedWithoutID, "")
		}
		return fail(markedDetail(res.Output, marker), "marker "+id)
	}
	switch {
	case res.Truncated:
		return errStep(fmt.Sprintf("the output is over %d bytes, so maul cannot judge it", toolexec.MaxOutputBytes))
	case x.Exit != nil && int32(res.Exit) != *x.Exit:
		return fail(fmt.Sprintf("exit %d, expected %d", res.Exit, *x.Exit), "")
	}
	// After the expected exit, content missing is the outcome's assertion.
	content := func(ok bool) string {
		if ok && x.Exit != nil {
			return "stdout"
		}
		return ""
	}
	if x.StdoutContains != "" {
		needle, err := substitute(x.StdoutContains, run, "", p)
		if err != nil {
			return errStep("the expected text could not be built")
		}
		if !strings.Contains(res.Output, needle) {
			return fail("the output does not contain the expected text", content(true))
		}
	}
	doc, valid := decodeJSON([]byte(res.Output))
	for _, ptr := range sortedKeys(x.StdoutJSON) {
		raw, present := pointerValue(doc, valid, ptr)
		if !checks.MatchJSON(x.StdoutJSON[ptr], run, nil, raw, present) {
			return fail("stdoutJSON "+fmt.Sprintf("%q", ptr)+" did not match", content(valid && present))
		}
	}
	return []toolwire.StepRecord{{Result: toolwire.ResultPass}}
}

// maxMarkedBytes bounds the program's own words a marked run's detail
// keeps, well inside a step record's detail bound.
const maxMarkedBytes = 1 << 10

// markedDetail is the detail of a run whose program printed marker: maul's
// words, then what the program printed after the marker on each such line
// (its own evidence, such as the value it observed; gate D-B1), joined by
// "; ", valid UTF-8 and cut to maxMarkedBytes. The marker is never kept.
// The program's words are data, which every reader of a detail shows as
// untrusted.
func markedDetail(output, marker string) string {
	const words = "the program printed its assertion marker"
	var said []string
	for line := range strings.SplitSeq(output, "\n") {
		if rest, ok := strings.CutPrefix(line, marker); ok {
			if rest = strings.TrimSpace(strings.ReplaceAll(rest, marker, "")); rest != "" {
				said = append(said, rest)
			}
		}
	}
	if len(said) == 0 {
		return words
	}
	s := strings.ToValidUTF8(strings.Join(said, "; "), "�")
	if len(s) > maxMarkedBytes {
		cut := maxMarkedBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return words + ": " + s
}

// baseUnbuiltRun is the base run of check c when the base did not build:
// no shim is asked, and the run is an error. On an unbuildable base (its
// build failed) the error is n/a, so a Required base still has its run and
// reads unbuildable, never missing; on a base whose build did not finish it
// has no cause: no base evidence, never n/a (review VC-1).
func baseUnbuiltRun(c checks.Check, mode toolwire.CheckRunMode, unbuildable bool) toolwire.CheckRunRecord {
	steps := causedStep(toolwire.CauseBaseUnbuildable, ReasonBaseUnbuildable+": the base tree did not build")
	if !unbuildable {
		steps = errStep(ReasonBaseBuildError + ": the base tree's build did not finish")
	}
	steps[0].Side = toolwire.SideBase
	a := toolwire.Attempt{Result: toolwire.ResultError, Steps: steps}
	return toolwire.CheckRunRecord{Digest: bareDigestOf(checks.Digest(c)), CheckID: c.ID, Mode: mode,
		Result: toolwire.DeriveResult([]toolwire.Attempt{a}), Attempts: []toolwire.Attempt{a}}
}

// baseRun runs c at the base for a replay or a confirmation: alone, under
// fresh tokens, when the base built; an unbuilt run when it did not; none
// once MaxBaseRuns are spent (ok false).
func (r *Relay) baseRun(ctx context.Context, c checks.Check, mode toolwire.CheckRunMode) (toolwire.CheckRunRecord,
	bool) {
	if r.baseRuns >= MaxBaseRuns {
		return toolwire.CheckRunRecord{}, false
	}
	r.baseRuns++
	if !r.baseBuilt {
		return baseUnbuiltRun(c, mode, r.baseUnbuildable), true
	}
	return r.runCheckAt(ctx, c, mode, false, toolwire.SideBase), true
}

// workspaceSurface is the checks.Context of this workspace's surface.
func (r *Relay) workspaceSurface() checks.Context {
	w := r.cfg.Workspace
	return checks.Context{Workspace: &checks.Workspace{Binary: w.Binary, Language: w.Language}}
}

// reasonNoPreview is maul's words for a preview tool in a workspace.
const reasonNoPreview = "this tool pod has no preview: a workspace offers exec, files, run and checks"
