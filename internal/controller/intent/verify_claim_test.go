// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// doubtedReport is an evaluator's report that the version outcome does not
// hold, with a note carrying a mention and a reference, citing its own
// dry run of a check (seq 4, also where it saw the outcome fail), an
// exploratory http call (6), a source read (5) and a refused call (7):
// the shape gate 3's G1 and G10 left unshown.
const doubtedReport = `---
verdicts:
  - id: version-shown
    verdict: fail
    ledger: [5, 6, 7, 4]
    contrary: [4]
    note: "GET /version answers dev, not the commit @octocat #12"
  - id: still-healthy
    verdict: pass
    ledger: [3]
---
The version endpoint answers the wrong value.
`

// agentEntry is an agent's call at seq: verb, its args and its whole
// output.
func agentEntry(seq int64, verb toolwire.Verb, args, output string) toolwire.LedgerEntry {
	sum := sha256.Sum256([]byte(output))
	return toolwire.LedgerEntry{Seq: seq, TS: time.Unix(seq, 0).UTC(), Kind: toolwire.KindAgent, Verb: verb,
		Args: json.RawMessage(args), Output: output, OutputSHA256: hex.EncodeToString(sum[:])}
}

// TestDoubtedOutcomeShowsTheEvaluatorsClaim (gate 3 G1, G10): an outcome
// the evaluator judged not to hold, with no check maul re-ran showing it,
// shows on the issue and on the pull request the evaluator's note, marked
// as its unconfirmed claim, and the calls it cites as the tools ledger
// recorded them: its dry run's check steps and observed failure, then its
// http call's request and output, defanged; never a source read or a
// refused call. Showing it changes no verdict.
func TestDoubtedOutcomeShowsTheEvaluatorsClaim(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(doubtedReport)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, health)
	e.review(name, 3)
	pv := e.evalRound(name, v1alpha1.CheckRun{CheckDigest: health.Digest, Result: v1alpha1.CheckRunPass,
		Token: "t2", Seq: 3})
	before := e.verification(name)
	i := slices.IndexFunc(before.Outcomes, func(r v1alpha1.OutcomeResult) bool { return r.ID == "version-shown" })
	if before.State != v1alpha1.VerificationUnconfirmed || i < 0 ||
		before.Outcomes[i].Reason != acceptance.ReasonModelOnlyFail {
		t.Fatalf("verification = %s %+v, want Unconfirmed with version-shown model-only-fail", before.State,
			before.Outcomes)
	}
	dry := checkRunEntry(4, version.Digest, "version-shown", 1, `/note: expected "t1-v", got "dev @octocat #3"`)
	dry.Kind, dry.Args = toolwire.KindAgent, json.RawMessage(`{"check":`+version.Body+`}`)
	dry.CheckRun.Mode = toolwire.ModeDryRun
	refused := agentEntry(7, toolwire.VerbExec, `{"argv":["curl","refused-call"]}`, "")
	refused.Refused, refused.Reason = "check-invalid", "refused"
	e.ledgerOfEntries(pv.Name,
		checkRunEntry(3, health.Digest, "still-healthy", 0, "never shown"),
		dry,
		agentEntry(5, toolwire.VerbFiles, `{"op":"read","path":"version.go"}`, "package main // SOURCE-READ"),
		agentEntry(6, toolwire.VerbHTTP, `{"target":"app","method":"GET","path":"/version"}`,
			`{"version":"dev"} see #44 @octocat`),
		refused)
	e.review(name, 3)
	after := e.verification(name)
	if after.State != before.State || !slices.EqualFunc(after.Outcomes, before.Outcomes,
		func(a, b v1alpha1.OutcomeResult) bool {
			return a.ID == b.ID && a.Result == b.Result && a.Reason == b.Reason
		}) {
		t.Errorf("showing the claim changed the verdict: %s %+v, was %s %+v", after.State, after.Outcomes,
			before.State, before.Outcomes)
	}
	cs := e.verifyComments(name, 1)
	if len(cs) != 1 {
		t.Fatalf("verification comments = %d, want one", len(cs))
	}
	pr := e.get(name).Status.PullRequests[0]
	for surface, body := range map[string]string{"issue": cs[0].Body,
		"pull request": e.onePreviewComment(name, pr.Number).Body} {
		for _, want := range []string{
			"The evaluator's claim on `version-shown`, which maul could not confirm (its words, not checked by " +
				"maul): GET /version answers dev, not the commit `@octocat` `#12`.",
			"Call 4, a dry run of a check:",
			"1. Send `POST /notes`",
			`attempt 1, step 2: failed: ` + "`" + `/note: expected "t1-v", got "dev @ octocat # 3"` + "`",
			"Call 6, an http request:",
			`{"version":"dev"} see # 44 @ octocat`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q:\n%s", surface, want, body)
			}
		}
		for _, never := range []string{"SOURCE-READ", "refused-call", "Call 5", "Call 7", "never shown"} {
			if strings.Contains(body, never) {
				t.Errorf("%s shows %q:\n%s", surface, never, body)
			}
		}
		if strings.Index(body, "Call 4,") > strings.Index(body, "Call 6,") {
			t.Errorf("%s: the call where the evaluator saw the outcome fail is not first:\n%s", surface, body)
		}
	}
}

// TestCitedCallPrefersMaulsOwnRunAtTheHead: a cited dry run at the head
// shows maul's own failing run of the same check in its place; a dry run
// at the base keeps its own record, never the head's failure.
func TestCitedCallPrefersMaulsOwnRunAtTheHead(t *testing.T) {
	version := savedCheck(t, versionCheck)
	args := json.RawMessage(`{"check":` + version.Body + `}`)
	head := checkRunEntry(4, version.Digest, "version-shown", 1, "dry run at head")
	head.Kind, head.Args, head.CheckRun.Mode = toolwire.KindAgent, args, toolwire.ModeDryRun
	base := checkRunEntry(5, version.Digest, "version-shown", 1, "")
	base.Kind, base.Args, base.Side = toolwire.KindAgent, args, toolwire.SideBase
	base.CheckRun.Mode, base.CheckRun.Result = toolwire.ModeDryRun, toolwire.RunPass
	base.CheckRun.Attempts = []toolwire.Attempt{{Run: "b1", Result: toolwire.ResultPass}}
	own := checkRunEntry(20, version.Digest, "version-shown", 1, "maul's own confirmation")
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
	replay := checkRunEntry(3, version.Digest, "version-shown", 1, "maul's replay")
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
