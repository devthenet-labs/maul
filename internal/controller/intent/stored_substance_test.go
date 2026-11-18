// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

// postedCheck writes data of its own run and asserts only that the call
// worked: the substance rules refuse it, and a failure of a check that
// writes run data would otherwise start an automatic fix.
const postedCheck = `{"id":"version-shown.posted","outcome":"version-shown","form":"http","http":{"target":"app",
 "steps":[{"request":{"method":"POST","path":"/notes","json":{"name":"{{run}}-v"}},"expect":{"status":201}}]}}`

// TestStoredCheckTheSubstanceRulesRefuseSupportsNothing (code review
// 2026-11-18, checks 6): a check stored under earlier rules that the
// saved-check substance rules now refuse (checks.Substantive) is replayed
// as it was stored, by its digest, but supports nothing: green it passes
// no outcome, red it fails none and asks for no fix, even with the
// evaluator's cited fail, and the next round is full, so an evaluator can
// save a check that asserts something.
func TestStoredCheckTheSubstanceRulesRefuseSupportsNothing(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	vacuous := savedCheck(t, postedCheck)
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}, OutcomesDigest: "sha256:o",
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: vacuous.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: vacuous.ID, Reason: "yes"}}}
	stored := []v1alpha1.SavedCheck{vacuous}
	if store := storeInput(acc, stored, ""); store.Checks[0].Valid {
		t.Fatal("a stored check the substance rules refuse is valid to Decide")
	}
	fail := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: oVersion, Verdict: report.VerdictFail,
		Ledger: []int32{1}}}}
	for _, tc := range []struct {
		name   string
		result v1alpha1.CheckRunResult
		ev     *report.Evaluation
		rung   v1alpha1.VerificationRung
	}{
		{name: "green on replay", result: v1alpha1.CheckRunPass, rung: v1alpha1.RungReplay},
		{name: "green in a full round", result: v1alpha1.CheckRunPass, rung: v1alpha1.RungFull},
		{name: "red in a full round the evaluator fails", result: v1alpha1.CheckRunFail, ev: fail,
			rung: v1alpha1.RungFull},
		{name: "red on replay", result: v1alpha1.CheckRunFail, rung: v1alpha1.RungReplay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := v1alpha1.CheckRun{CheckDigest: vacuous.Digest, Result: tc.result, Token: "t1", Seq: 1}
			if tc.result == v1alpha1.CheckRunFail {
				run.AssertionStep = 1
			}
			pv := &v1alpha1.PreviewVerification{
				Spec: v1alpha1.PreviewVerificationSpec{Rung: tc.rung, Checks: stored},
				Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{run},
					Ledger: &v1alpha1.LedgerSummary{Count: 5}},
			}
			d := judgeRound(acc, pv, stored, tc.ev).decision
			if got := d.PerOutcome[0]; got.Result != acceptance.ResultUnsure {
				t.Fatalf("outcome = %+v, want unsure", got)
			}
			if d.Fix != nil || d.Suspected != nil || d.Handoff == acceptance.HandoffReady {
				t.Fatalf("decision = %+v: no fix, no suspected defect, not ready", d)
			}
		})
	}
	store := storeInput(acc, stored, "")
	rung, _ := acceptance.StartRung(acceptance.RungInput{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest,
		Model:    &acceptance.ModelRound{HeadSet: "h", OutcomesDigest: acc.OutcomesDigest, Clear: true},
		Outcomes: store.Outcomes, Checks: store.Checks, Reviews: store.Reviews})
	if rung != acceptance.RungFull {
		t.Fatalf("rung = %s, want full: the outcome has no stored check a replay could pass it on", rung)
	}
}
