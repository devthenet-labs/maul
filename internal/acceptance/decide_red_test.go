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
			ExpectFail: true, FailedStep: 2, FailedAssertion: "json /stake"}},
		Confirmations: []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-c", Seq: 31,
			AssertionStep: 2, AssertionID: "json /stake", ExpectFail: true}},
		Reviews: []CheckReview{{CheckDigest: dg("red"), OutcomeDigest: od("elo"), Answer: ReviewProves,
			ExpectFail: true, FailedStep: 2, FailedAssertion: "json /stake"}},
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
		{name: "after the fix the saved check replays green, reviewed with the green question too: passed",
			in: with(redFound(), func(in *Input) {
				in.Confirmations = nil
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-r", Seq: 2}}
				in.Verdicts[0] = EvaluatorVerdict{Outcome: "elo", Claim: ClaimPass, Ledger: []int32{2}}
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("red"), OutcomeDigest: od("elo"),
					Answer: ReviewProves})
			}), result: ResultPassed, reason: ReasonConfirmed},
		// Third review, F5: the red review asked the red question only; a
		// pass needs the green one too.
		{name: "after the fix the saved check replays green with only its red review: unsure",
			in: with(redFound(), func(in *Input) {
				in.Confirmations = nil
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-r", Seq: 2}}
				in.Verdicts[0] = EvaluatorVerdict{Outcome: "elo", Claim: ClaimPass, Ledger: []int32{2}}
			}), result: ResultUnsure, reason: ReasonModelOnlyPass},
		{name: "after the fix, re-proposed and confirmed green beside its green replay: passed",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-r", Seq: 2}}
				in.Confirmations = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-c", Seq: 9}}
				in.Verdicts[0] = EvaluatorVerdict{Outcome: "elo", Claim: ClaimPass, Ledger: []int32{2}}
				in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("red"), OutcomeDigest: od("elo"),
					Answer: ReviewProves})
			}), result: ResultPassed, reason: ReasonConfirmed},
		{name: "after the fix, still red by its saved assertion, the evaluator's fail: failed again",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, AssertionStep: 2,
					AssertionID: "json /stake"}}
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
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, AssertionStep: 3,
					AssertionID: "json /stake"}}
				in.Confirmations = nil
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		// Second review, 2B: the unit is the assertion, not the step.
		{name: "after the fix, red by another assertion of the saved step: unsure",
			in: with(redFound(), func(in *Input) {
				in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, AssertionStep: 2,
					AssertionID: "header x-stake"}}
				in.Confirmations = nil
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		// Second review, 2A: re-confirming the stored check is judged as its
		// replay is; a red review counts only for the assertion it judged.
		{name: "re-confirmed red at another step: unsure",
			in: with(redFound(), func(in *Input) {
				in.Confirmations[0].AssertionStep, in.Confirmations[0].AssertionID = 3, "json /other"
			}), result: ResultUnsure, reason: ReasonNotReproduced},
		{name: "a red review of another assertion: unsure",
			in: with(redFound(), func(in *Input) {
				in.Reviews[0].FailedStep, in.Reviews[0].FailedAssertion = 3, "json /other"
			}), result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "once cleared, re-confirmed red: unsure",
			in:     with(redFound(), func(in *Input) { in.Checks[0].Cleared = true }),
			result: ResultUnsure, reason: ReasonNotReproduced},
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

// TestDecideRedCheckDeadEnd (second review): a check saved red whose replay
// fails another way than its saved assertion is not evidence, never a dead
// end: a dispute with a proven green replacement still retires it (a
// replay round follows), and another proven green check passing passes the
// outcome.
func TestDecideRedCheckDeadEnd(t *testing.T) {
	dropped := with(redFound(), func(in *Input) {
		in.Confirmations = nil
		in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2, AssertionStep: 2,
			AssertionID: "header x-stake"}}
		in.Verdicts[0] = EvaluatorVerdict{Outcome: "elo", Claim: ClaimUnsure}
	})
	green := func(in *Input) {
		in.Checks = append(in.Checks, StoredCheck{Digest: dg("green"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("green"), OutcomeDigest: od("elo"),
			Answer: ReviewProves})
	}
	disputed := with(dropped, func(in *Input) {
		green(in)
		in.Confirmations = []CheckRun{{CheckDigest: dg("green"), Result: RunPass, Token: "tok-c", Seq: 9}}
		in.Disputes = []CheckDispute{{CheckDigest: dg("red"), Reason: "wrong check", Replacement: dg("green"),
			Ledger: []int32{2}}}
	})
	d := Decide(disputed)
	if d.PerOutcome[0].Result != ResultUnsure || d.PerOutcome[0].Reason != ReasonDisputed || !d.ReplayRound ||
		len(d.Retire) != 1 || d.Retire[0] != dg("red") {
		t.Errorf("disputed: %+v, retire %v, replay %v; want the red check retired for its replacement",
			d.PerOutcome[0], d.Retire, d.ReplayRound)
	}
	passing := with(dropped, func(in *Input) {
		green(in)
		in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("green"), Result: RunPass, Token: "tok-r", Seq: 3})
	})
	if r := Decide(passing).PerOutcome[0]; r.Result != ResultPassed || r.Basis != dg("green") {
		t.Errorf("beside a proven green pass: %+v, want passed on the green check", r)
	}
	if d := Decide(passing); d.Fix != nil {
		t.Errorf("fix %+v from a fail that is not evidence", d.Fix)
	}
}

// TestDecideFlakyCheck (third review, F4): a check that both passed and
// failed at the same heads in one round is flaky: unsure, never failed (and
// never passed), green-proposed or red alike.
func TestDecideFlakyCheck(t *testing.T) {
	green := deterministicFail(ClaimFail, 3)
	green.Confirmations = []CheckRun{{CheckDigest: dg("a"), Result: RunPass, Token: "tok-c", Seq: 9}}
	if d := Decide(green); d.PerOutcome[0].Result != ResultUnsure || d.PerOutcome[0].Reason != ReasonFlaky ||
		d.Fix != nil {
		t.Errorf("a green check failing its replay and passing its confirmation: %+v, fix %+v", d.PerOutcome[0], d.Fix)
	}
	red := with(redFound(), func(in *Input) {
		in.Checks[0].Cleared = true
		in.Confirmations = []CheckRun{{CheckDigest: dg("red"), Result: RunPass, Token: "tok-c", Seq: 9}}
		in.Replay = []CheckRun{{CheckDigest: dg("red"), Result: RunFail, Token: "tok-r", Seq: 2}}
	})
	if d := Decide(red); d.PerOutcome[0].Result != ResultUnsure || d.PerOutcome[0].Reason != ReasonFlaky ||
		d.Fix != nil {
		t.Errorf("a saved red check failing its replay and passing its confirmation: %+v, fix %+v", d.PerOutcome[0],
			d.Fix)
	}
}
