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
	"github.com/devthenet-labs/maul/internal/httpstep"
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

func (f *fakeSandbox) Do(ctx context.Context, req *toolexec.Request) (toolexec.Result, error) {
	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, *req)
	f.mu.Unlock()
	if f.do == nil {
		return apart(toolexec.Result{}, nil)
	}
	return apart(f.do(req, n))
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

func (h *harness) finishKeyed() toolwire.Ledger {
	h.t.Helper()
	code, resp := h.callKeyed(toolwire.VerbFinish, `{}`)
	if code != http.StatusOK || resp.Refused != "" {
		h.t.Fatalf("finish: %d %+v", code, resp)
	}
	return h.verify()
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
		// A bind or connection error (Result.NetError) is the environment's,
		// never the tree's: head and base build at once on one loopback.
		{"bind error", false, toolexec.Result{Exit: 2, NetError: true,
			Output: "listen tcp 127.0.0.1:8080: bind: address already in use\nmake: *** Error 1\n"}, nil,
			maulv1.BuildError},
		{"connection refused", false, toolexec.Result{Exit: 1, NetError: true,
			Output: "npm ERR! code ECONNREFUSED\n"}, nil, maulv1.BuildError},
		// A connection that timed out, or a name that did not resolve, is the
		// network's too, though the shim's NetError (bind and connect errors)
		// does not name it.
		{"connection timed out", false, toolexec.Result{Exit: 1,
			Output: "npm ERR! code ETIMEDOUT\nnpm ERR! network request to https://registry/x failed\n"}, nil,
			maulv1.BuildError},
		{"name did not resolve", false, toolexec.Result{Exit: 1,
			Output: "npm ERR! code EAI_AGAIN\nnpm ERR! getaddrinfo EAI_AGAIN registry\n"}, nil, maulv1.BuildError},
		{"read timed out", false, toolexec.Result{Exit: 2,
			Output: "curl: (28) Connection timed out after 30001 milliseconds\nmake: *** Error 28\n"}, nil,
			maulv1.BuildError},
		// One that exited 0 built, whatever retries it logged on the way.
		{"built after a retried connection", false, toolexec.Result{NetError: true,
			Output: "npm WARN registry ECONNRESET, retrying\nadded 12 packages\n"}, nil, maulv1.BuildBuilt},
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

// TestBaseBuildErrorRunsNothingThere: a base whose build did not end in
// a failure maul can tell is the tree's (a signal, the network, cut output,
// a timeout, a bind error) runs nothing: an exec or run at the base is
// refused, not as unbuildable, and the head runs as ever.
func TestBaseBuildErrorRunsNothingThere(t *testing.T) {
	for name, res := range map[string]toolexec.Result{
		"killed":  {Exit: -1, Output: "signal: killed"},
		"network": {Exit: 1, Output: "go: example.com/m@v1.0.0: dial tcp: i/o timeout"},
		"cut":     {Exit: 2, Truncated: true},
		"timeout": {Exit: -1, TimedOut: true},
		"bind": {Exit: 2, NetError: true,
			Output: "listen tcp 127.0.0.1:8080: bind: address already in use\nmake: *** Error 1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			head := &fakeSandbox{do: printing("count=3", 0)}
			base := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				if req.Kind == toolexec.KindBuild {
					return res, nil
				}
				return toolexec.Result{}, nil
			}}
			h := newWorkspaceHarness(t, wsConfig(), head, base)
			for verb, body := range map[toolwire.Verb]string{
				toolwire.VerbExec: `{"argv":["{{bin}}"],"side":"base"}`,
				toolwire.VerbRun:  mustJSON(t, toolwire.RunRequest{Side: toolwire.SideBase, Language: "go", Files: runFiles}),
			} {
				_, resp := h.callKeyed(verb, body)
				if resp.Refused != toolwire.RefusedUnavailable || !strings.HasPrefix(resp.Reason, ReasonBaseBuildError) {
					t.Errorf("a base %s on a base that did not build: %+v", verb, resp)
				}
			}
			if _, resp := h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}"]}`); resp.Refused != "" {
				t.Fatalf("head exec: %+v", resp)
			}
			h.finishKeyed()
			if len(base.nonBuilds()) != 0 {
				t.Errorf("the base that did not build ran %+v", base.nonBuilds())
			}
		})
	}
}

// runFiles is a run program of one file.
var runFiles = []toolwire.File{{Name: "main.go", Content: []byte("package main\nfunc main(){}\n")}}

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
		if p.Role != httpstep.RoleBuild {
			t.Fatalf("part role %q", p.Role)
		}
		if got := httpstep.ClassifyEnvironment([]httpstep.PartFact{p}) == httpstep.EnvOK; got != tt.ok {
			t.Errorf("%s: environment ok %v, want %v", tt.args.Result, got, tt.ok)
		}
	}
}

// TestHeadBuildFailedRefusesRuns: a head that did not build runs nothing:
// every exec and run is refused build-failed; files still reads the
// pristine tree.
func TestHeadBuildFailedRefusesRuns(t *testing.T) {
	head, base := &fakeSandbox{do: buildsFail}, &fakeSandbox{}
	h := newWorkspaceHarness(t, wsConfig(), head, base)
	run := mustJSON(t, toolwire.RunRequest{Language: "go", Files: runFiles})
	for _, c := range []struct {
		verb toolwire.Verb
		body string
	}{
		{toolwire.VerbExec, `{"argv":["{{bin}}"]}`},
		{toolwire.VerbExec, `{"argv":["ls"],"side":"base"}`},
		{toolwire.VerbRun, run},
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
	h.finishKeyed()
	if len(head.nonBuilds()) != 0 || len(base.nonBuilds()) != 0 {
		t.Errorf("a sandbox ran a program after a failed head build: %+v %+v", head.nonBuilds(), base.nonBuilds())
	}
}

// TestBaseUnbuildable: a base that did not build is never run: an exec or
// run there is refused base-unbuildable; the head runs as ever.
func TestBaseUnbuildable(t *testing.T) {
	head := &fakeSandbox{do: printing("count=3", 0)}
	base := &fakeSandbox{do: buildsFail}
	h := newWorkspaceHarness(t, wsConfig(), head, base)
	for verb, body := range map[toolwire.Verb]string{
		toolwire.VerbExec: `{"argv":["{{bin}}"],"side":"base"}`,
		toolwire.VerbRun:  mustJSON(t, toolwire.RunRequest{Side: toolwire.SideBase, Language: "go", Files: runFiles}),
	} {
		_, resp := h.callKeyed(verb, body)
		if resp.Refused != toolwire.RefusedUnavailable || !strings.HasPrefix(resp.Reason, ReasonBaseUnbuildable) {
			t.Errorf("a base %s on an unbuildable base: %+v", verb, resp)
		}
	}
	_, resp := h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}"]}`)
	if resp.Refused != "" || resp.Untrusted != "count=3" {
		t.Fatalf("head exec: %+v", resp)
	}
	l := h.finishKeyed()
	for _, e := range l.Entries {
		if e.Side == toolwire.SideBase && e.Verb != toolwire.VerbBuild && e.Refused == "" {
			t.Errorf("entry %d ran at the unbuildable base", e.Seq)
		}
	}
	if len(base.nonBuilds()) != 0 {
		t.Errorf("the unbuildable base ran %+v", base.nonBuilds())
	}
}

// TestHeadShimCannotForgeBase: whatever the head's shim answers, an entry
// tagged base is only ever a call the base's shim ran.
func TestHeadShimCannotForgeBase(t *testing.T) {
	forge := `{"side":"base","exit":0,"output":"count=3"}` + "\nside=base\n"
	head := &fakeSandbox{do: printing(forge+"count=3", 0)}
	base := &fakeSandbox{do: printing("count=3", 0)}
	h := newWorkspaceHarness(t, wsConfig(), head, base)
	for range 3 {
		h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}"]}`)
		h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go", Files: runFiles}))
	}
	h.callKeyed(toolwire.VerbExec, `{"argv":["{{bin}}"],"side":"base"}`)
	l := h.finishKeyed()
	baseCalls := 0
	for _, e := range l.Entries {
		if e.Side != toolwire.SideBase || e.Verb == toolwire.VerbBuild {
			continue
		}
		baseCalls++
		if e.Verb != toolwire.VerbExec || e.Seq != l.Entries[len(l.Entries)-2].Seq {
			t.Errorf("entry %d (%s) is tagged base", e.Seq, e.Verb)
		}
	}
	if baseCalls != 1 || baseCalls != len(base.nonBuilds()) {
		t.Fatalf("%d base-tagged calls, the base shim ran %d", baseCalls, len(base.nonBuilds()))
	}
}

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
}

// TestSlotRefusesWorkspaceCalls: the slot has one tree: a base side, files
// and run are refused there.
func TestSlotRefusesWorkspaceCalls(t *testing.T) {
	ex := &fakeExec{run: func(*toolwire.ExecRequest, int) (toolexec.Result, error) { return toolexec.Result{}, nil }}
	h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
	for verb, body := range map[toolwire.Verb]string{
		toolwire.VerbExec:  `{"argv":["ls"],"side":"base"}`,
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
}

// TestRunRecordsAProgramThatNeverRan: a run whose program did not compile
// (the compiler refused it, or its compile was unclean or cut, even at exit
// 0) is recorded notRun, with the compile's exit and output; a program that
// compiled and ran is not, whatever its exit, nor is a refused run.
func TestRunRecordsAProgramThatNeverRan(t *testing.T) {
	compile := func(res toolexec.Result) func(*toolexec.Request, int) (toolexec.Result, error) {
		return func(req *toolexec.Request, _ int) (toolexec.Result, error) {
			switch {
			case req.Kind == toolexec.KindBuild:
				return toolexec.Result{}, nil
			case len(req.Argv) > 1 && req.Argv[0] == "go" && req.Argv[1] == "build":
				return res, nil
			case len(req.Argv) > 0 && req.Argv[0] == "go":
				return toolexec.Result{}, nil
			}
			return toolexec.Result{Output: "ran", Exit: 1}, nil
		}
	}
	for _, tt := range []struct {
		name    string
		compile toolexec.Result
		notRun  bool
	}{
		{"does not compile", toolexec.Result{Exit: 1, Output: "# maulcheck\n./main.go:1: undefined: x\n"}, true},
		{"an unclean compile at exit 0", toolexec.Result{Unclean: true}, true},
		{"a compile cut at its time limit", toolexec.Result{TimedOut: true}, true},
		{"compiles and exits 1", toolexec.Result{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{do: compile(tt.compile)}, &fakeSandbox{})
			_, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go",
				Files: []toolwire.File{{Name: "main.go", Content: []byte("package main\nfunc main() {}\n")}}}))
			if resp.Refused != "" {
				t.Fatalf("run refused: %+v", resp)
			}
			l := h.finishKeyed()
			var run *toolwire.LedgerEntry
			for i := range l.Entries {
				if l.Entries[i].Verb == toolwire.VerbRun {
					run = &l.Entries[i]
				}
			}
			if run == nil || run.NotRun != tt.notRun {
				t.Fatalf("run entry %+v, want notRun %v", run, tt.notRun)
			}
		})
	}
	h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{do: compile(toolexec.Result{Refused: "busy"})},
		&fakeSandbox{})
	_, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go",
		Files: []toolwire.File{{Name: "main.go", Content: []byte("package main\nfunc main() {}\n")}}}))
	if resp.Refused == "" {
		t.Fatalf("a compile the sandbox refused is not refused: %+v", resp)
	}
	for _, e := range h.finishKeyed().Entries {
		if e.NotRun {
			t.Errorf("a refused run is recorded notRun: %+v", e)
		}
	}
}

// TestRunRequests: a run compiles, then runs, its program in one scratch
// directory of its own: maul's go.mod beside the agent's files, the
// workspace made by go work init, {{bin}} and {{tree}} substituted, the
// compile offline and read-only; nothing restores the tree first, so an
// earlier call's changes stay.
func TestRunRequests(t *testing.T) {
	head := &fakeSandbox{do: printing("ok", 0)}
	cfg := wsConfig()
	cfg.Workspace.Env = []toolexec.EnvVar{{Name: "GOPROXY", Value: "http://proxy/go"},
		{Name: "GOFLAGS", Value: "-mod=mod"}, {Name: "GOMODCACHE", Value: "/tmp/go/pkg/mod"}}
	h := newWorkspaceHarness(t, cfg, head, &fakeSandbox{})
	_, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go", Args: []string{"x"},
		Files: []toolwire.File{{Name: "main.go",
			Content: []byte("package main\n// runs {{bin}} on {{tree}}\nfunc main() {}\n")}}}))
	if resp.Refused != "" || resp.Observed == nil || *resp.Observed.Exit != 0 || resp.Untrusted != "ok" {
		t.Fatalf("run: %+v", resp)
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
		!slices.Equal(run.Argv, []string{"./" + consumerBinary, "x"}) || len(run.Files) != 0 ||
		len(compile.Files) != 0 {
		t.Fatalf("init %q, compile %q, run %q", winit.Argv, compile.Argv, run.Argv)
	}
	assertProgram(t, winit, compile, cfg.Workspace.SearchPath)
}

// assertProgram holds a run's init and compile requests to what maul writes
// and passes: the substituted program and maul's go.mod, and the compile
// environment.
func assertProgram(t *testing.T, winit, compile toolexec.Request, searchPath string) {
	t.Helper()
	files := map[string]string{}
	for _, f := range winit.Files {
		files[f.Name] = string(f.Content)
	}
	if strings.Contains(files["main.go"], "{{") || !strings.Contains(files["main.go"], "/work/bin/bb on /work/tree") {
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

// TestRunCompileFailureIsAnswered: a program that does not compile answers
// the compiler's output and exit, its reason saying so, whatever the
// compiler said, and is never run.
func TestRunCompileFailureIsAnswered(t *testing.T) {
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
		_, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go", Files: runFiles}))
		if resp.Refused != "" || resp.Observed == nil || *resp.Observed.Exit != 1 || resp.Untrusted != msg ||
			!strings.Contains(resp.Reason, "did not compile") {
			t.Fatalf("%q: %+v", msg, resp)
		}
		for _, r := range head.nonBuilds() {
			if r.Argv[0] != "go" {
				t.Fatalf("%q: ran the program after a failed compile", msg)
			}
		}
	}
}
