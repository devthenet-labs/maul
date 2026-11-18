// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/httpstep"
)

// Code review 2026-11-18, judge finding 1: an evaluator that saw an outcome
// fail at heads H (a fail verdict, or contrary entries) made a finding about
// H whatever reason Decide recorded for the outcome. Decide records
// no-base-evidence, not-replayed, quarantined and the others before (or
// instead of) evaluator-saw-failure, and the hold must not rest on which.

// objectionInput is a Decide input over the three outcomes of
// passingJudgement: still-healthy passes on its own check, the manual one
// is a person's, and version-shown's check, with the evaluator's verdict on
// it a fail (objected) or a pass, is shaped by mutate.
func objectionInput(t *testing.T, objected bool, mutate func(in *acceptance.Input, version string)) acceptance.Input {
	t.Helper()
	version, health := savedCheck(t, versionCheck).Digest, savedCheck(t, healthCheck).Digest
	verdict := acceptance.EvaluatorVerdict{Outcome: oVersion, Claim: acceptance.ClaimPass, Ledger: []int32{1}}
	if objected {
		verdict.Claim = acceptance.ClaimFail
	}
	in := acceptance.Input{
		Outcomes: []acceptance.OutcomeRef{{ID: oVersion, Digest: "ov"}, {ID: oHealthy, Digest: "oh"},
			{ID: oManual, Digest: "om", Manual: true}},
		Checks: []acceptance.StoredCheck{
			{Digest: version, Outcome: oVersion, Form: acceptance.CheckFormHTTP, Valid: true},
			{Digest: health, Outcome: oHealthy, Form: acceptance.CheckFormHTTP, Valid: true}},
		Replay: []acceptance.CheckRun{
			{CheckDigest: version, Result: acceptance.RunPass, Token: "tok", Seq: 1},
			{CheckDigest: health, Result: acceptance.RunPass, Token: "tok", Seq: 2}},
		Reviews: []acceptance.CheckReview{
			{CheckDigest: version, OutcomeDigest: "ov", Answer: acceptance.ReviewProves},
			{CheckDigest: health, OutcomeDigest: "oh", Answer: acceptance.ReviewProves}},
		Verdicts: []acceptance.EvaluatorVerdict{verdict,
			{Outcome: oHealthy, Claim: acceptance.ClaimPass, Ledger: []int32{2}}},
	}
	mutate(&in, version)
	return in
}

// replayOf sets digest's replay runs in in.
func replayOf(in *acceptance.Input, digest string, runs ...acceptance.CheckRun) {
	kept := in.Replay[:0]
	for _, r := range in.Replay {
		if r.CheckDigest != digest {
			kept = append(kept, r)
		}
	}
	for _, r := range runs {
		r.CheckDigest = digest
		kept = append(kept, r)
	}
	in.Replay = kept
}

// secondVersion is a second stored, proven check of version-shown.
func secondVersion(in *acceptance.Input) string {
	second := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	in.Checks = append(in.Checks, acceptance.StoredCheck{Digest: second, Outcome: oVersion,
		Form: acceptance.CheckFormHTTP, Valid: true})
	in.Reviews = append(in.Reviews, acceptance.CheckReview{CheckDigest: second, OutcomeDigest: "ov",
		Answer: acceptance.ReviewProves})
	return second
}

// maskedReasons are the rounds whose evaluator, objecting, sees Decide
// record another reason for version-shown than evaluator-saw-failure.
var maskedReasons = []struct {
	reason string
	mutate func(in *acceptance.Input, version string)
}{
	{acceptance.ReasonNoBaseEvidence, func(in *acceptance.Input, version string) {
		// The check passes at base too; the health check is red there.
		in.Base = &acceptance.BaseRuns{Required: true, Runs: []acceptance.CheckRun{
			{CheckDigest: version, Result: acceptance.RunPass, Token: "tok-b"},
			{CheckDigest: in.Checks[1].Digest, Result: acceptance.RunFail, Token: "tok-b", AssertionStep: 1,
				AssertionID: "json /status"}}}
	}},
	{acceptance.ReasonNotReplayed, func(in *acceptance.Input, version string) {
		in.ReplayRequired = []string{version, secondVersion(in)}
	}},
	{acceptance.ReasonQuarantined, func(in *acceptance.Input, version string) {
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunQuarantined, Token: "tok", Seq: 1})
	}},
	{acceptance.ReasonFlaky, func(in *acceptance.Input, version string) {
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunPass, Token: "tok", Seq: 1},
			acceptance.CheckRun{Result: acceptance.RunFail, Token: "tok", Seq: 3, AssertionStep: 2,
				AssertionID: "json /note"})
	}},
	{acceptance.ReasonChecksDisagree, func(in *acceptance.Input, version string) {
		second := secondVersion(in)
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunFail, Token: "tok", Seq: 1,
			AssertionStep: 2, AssertionID: "json /note"})
		in.Replay = append(in.Replay, acceptance.CheckRun{CheckDigest: second, Result: acceptance.RunPass,
			Token: "tok", Seq: 4})
	}},
	{acceptance.ReasonEvaluatorDisagrees, func(in *acceptance.Input, version string) {
		// A bare fail the evaluator cites no entry for.
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunFail, Token: "tok", Seq: 1})
		in.Verdicts[0].Ledger = nil
	}},
	{acceptance.ReasonNotAsserted, func(in *acceptance.Input, version string) {
		in.Checks[0].Form = acceptance.CheckFormExec
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunFail, Token: "tok", Seq: 1})
	}},
	{acceptance.ReasonNotReproduced, func(in *acceptance.Input, version string) {
		// Saved red, its replay red by another assertion: not counted.
		in.Checks[0].ExpectFail, in.Checks[0].FailedStep, in.Checks[0].FailedAssertion = true, 2, "json /note"
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunFail, Token: "tok", Seq: 1,
			AssertionStep: 2, AssertionID: "json /other"})
	}},
	{acceptance.ReasonEnvFailed, func(in *acceptance.Input, version string) {
		replayOf(in, version, acceptance.CheckRun{Result: acceptance.RunFail, Token: "tok", Seq: 1,
			AssertionStep: 2, AssertionID: "json /note"})
		in.Env = []httpstep.PartFact{{Name: "api", Role: httpstep.RoleComponent, Ready: true, CrashedDuring: true}}
	}},
}

// objectionJudgement is passingJudgement with Decide's own decision over in.
func objectionJudgement(t *testing.T, in acceptance.Input) roundJudgement {
	t.Helper()
	j := passingJudgement(t)
	j.decision = acceptance.Decide(in)
	return j
}

// TestEvaluatorObjectionHoldsItsHeads: an outcome the evaluator saw fail at
// H is held at H in every later round there, whatever reason Decide gave it;
// the same round with the evaluator not objecting holds nothing (no false
// fail), and moved heads release it.
func TestEvaluatorObjectionHoldsItsHeads(t *testing.T) {
	for _, tt := range maskedReasons {
		t.Run(tt.reason, func(t *testing.T) {
			first := objectionJudgement(t, objectionInput(t, true, tt.mutate))
			if got := first.decision.PerOutcome[0].Reason; got != tt.reason {
				t.Fatalf("round 1 reason %q, want %q (the case's premise)", got, tt.reason)
			}
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			heads := e.verification(name).HeadSetDigest
			e.judgeAt(name, 1, heads, first)
			v := e.judgeAt(name, 2, heads, passingJudgement(t))
			if v.State == v1alpha1.VerificationPassed || v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
				t.Errorf("round 2 at H after an objection (%s): %s %+v; want the outcome held", tt.reason, v.State,
					v.Outcomes[0])
			}
			if !openFindingAt(v, e.prVerifyPass(name).in.Status.Acceptance, heads) {
				t.Error("no open finding: a replay round could follow")
			}
			if v := e.judgeAt(name, 3, "sha256:moved", passingJudgement(t)); v.State != v1alpha1.VerificationPassed {
				t.Errorf("round 3 at moved heads: %s %+v; want Passed", v.State, v.Outcomes[0])
			}
		})
		t.Run(tt.reason+" without an objection", func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			heads := e.verification(name).HeadSetDigest
			e.judgeAt(name, 1, heads, objectionJudgement(t, objectionInput(t, false, tt.mutate)))
			if v := e.judgeAt(name, 2, heads, passingJudgement(t)); v.State != v1alpha1.VerificationPassed {
				t.Errorf("round 2 at H with no objection in round 1: %s %+v; want Passed", v.State, v.Outcomes[0])
			}
		})
	}
}

// TestEvaluatorObjectionOverAHeldCheckIsItsOwn: an outcome held on a check
// round 1 found failing, which round 2's evaluator saw fail again under
// another reason, rests from then on on round 2's objection too: the check
// retired later does not release it.
func TestEvaluatorObjectionOverAHeldCheckIsItsOwn(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	check := savedCheck(t, versionCheck)
	first := passingJudgement(t)
	first.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced,
		check.Digest)
	e.judgeAt(name, 1, heads, first)

	second := objectionJudgement(t, objectionInput(t, true, maskedReasons[0].mutate))
	second.stored = []v1alpha1.SavedCheck{check}
	e.judgeAt(name, 2, heads, second)

	gone := passingJudgement(t)
	gone.stored = []v1alpha1.SavedCheck{savedCheck(t, healthCheck)}
	if v := e.judgeAt(name, 3, heads, gone); v.State == v1alpha1.VerificationPassed ||
		v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
		t.Errorf("round 3 with the check retired after round 2 objected: %s %+v; want held", v.State, v.Outcomes[0])
	}
}

// TestJudgingAnObjectedRoundAgainIsNotHeldByIt: the same round judged
// again is that round's judgement written anew: its own earlier objection
// does not hold it, only an earlier round's does.
func TestJudgingAnObjectedRoundAgainIsNotHeldByIt(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	e.judgeAt(name, 1, heads, objectionJudgement(t, objectionInput(t, true, maskedReasons[0].mutate)))
	if v := e.judgeAt(name, 1, heads, passingJudgement(t)); v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 1 judged again is held by its own objection: %s %+v", v.State, v.Outcomes[0])
	}
}

// TestContraryEntriesAloneHoldTheHeads: an objection is a fail verdict or
// contrary entries; a pass verdict naming contrary entries, under a masked
// reason, holds its heads as a fail verdict does.
func TestContraryEntriesAloneHoldTheHeads(t *testing.T) {
	in := objectionInput(t, false, maskedReasons[0].mutate)
	in.Verdicts[0].Contrary = []int32{7}
	first := objectionJudgement(t, in)
	if got := first.decision.PerOutcome[0].Reason; got != acceptance.ReasonNoBaseEvidence {
		t.Fatalf("round 1 reason %q, want no-base-evidence (the case's premise)", got)
	}
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	e.judgeAt(name, 1, heads, first)
	if v := e.judgeAt(name, 2, heads, passingJudgement(t)); v.State == v1alpha1.VerificationPassed ||
		v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
		t.Errorf("round 2 at H after contrary entries: %s %+v; want the outcome held", v.State, v.Outcomes[0])
	}
}
