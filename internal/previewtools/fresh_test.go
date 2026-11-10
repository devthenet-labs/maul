// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// patchedCheck is a library run check that passes only on a tree holding
// bb.Patched, which neither side's tree has.
const patchedCheck = `{"id":"bb.patched","outcome":"bb","form":"run","run":{"language":"go","files":[
 {"path":"main.go","content":
  "package main\n\nimport (\"fmt\"; \"example.com/bb\")\n\nfunc main() { fmt.Println(bb.Patched()) }\n"}],
 "expect":{"exit":0,"stdoutContains":"true"}}}`

// TestCheckRunRequestsAreFresh (review VC-2): every check run's program
// starts from the build at its own side, a dry run, a replay and a
// confirmation alike; an exploratory exec or run keeps what earlier calls
// left.
func TestCheckRunRequestsAreFresh(t *testing.T) {
	head, base := &fakeSandbox{do: printing("count=3", 0)}, &fakeSandbox{do: printing("count=3", 0)}
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	h := newWorkspaceHarness(t, cfg, head, base)
	h.relay.replay(context.Background())
	_, dry := h.dryRunKeyed(cliExecCheck, "")
	if dry.Observed == nil || dry.Observed.CheckRun == nil {
		t.Fatalf("dry run: %+v", dry)
	}
	h.dryRunKeyed(cliExecCheck, toolwire.SideBase)
	_, resp := h.callKeyed(toolwire.VerbConfirm, mustJSON(t, toolwire.ConfirmRequest{
		Digests: []string{dry.Observed.CheckRun.Digest}}))
	if resp.Refused != "" {
		t.Fatalf("confirm: %+v", resp)
	}
	checkRuns := func(sb *fakeSandbox) int {
		t.Helper()
		reqs := sb.nonBuilds()
		for _, r := range reqs {
			if !r.Fresh {
				t.Errorf("side %q: a check run's request is not fresh: %+v", r.Side, r)
			}
		}
		return len(reqs)
	}
	// Replay, dry run and confirmation at the head; the replay's,
	// the dry run's and the confirmation's base runs at the base.
	if n := checkRuns(head); n != 3 {
		t.Errorf("%d check runs at the head, want 3", n)
	}
	if n := checkRuns(base); n != 3 {
		t.Errorf("%d check runs at the base, want 3", n)
	}
	h.callKeyed(toolwire.VerbExec, `{"argv":["ls"]}`)
	h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go",
		Files: []toolwire.File{{Name: "main.go", Content: []byte("package main\nfunc main() {}\n")}}}))
	reqs := head.nonBuilds()[3:]
	if len(reqs) != 4 {
		t.Fatalf("%d exploratory requests, want an exec, an init, a compile and a run", len(reqs))
	}
	for _, r := range reqs {
		if r.Fresh {
			t.Errorf("an exploratory request is fresh: %+v", r)
		}
	}
}

// TestCheckRunsStartFromTheBuild (review VC-2): an exploratory exec that
// patches a side's writable tree never reaches a check run there. Each
// side's dry run of a check that passes only on the patch must not pass,
// and the side's tree is the build's again after it, while the other
// side's tree is left alone.
func TestCheckRunsStartFromTheBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds with the go command")
	}
	cfg := liveConfig(t)
	cfg.Workspace.SearchPath += ":/usr/bin:/bin"
	patch := "package bb\n\nfunc Patched() bool { return true }\n"
	for _, side := range []toolwire.Side{"", toolwire.SideBase} {
		head, base := newLiveWorkspace(t), newLiveWorkspace(t)
		mine, other := head, base
		if side == toolwire.SideBase {
			mine, other = base, head
		}
		// In a pod both sides hold their tree at one path; here each has
		// its own, so {{tree}} is the side's under test.
		h := newHarness(t, cfg, routes{}, func(o *Options) {
			o.Head, o.Base, o.Trees, o.Key = head.client, base.client, head.trees, wsKey
			o.WorkTree = filepath.Join(mine.work, "tree")
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		h.relay.buildTrees(ctx)
		patched := filepath.Join(mine.work, "tree", "patched.go")
		_, resp := h.callKeyed(toolwire.VerbExec, mustJSON(t, toolwire.ExecRequest{Side: side,
			Argv: []string{"sh", "-c", "printf '" + strings.ReplaceAll(patch, "\n", `\n`) + "' > " + patched}}))
		if resp.Refused != "" || resp.Observed == nil || *resp.Observed.Exit != 0 {
			t.Fatalf("side %q: the exploratory patch: %+v", side, resp)
		}
		if _, err := os.Stat(patched); err != nil {
			t.Fatalf("side %q: the exploratory patch was not written: %v", side, err)
		}
		_, cr := h.dryRunKeyed(patchedCheck, side)
		if cr.Refused != "" || cr.Observed == nil || cr.Observed.CheckRun == nil {
			t.Fatalf("side %q: dry run: %+v", side, cr)
		}
		if got := cr.Observed.CheckRun.Result; got == toolwire.RunPass {
			t.Errorf("side %q: a check run passed on code the exploration wrote", side)
		}
		if _, err := os.Stat(patched); !os.IsNotExist(err) {
			t.Errorf("side %q: the patch survived the check run: %v", side, err)
		}
		if _, err := os.Stat(filepath.Join(other.work, "tree", "bb.go")); err != nil {
			t.Errorf("side %q: the other side's tree was touched: %v", side, err)
		}
	}
}
