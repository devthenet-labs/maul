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
