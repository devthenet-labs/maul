// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// agentEntry is an agent's call at seq: verb, its args and its whole
// output.
func agentEntry(seq int64, verb toolwire.Verb, args, output string) toolwire.LedgerEntry {
	sum := sha256.Sum256([]byte(output))
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindAgent, Verb: verb,
		Args: json.RawMessage(args), Output: output, OutputSHA256: hex.EncodeToString(sum[:])}
}

// TestCitedCallPrefersMaulsOwnRunAtTheHead: a cited dry run at the head
// shows maul's own failing run of the same check in its place; a dry run
// at the base keeps its own record, never the head's failure.
func TestCitedCallPrefersMaulsOwnRunAtTheHead(t *testing.T) {
	version := savedCheck(t, versionCheck)
	args := json.RawMessage(`{"check":` + version.Body + `}`)
	head := checkRunEntry(4, version.Digest, "version-shown", "dry run at head")
	head.Kind, head.Args, head.CheckRun.Mode = toolwire.KindAgent, args, toolwire.ModeDryRun
	base := checkRunEntry(5, version.Digest, "version-shown", "")
	base.Kind, base.Args, base.Side = toolwire.KindAgent, args, toolwire.SideBase
	base.CheckRun.Mode, base.CheckRun.Result = toolwire.ModeDryRun, toolwire.RunPass
	base.CheckRun.Attempts = []toolwire.Attempt{{Run: "b1", Result: toolwire.ResultPass}}
	own := checkRunEntry(20, version.Digest, "version-shown", "maul's own confirmation")
	own.CheckRun.Mode = toolwire.ModeConfirm
	entries := []toolwire.LedgerEntry{head, base, own}
	pv := &v1alpha1.PreviewVerification{Status: v1alpha1.PreviewVerificationStatus{
		Confirmations: []v1alpha1.CheckRun{{CheckDigest: version.Digest, Result: v1alpha1.CheckRunFail, Seq: 20,
			Token: "t9"}}}}
	for _, tc := range []struct {
		entry  toolwire.LedgerEntry
		runSeq int64
		result string
		detail string
	}{
		{head, 20, "fail", "maul's own confirmation"},
		{base, 5, "pass", ""},
	} {
		call, ok := citedCall(pv, entries, tc.entry)
		if !ok || call.Check == nil || call.RunSeq != tc.runSeq || call.Result != tc.result {
			t.Fatalf("call %d = %+v, want run %d %s", tc.entry.Seq, call, tc.runSeq, tc.result)
		}
		if tc.detail != "" && (len(call.Observed) == 0 || call.Observed[0].Detail != tc.detail) {
			t.Errorf("call %d observed %+v, want %q", tc.entry.Seq, call.Observed, tc.detail)
		}
		if tc.detail == "" && len(call.Observed) != 0 {
			t.Errorf("call %d at the base shows a failure: %+v", tc.entry.Seq, call.Observed)
		}
	}
}

// TestConfirmedOutcomeShowsNoClaim: an outcome maul itself judged (here
// failed on a replayed check) shows maul's own evidence, never the
// evaluator's note.
func TestConfirmedOutcomeShowsNoClaim(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 3)
	for _, c := range e.verifyComments(name, 1) {
		if strings.Contains(c.Body, "The evaluator's claim") || strings.Contains(c.Body, "The evaluator judged") {
			t.Errorf("a failed outcome shows the evaluator's claim:\n%s", c.Body)
		}
	}
}

// TestCitedCallShowsOnlyTheEvaluatorsOwnWholeCalls: a claim shows what the
// evaluator sent and what came back, so a cited seq that is maul's own
// call (the relay's) is not shown as one it sent: a relay check run (a
// replay, a confirmation, which the evaluator can cite from `check
// results`) is kept as maul's run, by its mode, and any other relay call
// is not shown. An output whose recorded checksum does not match the copy
// maul holds is not shown, only said to be not whole.
func TestCitedCallShowsOnlyTheEvaluatorsOwnWholeCalls(t *testing.T) {
	pv := &v1alpha1.PreviewVerification{}
	relay := agentEntry(2, toolwire.VerbHTTP, `{"target":"app","method":"GET","path":"/"}`, "relay probe")
	relay.Kind = toolwire.KindRelay
	if call, ok := citedCall(pv, []toolwire.LedgerEntry{relay}, relay); ok {
		t.Errorf("maul's own call shown as the evaluator's: %+v", call)
	}
	version := savedCheck(t, versionCheck)
	replay := checkRunEntry(3, version.Digest, "version-shown", "maul's replay")
	if call, ok := citedCall(pv, []toolwire.LedgerEntry{replay}, replay); !ok ||
		call.Mode != string(toolwire.ModeReplay) || call.RunSeq != 3 {
		t.Errorf("a cited replay = %+v, %v; want maul's replay shown as such", call, ok)
	}
	cut := agentEntry(6, toolwire.VerbExec, `{"argv":["{{bin}}"]}`, "whole output")
	cut.Output = "whole out" // dropped to fit: no longer the bytes its checksum is of
	call, ok := citedCall(pv, []toolwire.LedgerEntry{cut}, cut)
	if !ok || !call.OutputNotWhole || call.Output != "" {
		t.Errorf("an output not whole = %+v, want it marked not whole and not shown", call)
	}
	whole := agentEntry(7, toolwire.VerbExec, `{"argv":["{{bin}}"]}`, "whole output")
	if call, ok := citedCall(pv, []toolwire.LedgerEntry{whole}, whole); !ok || call.OutputNotWhole ||
		call.Output != "whole output" {
		t.Errorf("a whole output = %+v, want it shown", call)
	}
}
