// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"strings"
	"testing"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestWorkspaceBuildOutputFitsTheLedger: a build's output tail can be
// longer than a ledger entry's output (toolexec.MaxBuildOutputBytes over
// toolwire.MaxOutputBytes); the build entries are still entries 1 and 2,
// each keeping the end of its build's output.
func TestWorkspaceBuildOutputFitsTheLedger(t *testing.T) {
	long := strings.Repeat("é line of a verbose build\n", toolexec.MaxBuildOutputBytes/27) + "the last line\n"
	if len(long) <= toolwire.MaxOutputBytes {
		t.Fatalf("the output (%d bytes) does not exceed the ledger's bound", len(long))
	}
	verbose := func(req *toolexec.Request, _ int) (toolexec.Result, error) {
		if req.Kind == toolexec.KindBuild {
			return toolexec.Result{Exit: 2, Output: long}, nil
		}
		return toolexec.Result{}, nil
	}
	h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{do: verbose}, &fakeSandbox{do: verbose})
	l := h.finishKeyed()
	if len(l.Entries) < 2 || l.Entries[0].Verb != toolwire.VerbBuild || l.Entries[1].Verb != toolwire.VerbBuild {
		t.Fatalf("entries 1-2 are not the builds: %d entries", len(l.Entries))
	}
	for _, e := range l.Entries[:2] {
		if len(e.Output) > toolwire.MaxOutputBytes || !strings.HasSuffix(e.Output, "the last line\n") || e.Exit != 2 {
			t.Fatalf("build entry %d: %d bytes, exit %d", e.Seq, len(e.Output), e.Exit)
		}
	}
	if b := buildEntries(t, l); b[""].Result != maulv1.BuildFailed {
		t.Fatalf("head build: %+v", b[""])
	}
}

// TestWorkspaceLedgerHeadReaders: the ledger readers that fill a round's
// replay, confirmations and confirmed checks read the head's runs only. A
// check whose head confirmation failed is not confirmed by its base run
// passing, and no base replay or confirmation is read as the head's.
func TestWorkspaceLedgerHeadReaders(t *testing.T) {
	headCalls := 0
	head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
		if req.Kind == toolexec.KindBuild {
			return toolexec.Result{}, nil
		}
		headCalls++
		if headCalls <= 2 { // the replay and the dry run pass; the confirmation does not
			return toolexec.Result{Output: "count=3"}, nil
		}
		return toolexec.Result{Output: "count=2"}, nil
	}}
	base := &fakeSandbox{do: printing("count=3", 0)}
	cfg := wsConfig()
	cfg.Checks = []StoredCheck{storedExecCheck(t)}
	h := newWorkspaceHarness(t, cfg, head, base)
	h.relay.replay(context.Background())
	_, resp := h.dryRunKeyed(cliExecCheck, "")
	if resp.Refused != "" || resp.Observed.CheckRun.Result != toolwire.RunPass {
		t.Fatalf("dry run: %+v", resp)
	}
	d := resp.Observed.CheckRun.Digest
	if _, resp = h.callKeyed(toolwire.VerbConfirm, `{"digests":["`+d+`"]}`); resp.Refused != "" {
		t.Fatalf("confirm: %+v", resp)
	}
	l := h.finishKeyed()
	outcomes := []maulv1.AcceptanceOutcome{{ID: "count"}}
	if got := toolledger.ConfirmedChecks(l.Entries, outcomes); len(got) != 0 {
		t.Fatalf("a check whose head confirmation failed is confirmed: %+v", got)
	}
	if got := toolledger.ConfirmationRuns(l.Entries); len(got) != 1 || got[0].Result != maulv1.CheckRunFail {
		t.Fatalf("confirmation runs = %+v, want the head's one failed run", got)
	}
	if got := toolledger.ReplayRuns(l.Entries); len(got) != 1 || got[0].Result != maulv1.CheckRunPass {
		t.Fatalf("replay runs = %+v, want the head's one", got)
	}
	if got := toolledger.BaseRuns(l.Entries); len(got) != 2 ||
		got[0].Tree != maulv1.TreeBase || got[1].Tree != maulv1.TreeBase {
		t.Fatalf("base runs = %+v, want the base replay and confirmation", got)
	}
}
