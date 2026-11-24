// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"testing"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// notFound is the exec shim's answer for a program it could not find.
var notFound = toolexec.Result{Exit: 127,
	Output: `[maul: could not start nosuch: exec: "nosuch": executable file not found in $PATH]`}

// TestProgramThatCouldNotStartGotNoAnswer (FD 2): an exec, slot or agents,
// or a run whose compiled program the shim could not start ran nothing, so
// the round's index marks it noAnswer and it is never evidence. The relay
// records the shim's output as given, maul's marker alone.
func TestProgramThatCouldNotStartGotNoAnswer(t *testing.T) {
	lastIndexed := func(t *testing.T, l toolwire.Ledger, verb toolwire.Verb) bool {
		t.Helper()
		ix := toolledger.Index(l.Entries)
		for i := len(l.Entries) - 1; i >= 0; i-- {
			if l.Entries[i].Verb == verb {
				if l.Entries[i].Output != notFound.Output || l.Entries[i].Exit != notFound.Exit {
					t.Fatalf("the relay recorded %d %q, not the shim's answer", l.Entries[i].Exit, l.Entries[i].Output)
				}
				return ix[i].NoAnswer
			}
		}
		t.Fatalf("no %s entry", verb)
		return false
	}
	t.Run("slot exec", func(t *testing.T) {
		ex := &fakeExec{run: func(*toolwire.ExecRequest, int) (toolexec.Result, error) { return notFound, nil }}
		h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
		if _, resp := h.call(toolwire.VerbExec, `{"argv":["nosuch"]}`); resp.Refused != "" {
			t.Fatalf("exec: %+v", resp)
		}
		if !lastIndexed(t, h.finish(), toolwire.VerbExec) {
			t.Error("a slot exec that could not start is not noAnswer")
		}
	})
	t.Run("agents exec", func(t *testing.T) {
		head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
			if req.Kind == toolexec.KindBuild {
				return toolexec.Result{}, nil
			}
			return notFound, nil
		}}
		h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
		if _, resp := h.callKeyed(toolwire.VerbExec, `{"argv":["nosuch"]}`); resp.Refused != "" {
			t.Fatalf("exec: %+v", resp)
		}
		if !lastIndexed(t, h.finishKeyed(), toolwire.VerbExec) {
			t.Error("an agents exec that could not start is not noAnswer")
		}
	})
	t.Run("run", func(t *testing.T) {
		head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
			if len(req.Argv) > 0 && req.Argv[0] == "./"+consumerBinary {
				return notFound, nil
			}
			return toolexec.Result{}, nil
		}}
		h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
		if _, resp := h.callKeyed(toolwire.VerbRun, mustJSON(t, toolwire.RunRequest{Language: "go",
			Files: runFiles})); resp.Refused != "" {
			t.Fatalf("run: %+v", resp)
		}
		if !lastIndexed(t, h.finishKeyed(), toolwire.VerbRun) {
			t.Error("a run whose program could not start is not noAnswer")
		}
	})
}
