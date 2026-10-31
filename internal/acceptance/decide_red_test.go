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
			ExpectFail: true}},
		Confirmations: []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-c", Seq: 31,
			Assertion: true, ExpectFail: true}},
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
					Assertion: true}}}
			}), result: ResultFailed, reason: ReasonReproduced, fix: true},
		{name: "red, not by assertion (a timeout, a check error): unsure",
			in:     with(redFound(), func(in *Input) { in.Confirmations[0].Assertion = false }),
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
				in.Confirmations[0].Result, in.Confirmations[0].Assertion = RunError, false
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
		{name: "after the fix, still red on replay with the evaluator's fail: failed again",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, Assertion: true}}
				in.Confirmations = nil
			}), result: ResultFailed, reason: ReasonReproduced, fix: true},
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
			if tc.fix && (len(d.Fix.Failures) != 1 || d.Fix.Failures[0].CheckDigest != dg("red")) {
				t.Errorf("fix failures %+v, want the red check's", d.Fix.Failures)
			}
			if d.Handoff == HandoffReady && tc.result != ResultPassed {
				t.Errorf("hand-off ready on %s", tc.result)
			}
		})
	}
}
