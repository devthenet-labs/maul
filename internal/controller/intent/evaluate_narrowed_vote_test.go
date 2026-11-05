// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

// TestNarrowedCheckIsNotAVoteNextRound (S1 demo bug 14, intent #61): round
// 2 found a defect off a narrow green check's path. The evaluator reported
// fail with contrary entries; its red check was confirmed red by assertion
// and reviewed proves on the red question, while the round-1 check replayed
// green. That round is checks-disagree (two vetted checks disagree, finding
// 3) and records the green check as narrowed. Round 3 at the same heads,
// judged alike but for the narrowing, no longer counts the narrowed check as
// a vote: the outcome is a suspected defect, held for a person to confirm,
// never failed on its own and never passed.
func TestNarrowedCheckIsNotAVoteNextRound(t *testing.T) {
	digest := func(digit string) string { return "sha256:" + strings.Repeat(digit, 64) }
	outcome := v1alpha1.AcceptanceOutcome{ID: "steps-on-remix",
		Outcome: "A remix's stepsOn equals the original's"}
	green := v1alpha1.SavedCheck{ID: "steps-on-remix", Outcome: outcome.ID, Form: v1alpha1.CheckFormHTTP,
		Body: `{}`, Digest: digest("5")}
	red := v1alpha1.SavedCheck{ID: "steps-on-remix-hatheavy-api", Outcome: outcome.ID,
		Form: v1alpha1.CheckFormHTTP, Body: `{}`, Digest: digest("6"), Expect: v1alpha1.CheckExpectFail,
		FailedStep: 4, FailedAssertion: "json /stepsOn"}
	headSet := digest("3")
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{outcome},
		Reviews: []v1alpha1.CheckReviewRecord{
			{CheckDigest: green.Digest, OutcomeDigest: outcomeDigest(outcome), Answer: v1alpha1.ReviewProves,
				Run: "x-cr-r1-a1"},
			{CheckDigest: red.Digest, OutcomeDigest: outcomeDigest(outcome), Answer: v1alpha1.ReviewProves,
				Expect: v1alpha1.CheckExpectFail, FailedStep: 4, FailedAssertion: "json /stepsOn", Run: "x-cr-r2-a1"},
		}}
	in := &v1alpha1.Intent{}
	in.Name = "x"
	in.Status.Acceptance = acc
	in.Status.Verification = &v1alpha1.IntentVerification{HeadSetDigest: headSet}
	ev := &report.Evaluation{Verdicts: []report.OutcomeVerdict{{ID: outcome.ID, Verdict: report.VerdictFail,
		Ledger: []int32{19, 28}, Contrary: []int32{13, 14, 15, 16, 17, 19}}}}
	greenRun := v1alpha1.CheckRun{CheckDigest: green.Digest, Result: v1alpha1.CheckRunPass, Token: "t1", Seq: 3}
	redRun := v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail, Token: "t2", Seq: 28,
		AssertionStep: 4, AssertionID: "json /stepsOn"}
	now := time.Now()

	round2 := &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Rung: v1alpha1.RungFull, HeadSetDigest: headSet,
			Checks: []v1alpha1.SavedCheck{green}},
		Status: v1alpha1.PreviewVerificationStatus{Ledger: &v1alpha1.LedgerSummary{Count: 30},
			Confirmed: []v1alpha1.SavedCheck{red}, Replay: []v1alpha1.CheckRun{greenRun}},
	}
	round2.Name = "x-v2-a1"
	confirm := redRun
	confirm.Expect = v1alpha1.CheckExpectFail
	round2.Status.Confirmations = []v1alpha1.CheckRun{confirm}
	stored := []v1alpha1.SavedCheck{green, red}
	applyJudgement(in, judgeRound(acc, round2, stored, ev), now)
	v := in.Status.Verification
	if o := v.Outcomes[0]; o.Result != v1alpha1.OutcomeUnsure || o.Reason != acceptance.ReasonChecksDisagree {
		t.Fatalf("round 2: %+v, want unsure, %s", o, acceptance.ReasonChecksDisagree)
	}
	if v.Suspected != nil {
		t.Fatalf("round 2: suspected %+v, want none", v.Suspected)
	}
	if got := acc.Narrowed; len(got) != 1 || got[0].Digest != green.Digest || got[0].Verification != round2.Name {
		t.Fatalf("round 2: narrowed %+v, want %s recorded by %s", got, green.Digest, round2.Name)
	}

	round3 := &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Rung: v1alpha1.RungFull, HeadSetDigest: headSet,
			Checks: []v1alpha1.SavedCheck{green, red}},
		Status: v1alpha1.PreviewVerificationStatus{Ledger: &v1alpha1.LedgerSummary{Count: 30},
			Replay: []v1alpha1.CheckRun{greenRun, redRun}},
	}
	round3.Name = "x-v3-a1"
	applyJudgement(in, judgeRound(acc, round3, stored, ev), now)
	v = in.Status.Verification
	if o := v.Outcomes[0]; o.Result != v1alpha1.OutcomeUnsure || o.Reason != acceptance.ReasonSuspectedDefect {
		t.Fatalf("round 3: %+v, want unsure, %s", o, acceptance.ReasonSuspectedDefect)
	}
	if v.State == v1alpha1.VerificationPassed || v.State == v1alpha1.VerificationFailing {
		t.Errorf("round 3: state %s, want neither passed nor failing (a person confirms)", v.State)
	}
	if s := v.Suspected; s == nil || s.Verification != round3.Name || !slices.Equal(s.Outcomes, []string{outcome.ID}) ||
		s.Confirmed != nil {
		t.Errorf("round 3: suspected %+v, want %s recorded by %s, unconfirmed", s, outcome.ID, round3.Name)
	}
}
