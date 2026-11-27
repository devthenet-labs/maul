// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"strings"
	"testing"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolexec"
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
