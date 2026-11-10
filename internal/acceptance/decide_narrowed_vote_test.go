// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"slices"
	"testing"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// issue61 is S1 demo bug 14's shape, round 3 (intent #61): outcome
// steps-on-remix's round-1 check (one original, hat 2) replays green but was
// narrowed in round 2, where the evaluator saw the outcome fail as worded;
// round 2's red check (hat 6: the remix wipes the hat line), saved failing
// step 4 by json /stepsOn and reviewed proves on the red question, replays
// red by that assertion; the evaluator again reports fail, citing it, with
// contrary entries.
func issue61() Input {
	return Input{
		Outcomes: []OutcomeRef{{ID: "steps-on-remix", Digest: od("remix")}},
		Checks: []StoredCheck{
			{Digest: dg("5487509e"), Outcome: "steps-on-remix", Form: CheckFormHTTP, Valid: true, Narrowed: true},
			{Digest: dg("63e16f80"), Outcome: "steps-on-remix", Form: CheckFormHTTP, Valid: true,
				ExpectFail: true, FailedStep: 4, FailedAssertion: "json /stepsOn"},
		},
		Replay: []CheckRun{
			{CheckDigest: dg("5487509e"), Result: RunPass, Token: "t-green", Seq: 3},
			{CheckDigest: dg("63e16f80"), Result: RunFail, Token: "t-red", Seq: 4, AssertionStep: 4,
				AssertionID: "json /stepsOn"},
		},
		Reviews: []CheckReview{
			{CheckDigest: dg("5487509e"), OutcomeDigest: od("remix"), Answer: ReviewProves},
			{CheckDigest: dg("63e16f80"), OutcomeDigest: od("remix"), Answer: ReviewProves, ExpectFail: true,
				FailedStep: 4, FailedAssertion: "json /stepsOn"},
		},
		Verdicts: []EvaluatorVerdict{{Outcome: "steps-on-remix", Claim: ClaimFail, Ledger: []int32{4, 19},
			Contrary: []int32{13, 14, 15, 16, 17, 19}}},
	}
}

// issue61Round2 is the same intent's round 2, where the red check was
// proposed and confirmed red by assertion, and the green check, not yet
// narrowed, replayed green (this round names it narrowed).
func issue61Round2() Input {
	in := issue61()
	in.Checks[0].Narrowed = false
	in.Replay = in.Replay[:1]
	in.Confirmations = []CheckRun{{CheckDigest: dg("63e16f80"), Result: RunFail, Token: "t-red", Seq: 28,
		AssertionStep: 4, AssertionID: "json /stepsOn", ExpectFail: true}}
	in.Verdicts[0].Ledger = []int32{19, 28}
	return in
}

// provenSibling adds a proven green check of the outcome, never narrowed,
// with one replay of result.
func provenSibling(result RunResult) func(*Input) {
	return func(in *Input) {
		in.Checks = append(in.Checks, StoredCheck{Digest: dg("g2"), Outcome: "steps-on-remix",
			Form: CheckFormHTTP, Valid: true})
		in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("g2"), Result: result, Token: "t-g2", Seq: 5})
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("g2"), OutcomeDigest: od("remix"),
			Answer: ReviewProves})
	}
}

// TestDecideNarrowedCheckIsNotAVote (S1 demo bug 14): a check narrowed in an
// earlier round asserts less than its outcome, so its clean pass is no vote
// that the outcome holds and never makes a failing check of the outcome a
// disagreement (checks-disagree). The failure is then judged by the usual
// rules: a red check confirmed red by its own assertion, reviewed proves on
// the red question, with the evaluator's cited fail, is a suspected defect;
// anything less stays unsure, never failed. A sibling never narrowed that
// passes cleanly still disagrees, as does the narrowing round itself
// (finding 3: the round that names a check narrowed is judged alike every
// time, and the evaluator's word only ever withholds a pass).
func TestDecideNarrowedCheckIsNotAVote(t *testing.T) {
	tests := []struct {
		name      string
		in        Input
		result    Result
		reason    string
		suspected bool
		fix       bool
	}{
		{name: "issue 61 round 3: the narrowed check is no vote, the red check is a suspected defect",
			in: issue61(), result: ResultUnsure, reason: ReasonSuspectedDefect, suspected: true},
		{name: "a red confirmation beside a check narrowed earlier: a suspected defect",
			in:     with(issue61Round2(), func(in *Input) { in.Checks[0].Narrowed = true }),
			result: ResultUnsure, reason: ReasonSuspectedDefect, suspected: true},
		{name: "issue 61 round 2 as it ran: the check this round narrows still disagrees",
			in: issue61Round2(), result: ResultUnsure, reason: ReasonChecksDisagree},
		{name: "a proven sibling never narrowed passing cleanly still disagrees",
			in: with(issue61(), provenSibling(RunPass)), result: ResultUnsure, reason: ReasonChecksDisagree},
		{name: "the red check not red by its saved assertion: no failure, and no pass",
			in:     with(issue61(), func(in *Input) { in.Replay[1].AssertionStep = 0 }),
			result: ResultUnsure, reason: ReasonEvaluatorSawFailure},
		{name: "the red check not reviewed proves: unsure",
			in:     with(issue61(), func(in *Input) { in.Reviews[1].Answer = ReviewDoesNotProve }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "the red check reviewed only on the green question: unsure",
			in:     with(issue61(), func(in *Input) { in.Reviews[1].ExpectFail = false }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "the evaluator says pass: the red check alone is not enough",
			in: with(issue61(), func(in *Input) {
				in.Verdicts = []EvaluatorVerdict{{Outcome: "steps-on-remix", Claim: ClaimPass, Ledger: []int32{3}}}
			}),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "the evaluator's fail cites nothing: unsure",
			in:     with(issue61(), func(in *Input) { in.Verdicts[0].Ledger = nil }),
			result: ResultUnsure, reason: ReasonEvaluatorDisagrees},
		{name: "the evaluator disputes the red check: unsure",
			in: with(issue61(), func(in *Input) {
				in.Disputes = []CheckDispute{{CheckDigest: dg("63e16f80"), Reason: "r", Ledger: []int32{4}}}
			}),
			result: ResultUnsure, reason: ReasonDisputed},
		{name: "a failing environment: unsure",
			in: with(issue61(), func(in *Input) {
				in.Env = []httpstep.PartFact{{Name: "api", Role: httpstep.RoleComponent, Ready: true, CrashedDuring: true}}
			}),
			result: ResultUnsure, reason: ReasonEnvFailed},
		{name: "a proven green check regressing beside the narrowed one, the evaluator agreeing: failed",
			in: with(issue61(), func(in *Input) {
				in.Checks, in.Replay, in.Reviews = in.Checks[:1], in.Replay[:1], in.Reviews[:1]
				provenSibling(RunFail)(in)
				in.Verdicts[0].Ledger = []int32{5}
			}),
			result: ResultFailed, reason: ReasonReproduced, fix: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			r := d.PerOutcome[0]
			if r.Result != tt.result || r.Reason != tt.reason {
				t.Errorf("got %s/%s, want %s/%s", r.Result, r.Reason, tt.result, tt.reason)
			}
			if (d.Suspected != nil) != tt.suspected || (d.Fix != nil) != tt.fix {
				t.Errorf("suspected %+v, fix %+v; want suspected %v, fix %v", d.Suspected, d.Fix, tt.suspected, tt.fix)
			}
			if d.Handoff != HandoffUnconfirmed {
				t.Errorf("handoff %s, want unconfirmed", d.Handoff)
			}
			saw := tt.in.Verdicts[0].Claim == ClaimFail || len(tt.in.Verdicts[0].Contrary) > 0
			if slices.Contains(d.Narrowed, dg("5487509e")) != saw {
				t.Errorf("narrowed %v, want the green check named only when the evaluator saw a failure", d.Narrowed)
			}
			if err := checkDecision(tt.in, d); err != nil {
				t.Errorf("oracles: %v", err)
			}
		})
	}
}
