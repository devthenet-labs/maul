// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import "testing"

// redFound is a defect found in round 1 (S1 bug 7): the evaluator reports
// fail citing the ledger, and proposes a check that shows it; tools
// confirmed it red by assertion in a fresh context, the reviewer answered
// the red question proves, and the check is stored expecting to fail.
func redFound() Input {
	return Input{
		Outcomes: []OutcomeRef{{ID: "elo", Digest: od("elo")}},
		Checks: []StoredCheck{{Digest: dg("red"), Outcome: "elo", Form: CheckFormHTTP, Valid: true,
			ExpectFail: true, FailedStep: 2}},
		Confirmations: []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-c", Seq: 31,
			AssertionStep: 2, ExpectFail: true}},
		Reviews: []CheckReview{{CheckDigest: dg("red"), OutcomeDigest: od("elo"), Answer: ReviewProves,
			ExpectFail: true}},
		Verdicts: []EvaluatorVerdict{{Outcome: "elo", Claim: ClaimFail, Ledger: []int32{12, 14}}},
	}
}

// TestDecideRedCheck: the guards on a check saved to show a defect. Only a
// confirmation red by assertion, a red review proves and the evaluator's
// cited fail make the outcome failed (a verify-fix round with the
// reproduction); anything less leaves it unsure, and a check proposed red
// that then ran green never passes it.
func TestDecideRedCheck(t *testing.T) {
	tests := []struct {
		name   string
		in     Input
		result Result
		reason string
		fix    bool
	}{
		{name: "red by assertion, reviewed, the evaluator's fail: failed", in: redFound(),
			result: ResultFailed, reason: ReasonReproduced, fix: true},
		{name: "a base where the new outcome fails too is recorded, never a refusal",
			in: with(redFound(), func(in *Input) {
				in.Base = &BaseRuns{Runs: []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-b", Seq: 40,
					AssertionStep: 2}}}
			}), result: ResultFailed, reason: ReasonReproduced, fix: true},
		{name: "red, not by assertion (a timeout, a check error): unsure",
			in:     with(redFound(), func(in *Input) { in.Confirmations[0].AssertionStep = 0 }),
			result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "the confirmation ran green (flaky): unsure, never passed",
			in: with(redFound(), func(in *Input) {
				in.Confirmations[0] = CheckRun{CheckDigest: dg("red"), Result: RunPass, Token: "tok-c", Seq: 31, ExpectFail: true}
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "the confirmation failed then passed: unsure",
			in:     with(redFound(), func(in *Input) { in.Confirmations[0].Result = RunQuarantined }),
			result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "the confirmation errored: unsure",
			in: with(redFound(), func(in *Input) {
				in.Confirmations[0].Result, in.Confirmations[0].AssertionStep = RunError, 0
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "the reviewer answered no: unsure",
			in:     with(redFound(), func(in *Input) { in.Reviews[0].Answer = ReviewDoesNotProve }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "reviewed only with the green question: unsure",
			in:     with(redFound(), func(in *Input) { in.Reviews[0].ExpectFail = false }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "the evaluator does not report fail: unsure",
			in:     with(redFound(), func(in *Input) { in.Verdicts[0].Claim = ClaimUnsure }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "a fail citing nothing: unsure",
			in:     with(redFound(), func(in *Input) { in.Verdicts[0].Ledger = nil }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "a failing environment: unsure",
			in: with(redFound(), func(in *Input) {
				in.Env = []PartFact{{Name: "api", Role: RoleComponent, Ready: true, CrashedDuring: true}}
			}), result: ResultUnsure, reason: ReasonEnvFailed},
		{name: "after the fix the saved check replays green: passed",
			in: with(redFound(), func(in *Input) {
				in.Confirmations = nil
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-r", Seq: 2}}
				in.Verdicts[0] = EvaluatorVerdict{Outcome: "elo", Claim: ClaimPass, Ledger: []int32{2}}
			}), result: ResultPassed, reason: ReasonConfirmed},
		{name: "after the fix, re-proposed and confirmed green beside its green replay: passed",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-r", Seq: 2}}
				in.Confirmations = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-c", Seq: 9}}
				in.Verdicts[0] = EvaluatorVerdict{Outcome: "elo", Claim: ClaimPass, Ledger: []int32{2}}
			}), result: ResultPassed, reason: ReasonConfirmed},
		{name: "after the fix, still red by assertion at its saved step, the evaluator's fail: failed again",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, AssertionStep: 2}}
				in.Confirmations = nil
			}), result: ResultFailed, reason: ReasonReproduced, fix: true},
		// Review finding 2: until a check saved red has replayed green once,
		// its steps after the saved one never ran: only a red by assertion at
		// or before that step reproduces the defect.
		{name: "after the fix, a replay fail that is no assertion: unsure",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2}}
				in.Confirmations = nil
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "after the fix, red by assertion at a step after the saved one: unsure",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, AssertionStep: 3}}
				in.Confirmations = nil
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "once it replayed green, a later regression fails as any saved check's",
			in: with(redFound(), func(in *Input) {
				in.Checks[0].Cleared = true
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2}}
				in.Confirmations = nil
			}), result: ResultFailed, reason: ReasonReproduced, fix: true},
		// Review finding 3: two vetted checks disagreeing is unsure.
		{name: "a red confirmation beside a green replay of the same check: unsure",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-r", Seq: 2}}
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "a red confirmation beside a proven green check passing: unsure",
			in: with(redFound(), func(in *Input) {
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("green"), Outcome: "elo", Form: CheckFormHTTP,
					Valid: true})
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("green"), OutcomeDigest: od("elo"),
					Answer: ReviewProves})
				in.Replay = []CheckRun{{CheckDigest: dg("green"), Result: RunPass, Token: "tok-r", Seq: 2}}
			}), result: ResultUnsure, reason: ReasonChecksDisagree},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.in)
			r := d.PerOutcome[0]
			if r.Result != tc.result || r.Reason != tc.reason {
				t.Fatalf("result %s (%s), want %s (%s)", r.Result, r.Reason, tc.result, tc.reason)
			}
			if (d.Fix != nil) != tc.fix {
				t.Fatalf("fix %+v, want %v", d.Fix, tc.fix)
			}
			if !tc.fix && d.Fix != nil {
				t.Fatalf("fix %+v on %s", d.Fix, tc.result)
			}
			if tc.fix && (len(d.Fix.Failures) != 1 || d.Fix.Failures[0].CheckDigest != dg("red")) {
				t.Errorf("fix failures %+v, want the red check's", d.Fix.Failures)
			}
			if d.Handoff == HandoffReady && tc.result != ResultPassed {
				t.Errorf("hand-off ready on %s", tc.result)
			}
		})
	}
}
