// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"slices"
	"testing"
)

// twoCheckPass is confirmedPass with a second stored check of the same
// outcome, c: a regression check saved earlier.
func twoCheckPass() Input {
	in := confirmedPass()
	in.Checks = append(in.Checks, StoredCheck{Digest: dg("c"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
	in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("c"), OutcomeDigest: od("elo"), Answer: ReviewProves})
	return in
}

// TestDecideStoredCheckNotReplayed is review VC-3: a stored check the round
// was to replay that has no replay run at these heads (the round cut short
// before it, or a run that never made the ledger) never lets its outcome
// pass on another check; it is unsure not-replayed. A check the round did
// not have to replay (proposed this round), or one replayed, does not hold
// it back, and nothing but a pass is withheld.
func TestDecideStoredCheckNotReplayed(t *testing.T) {
	cReplay := CheckRun{CheckDigest: dg("c"), Result: RunPass, Token: "tok-c", Seq: 4}
	for _, tc := range []struct {
		name   string
		in     Input
		result Result
		reason string
		fix    bool
	}{
		{name: "the regression check never replayed",
			in:     with(twoCheckPass(), func(in *Input) { in.ReplayRequired = []string{dg("c")} }),
			result: ResultUnsure, reason: ReasonNotReplayed},
		{name: "both required, only one replayed",
			in: with(twoCheckPass(), func(in *Input) {
				in.ReplayRequired = []string{dg("a"), dg("c")}
				in.Replay = []CheckRun{{CheckDigest: dg("a"), Result: RunPass, Token: "tok-r", Seq: 3}}
			}),
			result: ResultUnsure, reason: ReasonNotReplayed},
		{name: "the regression check replayed green",
			in: with(twoCheckPass(), func(in *Input) {
				in.ReplayRequired = []string{dg("c")}
				in.Replay = []CheckRun{cReplay}
			}),
			result: ResultPassed, reason: ReasonConfirmed},
		{name: "a confirmation is no replay",
			in: with(twoCheckPass(), func(in *Input) {
				in.ReplayRequired = []string{dg("c")}
				in.Confirmations = append(in.Confirmations, cReplay)
			}),
			result: ResultUnsure, reason: ReasonNotReplayed},
		{name: "a retired check need not replay",
			in: with(twoCheckPass(), func(in *Input) {
				in.ReplayRequired = []string{dg("c")}
				in.Checks[1].Retired = true
			}),
			result: ResultPassed, reason: ReasonConfirmed},
		{name: "a required check of another outcome",
			in: with(twoCheckPass(), func(in *Input) {
				in.ReplayRequired = []string{dg("c")}
				in.Checks[1].Outcome = "other"
			}),
			result: ResultPassed, reason: ReasonConfirmed},
		{name: "nothing required: today's judgement",
			in: twoCheckPass(), result: ResultPassed, reason: ReasonConfirmed},
		{name: "a reproduced failure still fails",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Checks = append(in.Checks, StoredCheck{Digest: dg("c"), Outcome: "elo", Form: CheckFormHTTP,
					Valid: true})
				in.ReplayRequired = []string{dg("a"), dg("c")}
			}),
			result: ResultFailed, reason: ReasonReproduced, fix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.in)
			got := d.PerOutcome[0]
			if got.Result != tc.result || got.Reason != tc.reason {
				t.Fatalf("result %s %s, want %s %s", got.Result, got.Reason, tc.result, tc.reason)
			}
			if (d.Fix != nil) != tc.fix {
				t.Errorf("fix %+v, want %v", d.Fix, tc.fix)
			}
			if tc.result != ResultPassed && d.Handoff == HandoffReady {
				t.Error("a ready hand-off without every outcome passed")
			}
		})
	}
}

// oracleReplayed restates VC-3: no pass while a stored, unretired check of
// the outcome that the round was to replay has no replay run this round.
func oracleReplayed(in Input, o OutcomeRef, _ string) error {
	for _, c := range in.Checks {
		if c.Outcome != o.ID || c.Retired || !slices.Contains(in.ReplayRequired, c.Digest) {
			continue
		}
		if !slices.ContainsFunc(in.Replay, func(run CheckRun) bool { return run.CheckDigest == c.Digest }) {
			return fmt.Errorf("VC-3: required check %s of %s was not replayed", c.Digest, o.ID)
		}
	}
	return nil
}
