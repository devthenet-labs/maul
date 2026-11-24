// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ownLabelsOf are a verified ledger's own-call labels as the controllers
// read them: over its index, each entry's args from its own line.
func ownLabelsOf(l toolwire.Ledger) []bool {
	args := map[int32]json.RawMessage{}
	for _, e := range l.Entries {
		args[toolledger.ClampInt32(e.Seq)] = e.Args
	}
	return toolledger.OwnCalls(toolledger.Index(l.Entries), func(seq int32) json.RawMessage { return args[seq] },
		true, wsConfig().Workspace.Binary)
}

// TestWorkspaceOwnLabelAfterAnExecTheSandboxDidNotAnswer (FD 1): an exec
// the sandbox did not answer is recorded refused, yet the shim may have run
// it (the answer, not the request, was lost), so a later bare run of the
// built binary at that side is labelled the evaluator's own. A call refused
// for want of the key reached no sandbox, and leaves it unlabelled.
func TestWorkspaceOwnLabelAfterAnExecTheSandboxDidNotAnswer(t *testing.T) {
	const shell = `{"argv":["sh","-c","cp x /work/bin/bb"]}`
	const bare = `{"argv":["{{bin}}","--version"]}`
	t.Run("the sandbox did not answer", func(t *testing.T) {
		lost := false
		head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
			if req.Kind != toolexec.KindBuild && !lost {
				lost = true
				return toolexec.Result{}, errors.New("connection reset by peer")
			}
			return toolexec.Result{}, nil
		}}
		h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
		if _, resp := h.callKeyed(toolwire.VerbExec, shell); resp.Refused != toolwire.RefusedUnavailable {
			t.Fatalf("the lost exec: %+v", resp)
		}
		if _, resp := h.callKeyed(toolwire.VerbExec, bare); resp.Refused != "" {
			t.Fatalf("the bare exec: %+v", resp)
		}
		own := ownLabelsOf(h.finishKeyed())
		if len(own) < 4 || !own[3] {
			t.Fatalf("the bare exec after a lost one is not labelled the evaluator's own: %v", own)
		}
	})
	// The codes that show nothing ran are the relay gate's alone: a sandbox
	// answering under one is recorded as any other sandbox refusal.
	for _, code := range []string{toolwire.RefusedKey, toolwire.RefusedToolCalls} {
		t.Run("a sandbox refusing under "+code, func(t *testing.T) {
			head := &fakeSandbox{do: func(req *toolexec.Request, n int) (toolexec.Result, error) {
				if req.Kind != toolexec.KindBuild && len(req.Argv) > 0 && req.Argv[0] == "sh" {
					return toolexec.Result{Exit: -1, Refused: code, Reason: "no"}, nil
				}
				return toolexec.Result{}, nil
			}}
			h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
			if _, resp := h.callKeyed(toolwire.VerbExec, shell); resp.Refused == "" || resp.Refused == code {
				t.Fatalf("the sandbox's refusal is recorded as %q", resp.Refused)
			}
			if _, resp := h.callKeyed(toolwire.VerbExec, bare); resp.Refused != "" {
				t.Fatalf("the bare exec: %+v", resp)
			}
			own := ownLabelsOf(h.finishKeyed())
			if len(own) < 4 || !own[3] {
				t.Fatalf("the bare exec after a sandbox's refusal is not the evaluator's own: %v", own)
			}
		})
	}
	t.Run("refused for want of the key", func(t *testing.T) {
		h := newWorkspaceHarness(t, wsConfig(), &fakeSandbox{}, &fakeSandbox{})
		if _, resp := h.call(toolwire.VerbExec, shell); resp.Refused != RefusedKey {
			t.Fatalf("the keyless exec: %+v", resp)
		}
		if _, resp := h.callKeyed(toolwire.VerbExec, bare); resp.Refused != "" {
			t.Fatalf("the bare exec: %+v", resp)
		}
		own := ownLabelsOf(h.finishKeyed())
		if len(own) < 4 || own[3] {
			t.Fatalf("a keyless call taints the head: %v", own)
		}
	})
}
