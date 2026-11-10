// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// A workspace shim's places and bounds (SPEC S2-07, Q23): the sandbox of an
// agents-namespace tool pod, running the repository's accepted image over
// one side's writable tree.
const (
	// WorkDir is the workspace: the tree at WorkDir/tree, the build output
	// at WorkDir/bin, each run's scratch at WorkDir/scratch/<run>.
	WorkDir = "/work"
	// TmpDir is the programs' TMPDIR, an emptyDir of the sandbox's own.
	TmpDir = "/tmp"
	// DefaultBuildTimeout is a build's timeout when the request names none;
	// MaxBuildTimeout the most one may name (SPEC O9).
	DefaultBuildTimeout = 10 * time.Minute
	MaxBuildTimeout     = 20 * time.Minute
	// MaxBuildOutputBytes bounds a build's output, which keeps its tail
	// (where a compiler's errors are), markers included.
	MaxBuildOutputBytes = 256 << 10
	// MaxEnv bounds the variables a request passes, MaxEnvValueBytes each
	// value.
	MaxEnv           = 64
	MaxEnvValueBytes = 4 << 10
	// DefaultPath is PATH when the request passes none.
	DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// TailMarker begins the output of a build whose earlier output was cut.
const TailMarker = "[maul: earlier output cut]\n"

// Kind is what a workspace request runs.
type Kind string

// Kinds.
const (
	// KindExec (or no kind) runs a program in its run's scratch directory,
	// for at most Timeout and MaxOutputBytes of output, as the slot shim
	// does.
	KindExec Kind = "exec"
	// KindBuild runs the tree's build in the tree, for the request's
	// timeout and MaxBuildOutputBytes of output's tail.
	KindBuild Kind = "build"
)

// EnvVar is one variable a request passes to its program.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Request is one call of the shim: the wire's exec (an ExecRequest's
// fields, which is all a slot shim takes) and, for a workspace shim only,
// what tools adds from its own configuration: the kind, the run whose
// scratch directory an exec uses, the environment, and a build's timeout.
// A slot shim refuses a request that sets any of these.
type Request struct {
	Argv  []string        `json:"argv"`
	Files []toolwire.File `json:"files,omitempty"`
	Side  toolwire.Side   `json:"side,omitempty"`
	Kind  Kind            `json:"kind,omitempty"`
	// Run names the exec's scratch directory, WorkDir/scratch/<Run>: one
	// per run token, so the steps of one run (a run verb's compile, then
	// its execution) share it and no two runs do.
	Run string `json:"run,omitempty"`
	// Env are the program's variables from tools: the Repository's search
	// path as PATH and the build's network profile, by AllowedEnv. Never
	// the shim's own environment.
	Env []EnvVar `json:"env,omitempty"`
	// TimeoutMs is a build's timeout, at most MaxBuildTimeout; zero is
	// DefaultBuildTimeout. An exec takes none.
	TimeoutMs int64 `json:"timeoutMs,omitempty"`
	// Fresh runs an exec on the tree and build output exactly as the last
	// build that built left them, whatever an earlier program changed
	// there (review VC-2): what a check run starts from. With none to
	// restore exactly, nothing runs. A build takes none.
	Fresh bool `json:"fresh,omitempty"`
}

var (
	runName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// Check implements toolwire.Request: the wire's exec grammar plus the
// workspace fields' own; whether this shim takes them is Do's.
func (r *Request) Check() error {
	wire := toolwire.ExecRequest{Argv: r.Argv, Files: r.Files, Side: r.Side}
	if err := wire.Check(); err != nil {
		return err
	}
	switch {
	case r.Kind != "" && r.Kind != KindExec && r.Kind != KindBuild:
		return fmt.Errorf("kind %q is not exec or build", r.Kind)
	case r.Run != "" && !runName.MatchString(r.Run):
		return fmt.Errorf("run %q is not a name", r.Run)
	case r.TimeoutMs < 0 || r.TimeoutMs > MaxBuildTimeout.Milliseconds():
		return fmt.Errorf("timeout must be within %s", MaxBuildTimeout)
	case len(r.Env) > MaxEnv:
		return errors.New("too many variables")
	}
	seen := map[string]bool{}
	for _, e := range r.Env {
		if !envName.MatchString(e.Name) || seen[e.Name] {
			return fmt.Errorf("variable %q is not a new name", e.Name)
		}
		seen[e.Name] = true
		if len(e.Value) > MaxEnvValueBytes || !utf8.ValidString(e.Value) || strings.ContainsRune(e.Value, 0) {
			return fmt.Errorf("variable %s is not a bounded value", e.Name)
		}
	}
	return nil
}

// workspace reports a request that sets a field only a workspace shim takes.
func (r *Request) workspace() bool {
	return r.Kind != "" || r.Run != "" || len(r.Env) > 0 || r.TimeoutMs != 0 || r.Fresh
}

// build reports a build request.
func (r *Request) build() bool { return r.Kind == KindBuild }

// buildTimeout is a build request's timeout.
func (r *Request) buildTimeout() time.Duration {
	if r.TimeoutMs == 0 {
		return DefaultBuildTimeout
	}
	return time.Duration(r.TimeoutMs) * time.Millisecond
}

// allowedEnv names the variables tools may pass: the search path and the
// build's network profile as jobs renders it for a (build, intent) Job
// (go, npm, pip and the tunnel's client presets). allowedEnvPrefixes are
// the ecosystems' own families.
var (
	allowedEnv = map[string]bool{
		"PATH": true, "LANG": true, "SSL_CERT_FILE": true,
		"GOPROXY": true, "GOFLAGS": true, "GOSUMDB": true, "GONOSUMDB": true, "GONOPROXY": true,
		"GOPRIVATE": true, "GOINSECURE": true, "GOAUTH": true, "GOENV": true, "GOPATH": true, "GOMODCACHE": true,
		"HTTPS_PROXY": true, "https_proxy": true, "NO_PROXY": true, "no_proxy": true,
		"JAVA_TOOL_OPTIONS": true, "UV_PYTHON_DOWNLOADS": true, "COREPACK_ENABLE_DOWNLOAD_PROMPT": true,
	}
	allowedEnvPrefixes = []string{"NPM_CONFIG_", "npm_config_", "pnpm_config_", "PIP_", "YARN_", "CARGO_",
		"NUGET_", "DOTNET_"}
)

// ownedEnv are the variables the shim itself sets in a workspace, which a
// request may not: the sandbox's own places, and no toolchain download.
var ownedEnv = []string{"HOME", "TMPDIR", "GOCACHE", "GOTOOLCHAIN", "NPM_CONFIG_CACHE"}

// AllowedEnv reports a variable a workspace request may pass: on the
// allowlist and not one the shim owns (HOME, TMPDIR, GOCACHE, GOTOOLCHAIN,
// the npm cache), which tools must leave out of what it passes.
func AllowedEnv(name string) bool {
	for _, o := range ownedEnv {
		if strings.EqualFold(name, o) {
			return false
		}
	}
	if allowedEnv[name] {
		return true
	}
	for _, p := range allowedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// checkWorkspace holds a request to what this workspace takes.
func (s *Server) checkWorkspace(r *Request) error {
	if r.build() {
		if len(r.Files) > 0 || r.Run != "" || r.Fresh {
			return errors.New("a build takes no files, no run and no fresh tree")
		}
	} else if r.Run == "" || r.TimeoutMs != 0 {
		return errors.New("an exec takes a run and no timeout")
	}
	for _, e := range r.Env {
		if !AllowedEnv(e.Name) {
			return fmt.Errorf("variable %s is not one a program may be passed", e.Name)
		}
		switch e.Name {
		case "PATH":
			for _, dir := range filepath.SplitList(e.Value) {
				if !filepath.IsAbs(dir) {
					return errors.New("PATH holds a relative directory")
				}
			}
		case "GOMODCACHE":
			// The image's baked cache or the profile's, never the
			// writable cache, whose emptyDir would hide it (SPEC Q23).
			if !filepath.IsAbs(e.Value) || within(filepath.Clean(e.Value), s.cache) {
				return errors.New("GOMODCACHE is not an absolute directory outside the cache")
			}
		}
	}
	return nil
}

// lookIn is exec.LookPath over path, a PATH value, instead of the shim's
// own: a name with a slash is used as it is, any other is the first
// executable regular file of that name in path's absolute directories.
func lookIn(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// envValue is name's value in env ("NAME=value" entries), the last one
// winning as it does for a program.
func envValue(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == name {
			v = val
		}
	}
	return v
}

// within reports p is dir or under it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// workspaceEnv is a request's program environment: the request's
// variables, the defaults for what it leaves out (the image's usual PATH;
// no module network; a build may complete go.mod and go.sum and warm the
// cache, any other program reads them only), then the shim's own places.
func (s *Server) workspaceEnv(r *Request) []string {
	env := make([]string, 0, len(r.Env)+len(ownedEnv)+3)
	has := map[string]bool{}
	for _, e := range r.Env {
		env = append(env, e.Name+"="+e.Value)
		has[e.Name] = true
	}
	goflags := "-mod=readonly"
	if r.build() {
		goflags = "-mod=mod"
	}
	for _, d := range []EnvVar{{"PATH", DefaultPath}, {"GOPROXY", "off"}, {"GOFLAGS", goflags}} {
		if !has[d.Name] {
			env = append(env, d.Name+"="+d.Value)
		}
	}
	return append(env,
		"HOME="+s.home,
		"TMPDIR="+TmpDir,
		"GOCACHE="+filepath.Join(s.cache, "go-build"),
		"GOTOOLCHAIN=local",
		"NPM_CONFIG_CACHE="+filepath.Join(s.cache, "npm"),
	)
}

// newWorkspaceServer is NewServer for a workspace.
func newWorkspaceServer(opts Options) (*Server, error) {
	if opts.Scratch != "" || len(opts.Env) > 0 {
		return nil, errors.New("toolexec: a workspace takes neither a scratch directory nor an environment")
	}
	for _, p := range []string{opts.Work, opts.Home, opts.Cache} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("toolexec: workspace directory %q is not absolute", p)
		}
	}
	if os.Getuid() == 0 {
		return nil, errors.New("toolexec: a workspace shim never runs as root")
	}
	work := filepath.Clean(opts.Work)
	s := &Server{
		scratch: filepath.Join(work, "scratch"),
		timeout: Timeout,
		slot:    make(chan struct{}, 1),
		work:    work,
		home:    filepath.Clean(opts.Home),
		cache:   filepath.Clean(opts.Cache),
	}
	for _, d := range []string{s.scratch, s.home, s.cache} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("toolexec: create %s: %w", d, err)
		}
	}
	proc := opts.Procfs
	if proc == "" {
		proc = Procfs
	}
	s.sweep = sweeper{proc: proc, uid: os.Getuid(), self: os.Getpid(), kill: killPID, reap: reapOrphans}
	return s, nil
}

// Do runs one request. A slot shim takes only the wire's exec (Run); a
// workspace shim takes a build or an exec of a run, and before it answers
// kills every process the program left (the sweep). When some survive,
// the Result says so and the shim runs nothing more until a sweep finds
// none.
func (s *Server) Do(ctx context.Context, req *Request) Result {
	if err := req.Check(); err != nil {
		return refused(toolwire.RefusedInvalid, err.Error())
	}
	if s.work == "" {
		if req.workspace() {
			return refused(toolwire.RefusedInvalid, "this shim takes only an exec")
		}
		return s.Run(ctx, &toolwire.ExecRequest{Argv: req.Argv, Files: req.Files, Side: req.Side})
	}
	if err := s.checkWorkspace(req); err != nil {
		return refused(toolwire.RefusedInvalid, err.Error())
	}
	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return refused(toolwire.RefusedUnavailable, "another program is still running")
	}
	defer func() { <-s.slot }()
	if s.unclean {
		if err := s.sweep.run(); err != nil {
			return refused(toolwire.RefusedUnavailable, "processes of an earlier program survive")
		}
		s.unclean = false
	}
	if req.Fresh {
		if err := s.restore(); err != nil {
			return refused(toolwire.RefusedUnavailable, "the tree and build output could not be restored as the "+
				"build left them: "+err.Error())
		}
	}
	var res Result
	if req.build() {
		out := &tailWriter{limit: MaxBuildOutputBytes}
		s.built = ""
		res = s.execute(ctx, req.Argv, filepath.Join(s.work, "tree"), s.workspaceEnv(req), req.buildTimeout(), out)
	} else {
		dir, err := s.runScratch(req.Run, req.Files)
		if err != nil {
			return refused(toolwire.RefusedInvalid, err.Error())
		}
		res = s.execute(ctx, req.Argv, dir, s.workspaceEnv(req), s.timeout, &capWriter{limit: MaxOutputBytes})
	}
	if err := s.sweep.run(); err != nil {
		s.unclean = true
		res.Unclean = true
	}
	if req.build() && res.Exit == 0 && !res.TimedOut && !res.Unclean {
		if err := s.keep(); err != nil {
			res.Output += "\nmaul: the build's tree and output could not be kept, so no check can run: " + err.Error()
		}
	}
	res.NetError = NetError(res.Output)
	return res
}

// runScratch creates run's scratch directory under the scratch directory
// and writes files there, all through an os.Root, so no name or symlink
// already there (a program of the sandbox's UID may have left one) places
// anything outside it.
func (s *Server) runScratch(run string, files []toolwire.File) (string, error) {
	root, err := os.OpenRoot(s.scratch)
	if err != nil {
		return "", fmt.Errorf("scratch directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(run, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("run %q has no scratch directory", run)
	}
	sub, err := root.OpenRoot(run)
	if err != nil {
		return "", fmt.Errorf("run %q has no scratch directory", run)
	}
	defer func() { _ = sub.Close() }()
	if fi, err := root.Lstat(run); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("run %q has no scratch directory", run)
	}
	if err := writeFilesIn(sub, files); err != nil {
		return "", err
	}
	return filepath.Join(s.scratch, run), nil
}

// netErrors are what a program's output says when it could not bind or
// connect: a port a leftover held, or nothing listening (SPEC 8a).
var netErrors = []string{
	"address already in use", "eaddrinuse",
	"connection refused", "econnrefused",
	"cannot assign requested address", "eaddrnotavail",
	"connection reset by peer", "econnreset",
	"network is unreachable", "enetunreach",
	"no route to host", "ehostunreach",
}

// NetError reports output that names a bind or connection error. The relay
// counts such a run an error (n/a), never an assertion fail: on a pod
// whose loopback both sides share, a port another program held is not
// what the check asserts.
func NetError(output string) bool {
	lower := strings.ToLower(output)
	for _, e := range netErrors {
		if strings.Contains(lower, e) {
			return true
		}
	}
	return false
}

// tailWriter keeps the last limit bytes written to it and notes that more
// came; it never refuses a write, so a program is never blocked on it.
type tailWriter struct {
	mu       sync.Mutex
	limit    int
	buf      []byte
	overflow bool
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > 2*w.limit {
		w.overflow = true
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-w.limit:]...)
	}
	return len(p), nil
}

// output is the build's output: its tail, bounded, valid UTF-8, marked at
// the front when cut and at the end when the build was killed.
func (w *tailWriter) output(timedOut bool) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	raw := w.buf
	overflow := w.overflow
	if len(raw) > w.limit {
		overflow = true
		raw = raw[len(raw)-w.limit:]
	}
	if overflow {
		for len(raw) > 0 && !utf8.RuneStart(raw[0]) {
			raw = raw[1:]
		}
	}
	suffix := ""
	if timedOut {
		suffix = TimeoutMarker
	}
	room := w.limit - len(suffix)
	v := strings.ToValidUTF8(string(raw), string(utf8.RuneError))
	truncated := overflow || len(v) > room
	if truncated {
		start := max(0, len(v)-(room-len(TailMarker)))
		for start < len(v) && !utf8.RuneStart(v[start]) {
			start++
		}
		v = TailMarker + v[start:]
	}
	return v + suffix, truncated
}
