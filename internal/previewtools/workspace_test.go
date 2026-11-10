// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

const wsKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeSandbox is one side's exec shim: it records every request and
// answers with do (builds pass and programs exit 0 printing nothing when
// do is nil).
type fakeSandbox struct {
	mu       sync.Mutex
	reqs     []toolexec.Request
	notReady bool
	do       func(req *toolexec.Request, n int) (toolexec.Result, error)
}

func (f *fakeSandbox) Do(_ context.Context, req *toolexec.Request) (toolexec.Result, error) {
	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, *req)
	f.mu.Unlock()
	if f.do == nil {
		return toolexec.Result{}, nil
	}
	return f.do(req, n)
}

func (f *fakeSandbox) Ready(context.Context) error {
	if f.notReady {
		return errors.New("no shim")
	}
	return nil
}

func (f *fakeSandbox) requests() []toolexec.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// nonBuilds are the requests that were not builds.
func (f *fakeSandbox) nonBuilds() []toolexec.Request {
	var out []toolexec.Request
	for _, r := range f.requests() {
		if r.Kind != toolexec.KindBuild {
			out = append(out, r)
		}
	}
	return out
}

// buildsFail makes a sandbox's build exit 2 and everything else pass.
func buildsFail(req *toolexec.Request, _ int) (toolexec.Result, error) {
	if req.Kind == toolexec.KindBuild {
		return toolexec.Result{Exit: 2, Output: "compile error"}, nil
	}
	return toolexec.Result{}, nil
}

// printing answers every program with output and exit, builds passing.
func printing(output string, exit int) func(*toolexec.Request, int) (toolexec.Result, error) {
	return func(req *toolexec.Request, _ int) (toolexec.Result, error) {
		if req.Kind == toolexec.KindBuild {
			return toolexec.Result{}, nil
		}
		return toolexec.Result{Output: output, Exit: exit}, nil
	}
}

// writeTree writes a pristine tree with files.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const treeGoMod = "module example.com/bb\n\ngo 1.24\n\nrequire golang.org/x/mod v0.41.0\n"

// newWorkspaceHarness is a workspace relay over head and base, its trees
// under a temp directory, built (entries 1 and 2).
func newWorkspaceHarness(t *testing.T, cfg Config, head, base *fakeSandbox) *harness {
	t.Helper()
	trees := t.TempDir()
	for _, side := range []string{"head", "base"} {
		writeTree(t, filepath.Join(trees, side), map[string]string{"go.mod": treeGoMod,
			"cmd/bb/main.go": "package main\n\nfunc main() {} // " + side + "\n"})
	}
	h := newHarness(t, cfg, routes{}, func(o *Options) {
		o.Head, o.Base, o.Trees, o.Key = head, base, trees, wsKey
	})
	h.relay.buildTrees(context.Background())
	return h
}

// callKeyed posts body to verb's endpoint with the key.
func (h *harness) callKeyed(verb toolwire.Verb, body string) (int, toolwire.Response) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, toolwire.Path(verb), strings.NewReader(body))
	req.RemoteAddr = "10.9.9.9:41000"
	req.Header.Set(toolwire.KeyHeader, wsKey)
	rec := httptest.NewRecorder()
	h.relay.Handler().ServeHTTP(rec, req)
	var resp toolwire.Response
	if err := toolwire.DecodeResponse(rec.Body, &resp); err != nil {
		h.t.Fatalf("%s: decode %q: %v", verb, rec.Body.String(), err)
	}
	return rec.Code, resp
}

// dryRunKeyed is `maul-tool check run` of src at side, with the key.
func (h *harness) dryRunKeyed(src string, side toolwire.Side) (int, toolwire.Response) {
	h.t.Helper()
	return h.callKeyed(toolwire.VerbCheckRun, mustJSON(h.t, toolwire.CheckRunRequest{Spec: json.RawMessage(src),
		Side: side}))
}

func (h *harness) finishKeyed() toolwire.Ledger {
	h.t.Helper()
	code, resp := h.callKeyed(toolwire.VerbFinish, `{}`)
	if code != http.StatusOK || resp.Refused != "" {
		h.t.Fatalf("finish: %d %+v", code, resp)
	}
	return h.verify()
}

// cliExecCheck is a cli exec check: the binary prints the count.
const cliExecCheck = `{"id":"count.prints","outcome":"count","form":"exec","exec":{
 "argv":["{{bin}}","--count","{{tree}}/x"],"expect":{"exit":0,"stdoutContains":"count=3"}}}`

// cliRunCheck is a cli run check: a program that runs the binary.
const cliRunCheck = `{"id":"count.run","outcome":"count","form":"run","run":{"language":"go","files":[
 {"path":"main.go","content":"package main\n// runs {{bin}} on {{tree}}; {{marker}}\nfunc main() {}\n"}],
 "args":["{{run}}"],"expect":{"exit":0,"stdoutContains":"ok"}}}`

func storedExecCheck(t *testing.T) StoredCheck {
	t.Helper()
	c, err := checks.Parse([]byte(cliExecCheck))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := checks.Validate(c, checks.Context{Workspace: &checks.Workspace{Binary: "/work/bin/bb"}}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return StoredCheck{ID: c.ID, Digest: checks.Digest(c), Body: string(checks.Canonical(c))}
}

// buildEntries are a ledger's build entries' args, by side.
func buildEntries(t *testing.T, l toolwire.Ledger) map[toolwire.Side]BuildArgs {
	t.Helper()
	out := map[toolwire.Side]BuildArgs{}
	for _, e := range l.Entries {
		if e.Verb != toolwire.VerbBuild {
			continue
		}
		var a BuildArgs
		if err := json.Unmarshal(e.Args, &a); err != nil {
			t.Fatalf("build args: %v", err)
		}
		out[e.Side] = a
	}
	return out
}

// TestWorkspaceBuilds: entries 1 and 2 are the head and base builds, each
// with its result, its argv, its timeout and its output's tail; the
// requests carry the search path and the profile's environment, filtered to
// what the shim admits.
func TestWorkspaceBuilds(t *testing.T) {
	cfg := wsConfig()
	cfg.Workspace.Env = []toolexec.EnvVar{{Name: "GOPROXY", Value: "http://proxy:3128/go"},
		{Name: "GOTOOLCHAIN", Value: "local"}, {Name: "NPM_CONFIG_CACHE", Value: "/tmp/npm-cache"},
		{Name: "MAUL_NETWORK_PROFILE", Value: "go"}, {Name: "HOME", Value: "/work/home"},
		{Name: "PATH", Value: "/evil"}, {Name: "NPM_CONFIG_REGISTRY", Value: "http://proxy:3128/r/npm/"}}
	head := &fakeSandbox{do: func(*toolexec.Request, int) (toolexec.Result, error) {
		return toolexec.Result{Output: "built head", DurationMs: 1200}, nil
	}}
	base := &fakeSandbox{do: buildsFail}
	h := newWorkspaceHarness(t, cfg, head, base)
	l := h.finishKeyed()
	if l.Entries[0].Verb != toolwire.VerbBuild || l.Entries[0].Side != "" ||
		l.Entries[1].Verb != toolwire.VerbBuild || l.Entries[1].Side != toolwire.SideBase {
		t.Fatalf("entries 1-2 are not the head and base builds: %+v", l.Entries[:2])
	}
	if l.Entries[0].Output != "built head" || l.Entries[1].Exit != 2 || l.Entries[0].Kind != toolwire.KindRelay {
		t.Fatalf("build entries: %+v", l.Entries[:2])
	}
	b := buildEntries(t, l)
	if b[""].Result != maulv1.BuildBuilt || b[toolwire.SideBase].Result != maulv1.BuildFailed {
		t.Fatalf("build results: %+v", b)
	}
	if !slices.Equal(b[""].Argv, cfg.Workspace.Build) || b[""].TimeoutMs != 600_000 {
		t.Fatalf("head build args: %+v", b[""])
	}
	req := head.requests()[0]
	if req.Kind != toolexec.KindBuild || req.TimeoutMs != 600_000 || !slices.Equal(req.Argv, cfg.Workspace.Build) {
		t.Fatalf("head build request: %+v", req)
	}
	want := []toolexec.EnvVar{{Name: "PATH", Value: cfg.Workspace.SearchPath},
		{Name: "GOPROXY", Value: "http://proxy:3128/go"}, {Name: "NPM_CONFIG_REGISTRY", Value: "http://proxy:3128/r/npm/"}}
	if !slices.Equal(req.Env, want) {
		t.Fatalf("build env = %+v, want %+v", req.Env, want)
	}
	for _, e := range req.Env {
		if !toolexec.AllowedEnv(e.Name) {
			t.Errorf("the build passes %s, which the shim refuses", e.Name)
		}
	}
}

// TestLibraryBuild: a library's build is go build ./... in the tree.
func TestLibraryBuild(t *testing.T) {
	cfg := wsConfig()
	cfg.Workspace.Build, cfg.Workspace.Binary, cfg.Workspace.Language = nil, "", "go"
	head, base := &fakeSandbox{}, &fakeSandbox{}
	newWorkspaceHarness(t, cfg, head, base)
	for _, sb := range []*fakeSandbox{head, base} {
		if got := sb.requests()[0].Argv; !slices.Equal(got, []string{"go", "build", "./..."}) {
			t.Fatalf("library build argv = %q", got)
		}
	}
}

// TestBuildResults: each side's build result. A build failed only when it
// ran to its own end and refused the tree (a normal exit of 1 to 125,
// clean, its output whole, no environmental marker); anything else is
// maul's error, never failed, since a failed base is unbuildable and that
// waives rule 5 (review VC-1).
func TestBuildResults(t *testing.T) {
	type row struct {
		name     string
		notReady bool
		res      toolexec.Result
		err      error
		want     maulv1.BuildResult
	}
	tests := []row{
		{"built", false, toolexec.Result{}, nil, maulv1.BuildBuilt},
		{"failed", false, toolexec.Result{Exit: 1}, nil, maulv1.BuildFailed},
		{"failed with diagnostics", false, toolexec.Result{Exit: 2, Output: "x.go:1:1: undefined: y\n"}, nil,
			maulv1.BuildFailed},
		{"timed out", false, toolexec.Result{Exit: -1, TimedOut: true}, nil, maulv1.BuildTimeout},
		{"no shim", true, toolexec.Result{}, nil, maulv1.BuildError},
		{"no answer", false, toolexec.Result{}, errors.New("eof"), maulv1.BuildError},
		{"refused", false, toolexec.Result{Refused: toolwire.RefusedInvalid}, nil, maulv1.BuildError},
		{"left processes", false, toolexec.Result{Unclean: true}, nil, maulv1.BuildError},
		{"left processes after failing", false, toolexec.Result{Exit: 2, Unclean: true}, nil, maulv1.BuildError},
		{"killed by a signal", false, toolexec.Result{Exit: -1}, nil, maulv1.BuildError},
		// A shell reports what ended its child by exit code alone: 126 and
		// 127 a command it could not run, 128+N signal N.
		{"a shell's child segfaulted", false, toolexec.Result{Exit: 139,
			Output: "sh: line 1:    12 Segmentation fault      go build ./...\n"}, nil, maulv1.BuildError},
		{"a shell's command not found", false, toolexec.Result{Exit: 127, Output: "sh: 1: make: not found\n"}, nil,
			maulv1.BuildError},
		{"a shell's command not executable", false, toolexec.Result{Exit: 126}, nil, maulv1.BuildError},
		{"failed at the highest plain exit", false, toolexec.Result{Exit: 125}, nil, maulv1.BuildFailed},
		{"output cut", false, toolexec.Result{Exit: 1, Truncated: true, Output: toolexec.TailMarker + "x.go:1:1: no"},
			nil, maulv1.BuildError},
		{"could not start", false, toolexec.Result{Exit: 127,
			Output: `[maul: could not start make: exec: "make": executable file not found in $PATH]`}, nil,
			maulv1.BuildError},
	}
	markers := make([]row, 0, len(environmental))
	for _, m := range environmental {
		markers = append(markers, row{"marker " + m, false,
			toolexec.Result{Exit: 1, Output: "x.go:1:1: " + strings.ToUpper(m) + " here\n"}, nil, maulv1.BuildError})
	}
	for _, tt := range append(tests, markers...) {
		t.Run(tt.name, func(t *testing.T) {
			do := func(*toolexec.Request, int) (toolexec.Result, error) { return tt.res, tt.err }
			head := &fakeSandbox{notReady: tt.notReady, do: do}
			base := &fakeSandbox{notReady: tt.notReady, do: do}
			h := newHarness(t, wsConfig(), routes{}, func(o *Options) {
				o.Head, o.Base, o.Trees, o.Key = head, base, t.TempDir(), wsKey
				o.ReadyWait = 50 * time.Millisecond
			})
			h.relay.buildTrees(context.Background())
			b := buildEntries(t, h.finishKeyed())
			if b[""].Result != tt.want || b[toolwire.SideBase].Result != tt.want {
				t.Fatalf("results %+v, want %s on both sides", b, tt.want)
			}
		})
	}
}

// TestBaseBuildErrorGivesNoBaseEvidence: a base whose build maul cannot
// tell failed (killed, cut short, an environmental failure, a timeout) is
// not unbuildable: its runs are errors with no cause, which give no base
// evidence (unsure), never the n/a that waives rule 5 (review VC-1).
func TestBaseBuildErrorGivesNoBaseEvidence(t *testing.T) {
	for name, res := range map[string]toolexec.Result{
		"killed":  {Exit: -1, Output: "signal: killed"},
		"network": {Exit: 1, Output: "go: example.com/m@v1.0.0: dial tcp: i/o timeout"},
		"cut":     {Exit: 2, Truncated: true},
		"timeout": {Exit: -1, TimedOut: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := wsConfig()
			cfg.Checks = []StoredCheck{storedExecCheck(t)}
			head := &fakeSandbox{do: printing("count=3", 0)}
			base := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				if req.Kind == toolexec.KindBuild {
					return res, nil
				}
				return toolexec.Result{}, nil
			}}
			h := newWorkspaceHarness(t, cfg, head, base)
			h.relay.replay(context.Background())
			_, resp := h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}"],"side":"base"}`)
			if resp.Refused != toolwire.RefusedUnavailable || strings.HasPrefix(resp.Reason, ReasonBaseUnbuildable) {
				t.Errorf("a base exec on a base that did not build: %+v", resp)
			}
			if _, resp = h.dryRunKeyed(cliExecCheck, ""); resp.Refused != "" {
				t.Fatalf("head dry run: %+v", resp)
			}
			_, resp = h.callKeyed(toolwire.VerbConfirm,
				`{"digests":["`+strings.TrimPrefix(cfg.Checks[0].Digest, "sha256:")+`"]}`)
			if resp.Refused != "" {
				t.Fatalf("confirm: %+v", resp)
			}
			var baseRuns int
			for _, e := range h.finishKeyed().Entries {
				if e.CheckRun == nil || sideOf(e.CheckRun) != toolwire.SideBase {
					continue
				}
				baseRuns++
				if e.CheckRun.Result != toolwire.RunError || e.CheckRun.Cause() != "" {
					t.Errorf("entry %d: base run %s cause %q, want an error with no cause", e.Seq,
						e.CheckRun.Result, e.CheckRun.Cause())
				}
			}
			if baseRuns != 2 {
				t.Errorf("%d base runs, want the replay's and the confirmation's", baseRuns)
			}
			if len(base.nonBuilds()) != 0 {
				t.Errorf("the base that did not build ran %+v", base.nonBuilds())
			}
		})
	}
}

// TestBuildPart: the head build part meets acceptance's RoleBuild
// contract: only a build that built is OK; anything else, an unrun build
// included, is the tool pod's Infra.
func TestBuildPart(t *testing.T) {
	for _, tt := range []struct {
		args BuildArgs
		exit int
		ok   bool
	}{
		{BuildArgs{Result: maulv1.BuildBuilt}, 0, true},
		{BuildArgs{Result: maulv1.BuildFailed}, 2, false},
		{BuildArgs{Result: maulv1.BuildTimeout}, -1, false},
		{BuildArgs{Result: maulv1.BuildError}, 0, false},
		{BuildArgs{Result: "bogus"}, 0, false},
	} {
		p := BuildPart(tt.args, tt.exit)
		if p.Role != acceptance.RoleBuild {
			t.Fatalf("part role %q", p.Role)
		}
		if got := acceptance.ClassifyEnvironment([]acceptance.PartFact{p}) == acceptance.EnvOK; got != tt.ok {
			t.Errorf("%s: environment ok %v, want %v", tt.args.Result, got, tt.ok)
		}
	}
}

// TestHeadBuildFailedRefusesRuns: a head that did not build runs nothing:
// no replay, and every exec, run, check run and confirm is refused
// build-failed; files still reads the pristine tree.
func TestHeadBuildFailedRefusesRuns(t *testing.T) {
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	head, base := &fakeSandbox{do: buildsFail}, &fakeSandbox{}
	h := newWorkspaceHarness(t, cfg, head, base)
	h.relay.replay(context.Background())
	run := mustJSON(t, toolwire.RunRequest{Language: "go", Files: []toolwire.File{{Name: "main.go",
		Content: []byte("package main\nfunc main(){}\n")}}})
	for _, c := range []struct {
		verb toolwire.Verb
		body string
	}{
		{toolwire.VerbExec, `{"argv":["{{bin}}"]}`},
		{toolwire.VerbExec, `{"argv":["ls"],"side":"base"}`},
		{toolwire.VerbRun, run},
		{toolwire.VerbCheckRun, mustJSON(t, map[string]json.RawMessage{"check": json.RawMessage(cliExecCheck)})},
		{toolwire.VerbConfirm, `{"digests":["` + strings.TrimPrefix(cfg.Checks[0].Digest, "sha256:") + `"]}`},
	} {
		_, resp := h.callKeyed(c.verb, c.body)
		if resp.Refused != toolwire.RefusedUnavailable || !strings.HasPrefix(resp.Reason, ReasonBuildFailed) {
			t.Errorf("%s after a failed head build: %+v", c.verb, resp)
		}
	}
	_, resp := h.callKeyed(toolwire.VerbFiles, `{"op":"read","path":"go.mod"}`)
	if resp.Refused != "" || resp.Untrusted != treeGoMod {
		t.Errorf("files after a failed head build: %+v", resp)
	}
	l := h.finishKeyed()
	for _, e := range l.Entries {
		if e.CheckRun != nil {
			t.Errorf("entry %d ran a check after a failed head build", e.Seq)
		}
	}
	if len(head.nonBuilds()) != 0 || len(base.nonBuilds()) != 0 {
		t.Errorf("a sandbox ran a program after a failed head build: %+v %+v", head.nonBuilds(), base.nonBuilds())
	}
}

// sideOf is the side a check run's steps name: base only when every step
// does.
func sideOf(rec *toolwire.CheckRunRecord) toolwire.Side {
	side := toolwire.Side("unset")
	for _, a := range rec.Attempts {
		for _, s := range a.Steps {
			if side != "unset" && s.Side != side {
				return "mixed"
			}
			side = s.Side
		}
	}
	return side
}

// TestWorkspaceReplayBothSides: each stored check replays alone at the
// head, then at the base, each with its own fresh run token, the base
// run's entry and steps naming the base.
func TestWorkspaceReplayBothSides(t *testing.T) {
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	head := &fakeSandbox{do: printing("count=3", 0)}
	base := &fakeSandbox{do: printing("count=2", 0)}
	h := newWorkspaceHarness(t, cfg, head, base)
	h.relay.replay(context.Background())
	l := h.finishKeyed()
	var runs []toolwire.LedgerEntry
	for _, e := range l.Entries {
		if e.CheckRun != nil {
			runs = append(runs, e)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("%d replay entries, want head and base", len(runs))
	}
	if runs[0].Side != "" || sideOf(runs[0].CheckRun) != "" || runs[0].CheckRun.Result != toolwire.RunPass {
		t.Errorf("head replay: %+v", runs[0])
	}
	b := runs[1]
	if b.Side != toolwire.SideBase || sideOf(b.CheckRun) != toolwire.SideBase || b.CheckRun.Result != toolwire.RunFail {
		t.Errorf("base replay: %+v", b)
	}
	if step, ok := b.CheckRun.AssertionStep(); !ok || step != 0 || b.CheckRun.FailedAssertionID() != "stdout" {
		t.Errorf("a stdout miss after the expected exit is not a stdout assertion: %+v", b.CheckRun)
	}
	hr, br := head.nonBuilds(), base.nonBuilds()
	if len(hr) != 1 || len(br) != 2 {
		t.Fatalf("head ran %d, base %d programs", len(hr), len(br))
	}
	tokens := map[string]bool{hr[0].Run: true, br[0].Run: true, br[1].Run: true}
	if len(tokens) != 3 || hr[0].Run == "" {
		t.Errorf("runs share a token: %q %q %q", hr[0].Run, br[0].Run, br[1].Run)
	}
	if !slices.Equal(hr[0].Argv, []string{"/work/bin/bb", "--count", "/work/tree/x"}) {
		t.Errorf("head argv %q", hr[0].Argv)
	}
}

// TestBaseRunsBounded: the relay makes at most MaxBaseRuns base runs for
// replays and confirmations, what status.base.runs keeps; past it a check
// gets none (Decide: rule 5 not met). The agent's base dry runs spend none.
func TestBaseRunsBounded(t *testing.T) {
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	base := &fakeSandbox{do: printing("count=2", 0)}
	h := newWorkspaceHarness(t, cfg, &fakeSandbox{do: printing("count=3", 0)}, base)
	h.relay.baseRuns = MaxBaseRuns - 1
	for range 3 {
		h.dryRunKeyed(cliExecCheck, toolwire.SideBase)
	}
	h.relay.replay(context.Background())
	_, resp := h.dryRunKeyed(cliExecCheck, "")
	_, resp = h.callKeyed(toolwire.VerbConfirm, `{"digests":["`+resp.Observed.CheckRun.Digest+`"]}`)
	if resp.Refused != "" {
		t.Fatalf("confirm: %+v", resp)
	}
	l := h.finishKeyed()
	byMode := map[toolwire.CheckRunMode]int{}
	for _, e := range l.Entries {
		if e.CheckRun != nil && sideOf(e.CheckRun) == toolwire.SideBase {
			byMode[e.CheckRun.Mode]++
		}
	}
	if byMode[toolwire.ModeDryRun] != 3 || byMode[toolwire.ModeReplay] != 1 || byMode[toolwire.ModeConfirm] != 0 {
		t.Fatalf("base runs by mode %v, want 3 dry runs, the last replay the bound allows and no confirmation", byMode)
	}
}

// TestBaseUnbuildable: a base that did not build is never run: its
// replays and confirmations are recorded as errors (n/a), and the agent's
// base calls are refused; the head runs as ever.
func TestBaseUnbuildable(t *testing.T) {
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	head := &fakeSandbox{do: printing("count=3", 0)}
	base := &fakeSandbox{do: buildsFail}
	h := newWorkspaceHarness(t, cfg, head, base)
	h.relay.replay(context.Background())
	for _, body := range []string{`{"argv":["{{bin}}"],"side":"base"}`} {
		_, resp := h.callKeyed(toolwire.VerbExec, body)
		if resp.Refused != toolwire.RefusedUnavailable || !strings.HasPrefix(resp.Reason, ReasonBaseUnbuildable) {
			t.Errorf("a base exec on an unbuildable base: %+v", resp)
		}
	}
	_, resp := h.dryRunKeyed(cliExecCheck, "")
	if resp.Refused != "" || resp.Observed.CheckRun.Result != toolwire.RunPass {
		t.Fatalf("head dry run: %+v", resp)
	}
	_, resp = h.dryRunKeyed(cliExecCheck, toolwire.SideBase)
	if resp.Refused != toolwire.RefusedUnavailable {
		t.Fatalf("base dry run on an unbuildable base: %+v", resp)
	}
	_, resp = h.callKeyed(toolwire.VerbConfirm, `{"digests":["`+strings.TrimPrefix(cfg.Checks[0].Digest, "sha256:")+`"]}`)
	if resp.Refused != "" {
		t.Fatalf("confirm: %+v", resp)
	}
	l := h.finishKeyed()
	var baseRuns int
	for _, e := range l.Entries {
		if e.CheckRun == nil || sideOf(e.CheckRun) != toolwire.SideBase {
			continue
		}
		baseRuns++
		if e.CheckRun.Result != toolwire.RunError {
			t.Errorf("entry %d: a base run on an unbuildable base is %s, not an error", e.Seq, e.CheckRun.Result)
		}
	}
	if baseRuns != 2 {
		t.Errorf("%d base runs, want the replay's and the confirmation's", baseRuns)
	}
	if len(base.nonBuilds()) != 0 {
		t.Errorf("the unbuildable base ran %+v", base.nonBuilds())
	}
}

// TestWorkspaceConfirmBothSides: confirm runs every proposed check at the
// head, then at the base, the base entry's args and steps naming the base;
// the response carries the head runs, as in the slot.
func TestWorkspaceConfirmBothSides(t *testing.T) {
	head := &fakeSandbox{do: printing("count=3", 0)}
	base := &fakeSandbox{do: printing("usage: bb", 2)}
	h := newWorkspaceHarness(t, wsConfig(), head, base)
	_, resp := h.dryRunKeyed(cliExecCheck, "")
	if resp.Refused != "" {
		t.Fatalf("dry run: %+v", resp)
	}
	d := resp.Observed.CheckRun.Digest
	_, resp = h.callKeyed(toolwire.VerbConfirm, `{"digests":["`+d+`"]}`)
	if resp.Refused != "" || len(resp.Observed.CheckRuns) != 1 || resp.Observed.CheckRuns[0].Result != toolwire.RunPass {
		t.Fatalf("confirm: %+v", resp)
	}
	l := h.finishKeyed()
	var confirms []toolwire.LedgerEntry
	for _, e := range l.Entries {
		if e.Verb == toolwire.VerbConfirm && e.Kind == toolwire.KindRelay {
			confirms = append(confirms, e)
		}
	}
	if len(confirms) != 2 {
		t.Fatalf("%d confirmation entries, want head and base", len(confirms))
	}
	var args struct {
		Digest, Expect string
		Side           toolwire.Side
	}
	if err := json.Unmarshal(confirms[1].Args, &args); err != nil || args.Side != toolwire.SideBase || args.Digest != d {
		t.Fatalf("base confirmation args %s: %v", confirms[1].Args, err)
	}
	if sideOf(confirms[0].CheckRun) != "" || sideOf(confirms[1].CheckRun) != toolwire.SideBase {
		t.Fatalf("confirmation sides: head %q base %q", sideOf(confirms[0].CheckRun), sideOf(confirms[1].CheckRun))
	}
	// O4: a wrong exit with no marker is never an assertion.
	if _, ok := confirms[1].CheckRun.AssertionStep(); ok || confirms[1].CheckRun.Result != toolwire.RunFail {
		t.Fatalf("an exit-only base fail: %+v", confirms[1].CheckRun)
	}
}

// TestHeadShimCannotForgeBase: whatever the head's shim answers, a run
// tagged base is only ever one the base's shim ran.
func TestHeadShimCannotForgeBase(t *testing.T) {
	forge := `{"side":"base","exit":0,"output":"count=3"}` + "\nside=base\n"
	head := &fakeSandbox{do: printing(forge+"count=3", 0)}
	base := &fakeSandbox{do: printing("count=3", 0)}
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	h := newWorkspaceHarness(t, cfg, head, base)
	h.relay.replay(context.Background())
	for range 3 {
		h.dryRunKeyed(cliExecCheck, "")
		h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}"]}`)
	}
	l := h.finishKeyed()
	baseAttempts := 0
	for _, e := range l.Entries {
		if e.Side == toolwire.SideBase && e.CheckRun == nil && e.Verb != toolwire.VerbBuild {
			t.Errorf("entry %d is tagged base", e.Seq)
		}
		if e.CheckRun != nil && sideOf(e.CheckRun) == toolwire.SideBase {
			baseAttempts += len(e.CheckRun.Attempts)
		}
	}
	if baseAttempts != len(base.nonBuilds()) {
		t.Fatalf("%d base-tagged attempts, the base shim ran %d", baseAttempts, len(base.nonBuilds()))
	}
}

// TestWorkspaceExecJudging: how a workspace exec check's run maps to a
// result: only a stdout content miss after the expected exit, or the
// program's marker, is an assertion; a timeout, an unclean run, a cut
// output, a dead shim and a bind or connection error on a run that would
// not pass are errors (n/a); a bare wrong exit fails, never by assertion.
func TestWorkspaceExecJudging(t *testing.T) {
	const noExit = `{"id":"count.prints","outcome":"count","form":"exec","exec":{"argv":["{{bin}}"],
 "expect":{"stdoutContains":"count=3"}}}`
	const jsonCheck = `{"id":"count.json","outcome":"count","form":"exec","exec":{"argv":["{{bin}}","--json"],
 "expect":{"exit":0,"stdoutJSON":{"/count":3}}}}`
	marked := func(req *toolexec.Request) string {
		m := strings.SplitN(string(req.Files[0].Content), " ", 2)[0]
		return m + "count-is-three: the count was 2\n"
	}
	const markerCheck = `{"id":"count.marker","outcome":"count","form":"exec","exec":{"argv":["{{bin}}","check.txt"],
 "files":[{"path":"check.txt","content":"{{marker}} rest"}],"expect":{"exit":0,"stdoutContains":"count=3"}}}`
	tests := []struct {
		name   string
		check  string
		res    func(req *toolexec.Request) (toolexec.Result, error)
		result toolwire.AttemptResult
		id     string // the assertion id, "" for none
	}{
		{"pass", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "count=3"}, nil
		}, toolwire.ResultPass, ""},
		{"stdout miss after the expected exit", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "count=2"}, nil
		}, toolwire.ResultFail, "stdout"},
		{"stdout miss with no exit expected", noExit, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "", Exit: 2}, nil
		}, toolwire.ResultFail, ""},
		{"a bare wrong exit", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "count=3", Exit: 1}, nil
		}, toolwire.ResultFail, ""},
		{"json miss after the expected exit", jsonCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: `{"count":2}`}, nil
		}, toolwire.ResultFail, "stdout"},
		{"not JSON", jsonCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: `panic: boom`}, nil
		}, toolwire.ResultFail, ""},
		{"the marker", markerCheck, func(req *toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: marked(req), Exit: 1}, nil
		}, toolwire.ResultFail, "marker count-is-three"},
		{"the marker beside a connection error", markerCheck, func(req *toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: marked(req), Exit: 1, NetError: true}, nil
		}, toolwire.ResultError, ""},
		{"a timeout", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Exit: -1, TimedOut: true}, nil
		}, toolwire.ResultError, ""},
		{"unclean", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "count=3", Unclean: true}, nil
		}, toolwire.ResultError, ""},
		{"truncated", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "count=2", Truncated: true}, nil
		}, toolwire.ResultError, ""},
		{"no answer", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{}, errors.New("eof")
		}, toolwire.ResultError, ""},
		{"refused", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Refused: toolwire.RefusedInvalid}, nil
		}, toolwire.ResultError, ""},
		{"a connection error on a fail", cliExecCheck, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "dial: connection refused", NetError: true}, nil
		}, toolwire.ResultError, ""},
		{"a connection error on a pass", `{"id":"count.refused","outcome":"count","form":"exec","exec":{"argv":["{{bin}}"],
 "expect":{"exit":1,"stdoutContains":"connection refused"}}}`, func(*toolexec.Request) (toolexec.Result, error) {
			return toolexec.Result{Output: "dial: connection refused", Exit: 1, NetError: true}, nil
		}, toolwire.ResultPass, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				if req.Kind == toolexec.KindBuild {
					return toolexec.Result{}, nil
				}
				return tt.res(req)
			}}
			h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
			_, resp := h.callKeyed(toolwire.VerbCheckRun, mustJSON(t, toolwire.CheckRunRequest{Spec: json.RawMessage(tt.check)}))
			if resp.Refused != "" {
				t.Fatalf("dry run refused: %+v", resp)
			}
			a := resp.Observed.CheckRun.Attempts[0]
			s := a.Steps[len(a.Steps)-1]
			if a.Result != tt.result || s.AssertionID != tt.id || s.Assertion != (tt.id != "") {
				t.Fatalf("attempt %+v, want %s with assertion %q", a, tt.result, tt.id)
			}
		})
	}
}

// TestWorkspaceKey: in a workspace every tool call carries the key,
// compared in constant time; a call without it, or with another, is
// refused and recorded, and spends none of the agent's calls. The
// readiness probe needs none.
func TestWorkspaceKey(t *testing.T) {
	cfg := wsConfig()
	cfg.MaxToolCalls = maulv1.MinToolCalls
	h := newWorkspaceHarness(t, cfg, &fakeSandbox{}, &fakeSandbox{})
	for _, key := range []string{"", strings.Repeat("f", 64), wsKey[:63], wsKey + "0", strings.ToUpper(wsKey)} {
		req := httptest.NewRequest(http.MethodPost, toolwire.Path(toolwire.VerbExec), strings.NewReader(`{"argv":["ls"]}`))
		req.RemoteAddr = "10.9.9.9:41000"
		if key != "" {
			req.Header.Set(toolwire.KeyHeader, key)
		}
		rec := httptest.NewRecorder()
		h.relay.Handler().ServeHTTP(rec, req)
		var resp toolwire.Response
		if err := toolwire.DecodeResponse(rec.Body, &resp); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusUnauthorized || resp.Refused != RefusedKey || resp.Seq < 1 {
			t.Fatalf("key %q: %d %+v", key, rec.Code, resp)
		}
	}
	rec := httptest.NewRecorder()
	h.relay.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness without a key: %d", rec.Code)
	}
	for range maulv1.MinToolCalls {
		if _, resp := h.callKeyed(toolwire.VerbFiles, `{"op":"ls"}`); resp.Refused != "" {
			t.Fatalf("a keyed call was refused: %+v", resp)
		}
	}
	l := h.finishKeyed()
	refused := 0
	for _, e := range l.Entries {
		if e.Refused == RefusedKey {
			refused++
			if bytes.Contains(e.Args, []byte(wsKey)) {
				t.Errorf("entry %d records the key", e.Seq)
			}
		}
	}
	if refused != 5 {
		t.Fatalf("%d key refusals recorded, want 5", refused)
	}
}

// TestWorkspaceRefusesPreviewVerbs: a workspace has no preview: http,
// browser, logs and events are refused by name.
func TestWorkspaceRefusesPreviewVerbs(t *testing.T) {
	h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{}, &fakeSandbox{})
	for verb, body := range map[toolwire.Verb]string{
		toolwire.VerbHTTP: `{"method":"GET","path":"/"}`, toolwire.VerbBrowser: `{"verb":"snapshot"}`,
		toolwire.VerbLogs: `{"component":"api"}`, toolwire.VerbEvents: `{}`,
	} {
		_, resp := h.callKeyed(verb, body)
		if resp.Refused != toolwire.RefusedUnavailable || !strings.Contains(resp.Reason, "no preview") {
			t.Errorf("%s in a workspace: %+v", verb, resp)
		}
	}
	// A preview check is off the workspace's surface.
	_, resp := h.dryRunKeyed(scoreCheck, "")
	if resp.Refused != toolwire.RefusedCheck {
		t.Errorf("an http check in a workspace: %+v", resp)
	}
}

// TestSlotRefusesWorkspaceCalls: the slot has one tree: a base side, files
// and run are refused there.
func TestSlotRefusesWorkspaceCalls(t *testing.T) {
	ex := &fakeExec{run: func(*toolwire.ExecRequest, int) (toolexec.Result, error) { return toolexec.Result{}, nil }}
	h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
	for verb, body := range map[toolwire.Verb]string{
		toolwire.VerbExec: `{"argv":["ls"],"side":"base"}`,
		toolwire.VerbCheckRun: mustJSON(t, toolwire.CheckRunRequest{Spec: json.RawMessage(scoreCheck),
			Side: toolwire.SideBase}),
		toolwire.VerbFiles: `{"op":"ls"}`,
		toolwire.VerbRun:   `{"language":"go","files":[{"name":"main.go","content":"eA=="}]}`,
	} {
		_, resp := h.call(verb, body)
		if resp.Refused == "" || resp.Seq < 1 {
			t.Errorf("%s in the slot: %+v", verb, resp)
		}
	}
	if len(ex.reqs) != 0 {
		t.Fatalf("the slot sandbox ran %+v", ex.reqs)
	}
	for _, e := range h.finish().Entries {
		if e.Side != "" {
			t.Errorf("slot entry %d names side %q", e.Seq, e.Side)
		}
	}
}

// TestExploratoryCalls: an agent's exec and run substitute {{bin}} and
// {{tree}} (never {{run}}), each in a fresh scratch directory of its own,
// at the side asked for.
func TestExploratoryCalls(t *testing.T) {
	head, base := &fakeSandbox{do: printing("hi", 0)}, &fakeSandbox{do: printing("lo", 3)}
	h := newWorkspaceHarness(t, wsConfig(), head, base)
	_, resp := h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}","{{tree}}/in.json","{{run}}"],"side":"base",
		"files":[{"name":"in.json","content":"e3t0cmVlfX0="}]}`)
	if resp.Refused != "" || *resp.Observed.Exit != 3 || resp.Untrusted != "lo" {
		t.Fatalf("base exec: %+v", resp)
	}
	reqs := base.nonBuilds()
	if len(reqs) != 1 || !slices.Equal(reqs[0].Argv, []string{"/work/bin/bb", "/work/tree/in.json", "{{run}}"}) ||
		string(reqs[0].Files[0].Content) != "/work/tree" || reqs[0].Run == "" {
		t.Fatalf("base exec request: %+v", reqs)
	}
	h.callKeyed(toolwire.VerbExec, `{"argv":["ls"]}`)
	if hr := head.nonBuilds(); len(hr) != 1 || hr[0].Run == reqs[0].Run {
		t.Fatalf("head exec: %+v", hr)
	}
	l := h.finishKeyed()
	sides := []toolwire.Side{}
	for _, e := range l.Entries {
		if e.Verb == toolwire.VerbExec {
			sides = append(sides, e.Side)
		}
	}
	if !slices.Equal(sides, []toolwire.Side{toolwire.SideBase, ""}) {
		t.Fatalf("exec entry sides %q", sides)
	}
}

// TestRunRefusesSubdirectoryFiles: a run program's files sit at the top of
// its directory (U6).
func TestRunRefusesSubdirectoryFiles(t *testing.T) {
	head := &fakeSandbox{}
	h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
	_, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go",
		Files: []toolwire.File{{Name: "pkg/main.go", Content: []byte("package main")}}}))
	if resp.Refused != toolwire.RefusedInvalid || len(head.nonBuilds()) != 0 {
		t.Fatalf("a run file in a subdirectory: %+v", resp)
	}
	const sub = `{"id":"count.sub","outcome":"count","form":"run","run":{"language":"go","files":[
 {"path":"pkg/main.go","content":"package main\n// {{bin}}\nfunc main() {}\n"}],
 "expect":{"exit":0,"stdoutContains":"ok"}}}`
	_, resp = h.dryRunKeyed(sub, "")
	if resp.Refused == "" {
		if r := resp.Observed.CheckRun; r.Result != toolwire.RunError {
			t.Fatalf("a run check with a file in a subdirectory: %+v", r)
		}
	}
	if len(head.nonBuilds()) != 0 {
		t.Fatalf("the sandbox ran %+v", head.nonBuilds())
	}
}

// TestRunCheckRequests: a run check compiles, then runs, its program in one
// scratch directory per attempt: maul's go.mod beside the agent's files,
// the workspace made by go work init, every placeholder substituted, the
// compile offline and read-only.
func TestRunCheckRequests(t *testing.T) {
	head := &fakeSandbox{do: printing("ok", 0)}
	cfg := wsConfig()
	cfg.Workspace.Env = []toolexec.EnvVar{{Name: "GOPROXY", Value: "http://proxy/go"},
		{Name: "GOFLAGS", Value: "-mod=mod"}, {Name: "GOMODCACHE", Value: "/tmp/go/pkg/mod"}}
	h := newWorkspaceHarness(t, cfg, head, &fakeSandbox{})
	_, resp := h.dryRunKeyed(cliRunCheck, "")
	if resp.Refused != "" || resp.Observed.CheckRun.Result != toolwire.RunPass {
		t.Fatalf("run check: %+v", resp)
	}
	reqs := head.nonBuilds()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want a workspace init, a compile and a run", len(reqs))
	}
	winit, compile, run := reqs[0], reqs[1], reqs[2]
	if compile.Run == "" || compile.Run != run.Run || winit.Run != run.Run {
		t.Fatalf("init, compile and run do not share a scratch directory: %q %q %q", winit.Run, compile.Run, run.Run)
	}
	if !slices.Equal(winit.Argv, []string{"go", "work", "init", ".", "/work/tree"}) ||
		!slices.Equal(compile.Argv, []string{"go", "build", "-o", consumerBinary, "."}) ||
		!slices.Equal(run.Argv, []string{"./" + consumerBinary, compile.Run}) || len(run.Files) != 0 ||
		len(compile.Files) != 0 {
		t.Fatalf("init %q, compile %q, run %q", winit.Argv, compile.Argv, run.Argv)
	}
	if !winit.Fresh || compile.Fresh || run.Fresh {
		t.Fatalf("fresh: init %t, compile %t, run %t; want only the attempt's first", winit.Fresh, compile.Fresh,
			run.Fresh)
	}
	assertProgram(t, winit, compile, cfg.Workspace.SearchPath)
}

// assertProgram holds a run check's init and compile requests to what maul
// writes and passes: the substituted program and maul's go.mod, and the
// compile environment.
func assertProgram(t *testing.T, winit, compile toolexec.Request, searchPath string) {
	t.Helper()
	files := map[string]string{}
	for _, f := range winit.Files {
		files[f.Name] = string(f.Content)
	}
	if strings.Contains(files["main.go"], "{{") || !strings.Contains(files["main.go"], "/work/bin/bb on /work/tree") ||
		!strings.Contains(files["main.go"], "MAUL-ASSERTION-FAILED-") {
		t.Fatalf("main.go not substituted: %q", files["main.go"])
	}
	if files["go.mod"] != "module maulcheck\n\ngo 1.24\n" || len(files) != 2 {
		t.Fatalf("program files %q", files)
	}
	if !slices.Equal(winit.Env, compile.Env) {
		t.Fatalf("init env %+v, compile env %+v", winit.Env, compile.Env)
	}
	env := map[string]string{}
	for _, e := range compile.Env {
		env[e.Name] = e.Value
	}
	if env["GOPROXY"] != "off" || env["GOFLAGS"] != "-mod=readonly" || env["GOENV"] != "off" ||
		env["GOMODCACHE"] != "/tmp/go/pkg/mod" || env["PATH"] != searchPath {
		t.Fatalf("compile env %+v", env)
	}
	for _, e := range compile.Env {
		if !toolexec.AllowedEnv(e.Name) {
			t.Errorf("the compile passes %s, which the shim refuses", e.Name)
		}
	}
}

// TestRunCompileFailureIsError: a program that does not compile is an
// error (n/a at base, never a fail), whatever the compiler said.
func TestRunCompileFailureIsError(t *testing.T) {
	for _, msg := range []string{"./main.go:5:9: undefined: bb.Parse", "package foo is not in std",
		"no required module provides package example.com/x", "x.y undefined (type T has no field or method Z)"} {
		head := &fakeSandbox{do: func(req *toolexec.Request, n int) (toolexec.Result, error) {
			if req.Kind == toolexec.KindBuild {
				return toolexec.Result{}, nil
			}
			if req.Argv[0] == "go" {
				return toolexec.Result{Exit: 1, Output: msg}, nil
			}
			return toolexec.Result{Output: "ok"}, nil
		}}
		h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
		_, resp := h.dryRunKeyed(cliRunCheck, "")
		if resp.Refused != "" || resp.Observed.CheckRun.Result != toolwire.RunError {
			t.Fatalf("%q: %+v", msg, resp)
		}
		for _, r := range head.nonBuilds() {
			if r.Argv[0] != "go" {
				t.Fatalf("%q: ran the program after a failed compile", msg)
			}
		}
	}
}

// TestWorkspaceRedConfirm: in a workspace a check shown red by assertion
// needs no data of its own run (SPEC S2-04's decision for S2-08: the
// writable tree is no shared store), so its red confirmation is not
// refused for the slot's run-data rule.
func TestWorkspaceRedConfirm(t *testing.T) {
	head := &fakeSandbox{do: printing("count=2", 0)}
	h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{do: printing("count=2", 0)})
	_, resp := h.dryRunKeyed(cliExecCheck, "")
	if _, ok := resp.Observed.CheckRun.AssertionStep(); !ok {
		t.Fatalf("dry run not red by assertion: %+v", resp.Observed.CheckRun)
	}
	d := resp.Observed.CheckRun.Digest
	_, resp = h.callKeyed(toolwire.VerbConfirm, `{"digests":["`+d+`"],"expectFail":["`+d+`"]}`)
	if resp.Refused != "" {
		t.Fatalf("a workspace red confirmation: %+v", resp)
	}
	if r := resp.Observed.CheckRuns[0]; r.Result != toolwire.RunFail {
		t.Fatalf("red confirmation: %+v", r)
	}
}
