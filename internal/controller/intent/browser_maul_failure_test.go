// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

// namedNoteCheck writes a note of its own run in the browser and expects
// it listed.
const namedNoteCheck = `{"id":"note-listed","outcome":"note-listed","form":"browser","browser":{"steps":[` +
	`{"goto":"/notes"},{"fill":{"locator":{"testid":"name"},"text":"{{run}}-n"}},` +
	`{"click":{"role":"button","name":"Save"}},` +
	`{"expect":{"assert":"text","locator":{"testid":"notes"},"text":{"contains":"{{run}}-n"}}}]}}`

// TestBrowserCheckErrorNeverFailsItsOutcome (code review 2026-11-18, relay
// finding 9): the relay records maul's own browser failures (the shim not
// answering, the key unreadable, the browser closing under a step, the
// test timing out) as an error run with no assertion step, and a stored
// browser check's error run leaves its outcome unsure, with no fix round
// and no suspected defect, even beside the evaluator's cited fail, in a
// full round and on replay, and the replay round asks for the head set's
// full round. A browser check that failed by its assertion is still
// failed with a fix: the change takes no genuine failure away. The bare
// fail row is what the relay recorded for maul's failures before: it
// counts as a reproduced defect.
func TestBrowserCheckErrorNeverFailsItsOutcome(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: "note-listed", Outcome: "A saved note is listed"}
	c := savedCheck(t, namedNoteCheck)
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}, OutcomesDigest: "sha256:o",
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: c.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: c.ID, Reason: "yes"}}}
	stored := []v1alpha1.SavedCheck{c}
	fail := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: o.ID, Verdict: report.VerdictFail,
		Ledger: []int32{3}}}}
	for _, tc := range []struct {
		name   string
		run    v1alpha1.CheckRun
		rung   v1alpha1.VerificationRung
		ev     *report.Evaluation
		result acceptance.Result
		fix    bool
	}{
		{name: "maul's own failure in a full round the evaluator fails",
			run:  v1alpha1.CheckRun{Result: v1alpha1.CheckRunError},
			rung: v1alpha1.RungFull, ev: fail, result: acceptance.ResultUnsure},
		{name: "maul's own failure on replay", run: v1alpha1.CheckRun{Result: v1alpha1.CheckRunError},
			rung: v1alpha1.RungReplay, result: acceptance.ResultUnsure},
		{name: "control: an assertion that did not hold, the evaluator failing it",
			run:  v1alpha1.CheckRun{Result: v1alpha1.CheckRunFail, AssertionStep: 4, AssertionID: "expect"},
			rung: v1alpha1.RungFull, ev: fail, result: acceptance.ResultFailed, fix: true},
		{name: "before: maul's own failure recorded as a bare fail",
			run:  v1alpha1.CheckRun{Result: v1alpha1.CheckRunFail},
			rung: v1alpha1.RungFull, ev: fail, result: acceptance.ResultFailed, fix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			run.CheckDigest, run.Token, run.Seq = c.Digest, "t1", 3
			pv := &v1alpha1.PreviewVerification{
				Spec: v1alpha1.PreviewVerificationSpec{Rung: tc.rung, Checks: stored},
				Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{run},
					Ledger: &v1alpha1.LedgerSummary{Count: 5}},
			}
			j := judgeRound(acc, pv, stored, tc.ev)
			d := j.decision
			if got := d.PerOutcome[0]; got.Result != tc.result {
				t.Fatalf("outcome = %+v, want %s", got, tc.result)
			}
			if (d.Fix != nil) != tc.fix || d.Suspected != nil || d.Handoff == acceptance.HandoffReady {
				t.Fatalf("decision = %+v: want a fix %v, no suspected defect, not ready", d, tc.fix)
			}
			if tc.rung != v1alpha1.RungReplay {
				return
			}
			nv := &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed, Round: 2,
				HeadSetDigest: "h", Outcomes: []v1alpha1.OutcomeResult{{ID: o.ID, Result: v1alpha1.OutcomeUnsure}}}
			recordLadder(nv, acc, j)
			if e := nv.Escalation; e == nil || e.Round != 2 || e.HeadSetDigest != "h" {
				t.Errorf("escalation %+v, want the head set's full round asked for", e)
			}
		})
	}
}

// shownNoteCheck is a second check of the same outcome, by another route.
const shownNoteCheck = `{"id":"note-shown","outcome":"note-listed","form":"browser","browser":{"steps":[` +
	`{"goto":"/notes/new"},{"fill":{"locator":{"testid":"name"},"text":"{{run}}-s"}},` +
	`{"click":{"role":"button","name":"Save"}},` +
	`{"expect":{"assert":"text","locator":{"testid":"notes"},"text":{"contains":"{{run}}-s"}}}]}}`

// TestBrowserCheckErrorBesideAPassingCheck (code review 2026-11-18, relay
// finding 9): a browser check that erred for maul's own reason is no
// evidence either way, as every check error is. Beside another proven
// check of the same outcome that passed cleanly it neither fails the
// outcome nor starts a fix, and the outcome passes on the other check
// only while the evaluator saw nothing fail: its fail verdict, or a pass
// naming contrary entries, withholds the pass (unsure).
func TestBrowserCheckErrorBesideAPassingCheck(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: "note-listed", Outcome: "A saved note is listed"}
	erred, passed := savedCheck(t, namedNoteCheck), savedCheck(t, shownNoteCheck)
	review := func(c v1alpha1.SavedCheck) v1alpha1.CheckReviewRecord {
		return v1alpha1.CheckReviewRecord{CheckDigest: c.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: c.ID, Reason: "yes"}
	}
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}, OutcomesDigest: "sha256:o",
		Reviews: []v1alpha1.CheckReviewRecord{review(erred), review(passed)}}
	stored := []v1alpha1.SavedCheck{erred, passed}
	for _, tc := range []struct {
		name    string
		verdict report.OutcomeVerdict
		result  acceptance.Result
		reason  string
	}{
		{name: "the evaluator passes the outcome",
			verdict: report.OutcomeVerdict{ID: o.ID, Verdict: report.VerdictPass, Ledger: []int32{4}},
			result:  acceptance.ResultPassed, reason: acceptance.ReasonConfirmed},
		{name: "the evaluator fails the outcome",
			verdict: report.OutcomeVerdict{ID: o.ID, Verdict: report.VerdictFail, Ledger: []int32{3}},
			result:  acceptance.ResultUnsure, reason: acceptance.ReasonEvaluatorSawFailure},
		{name: "the evaluator passes it but names contrary entries",
			verdict: report.OutcomeVerdict{ID: o.ID, Verdict: report.VerdictPass, Ledger: []int32{4},
				Contrary: []int32{3}},
			result: acceptance.ResultUnsure, reason: acceptance.ReasonEvaluatorSawFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pv := &v1alpha1.PreviewVerification{
				Spec: v1alpha1.PreviewVerificationSpec{Rung: v1alpha1.RungFull, Checks: stored},
				Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{
					{CheckDigest: erred.Digest, Token: "t1", Seq: 3, Result: v1alpha1.CheckRunError},
					{CheckDigest: passed.Digest, Token: "t2", Seq: 4, Result: v1alpha1.CheckRunPass},
				}, Ledger: &v1alpha1.LedgerSummary{Count: 6}},
			}
			d := judgeRound(acc, pv, stored, &report.Evaluation{Verdicts: []report.OutcomeVerdict{tc.verdict}}).decision
			if got := d.PerOutcome[0]; got.Result != tc.result || got.Reason != tc.reason {
				t.Fatalf("outcome = %+v, want %s %s", got, tc.result, tc.reason)
			}
			if d.Fix != nil || d.Suspected != nil {
				t.Fatalf("decision = %+v: want no fix and no suspected defect", d)
			}
		})
	}
}
