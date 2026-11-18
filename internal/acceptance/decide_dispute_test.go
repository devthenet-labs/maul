// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"slices"
	"testing"
)

// disputeRound is a round whose stored check a fails red by assertion while
// the evaluator says the outcome holds and disputes a, proposing r: r is
// stored for the same outcome, confirmed green with a tools token and
// reviewed proves. At a base, a is red there too.
func disputeRound() Input {
	in := deterministicFail(ClaimPass, 3, 9)
	in.Replay[0].AssertionStep, in.Replay[0].AssertionID = 1, "json /a"
	in.Disputes = []CheckDispute{{CheckDigest: dg("a"), Replacement: dg("r"), Ledger: []int32{3}}}
	in.Checks = append(in.Checks, StoredCheck{Digest: dg("r"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
	in.Confirmations = []CheckRun{{CheckDigest: dg("r"), Result: RunPass, Token: "tok-c", Seq: 9}}
	in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("r"), OutcomeDigest: od("elo"), Answer: ReviewProves})
	return in
}

// disputeBase gives disputeRound a required base where a is red by assertion and
// r ran as run says ("" for no run of r there).
func disputeBase(in *Input, r RunResult, asserted bool) {
	in.Base = &BaseRuns{Required: true, Runs: []CheckRun{
		{CheckDigest: dg("a"), Result: RunFail, Token: "tok-b", AssertionStep: 1, AssertionID: "json /a"}}}
	if r == "" {
		return
	}
	run := CheckRun{CheckDigest: dg("r"), Result: r, Token: "tok-b"}
	if asserted {
		run.AssertionStep, run.AssertionID = 1, "json /a"
	}
	in.Base.Runs = append(in.Base.Runs, run)
}

// TestDisputeReplacementMustBeAbleToPass: a dispute retires the disputed
// check only on a replacement that would count toward a pass itself under
// every pass rule (rules 1-5; rule 6 and the dispute itself aside, which a
// dispute round always breaks) and not narrowed (DESIGN 4.3: "the
// replacement check must clear rules 1-6 like any other"). Otherwise the failing check stands, the outcome is
// unsure (disputed) and nothing is replayed (code review 2026-11-18,
// judge 2).
func TestDisputeReplacementMustBeAbleToPass(t *testing.T) {
	tests := []struct {
		name   string
		in     Input
		retire bool
	}{
		{name: "no base: a proven replacement retires", in: disputeRound(), retire: true},
		{
			name:   "change outcome: a replacement red by assertion at base retires",
			in:     with(disputeRound(), func(in *Input) { disputeBase(in, RunFail, true) }),
			retire: true,
		},
		{
			name: "change outcome: a replacement that cannot compile at base retires (n/a)",
			in: with(disputeRound(), func(in *Input) {
				disputeBase(in, RunError, false)
				in.Base.Runs[1].Cause = "compile"
			}),
			retire: true,
		},
		{
			name: "an unbuildable base: a replacement with no run there retires (n/a)",
			in: with(disputeRound(), func(in *Input) {
				disputeBase(in, "", false)
				in.Base.Unbuildable = true
			}),
			retire: true,
		},
		{
			name: "keep outcome: a replacement green at base retires",
			in: with(disputeRound(), func(in *Input) {
				in.Outcomes[0].Keep = true
				disputeBase(in, RunPass, false)
			}),
			retire: true,
		},
		{
			name: "change outcome: a replacement green at base retires nothing (rule 5)",
			in:   with(disputeRound(), func(in *Input) { disputeBase(in, RunPass, false) }),
		},
		{
			name: "change outcome: a replacement failing bare at base retires nothing (rule 5)",
			in:   with(disputeRound(), func(in *Input) { disputeBase(in, RunFail, false) }),
		},
		{
			name: "change outcome: a replacement timing out at base retires nothing (rule 5)",
			in:   with(disputeRound(), func(in *Input) { disputeBase(in, RunError, false) }),
		},
		{
			name: "a required base with no run of the replacement retires nothing (rule 5)",
			in:   with(disputeRound(), func(in *Input) { disputeBase(in, "", false) }),
		},
		{
			name: "keep outcome: a replacement red by assertion at base retires nothing (rule 5)",
			in: with(disputeRound(), func(in *Input) {
				in.Outcomes[0].Keep = true
				disputeBase(in, RunFail, true)
			}),
		},
		{
			name: "a replacement narrowed in an earlier round retires nothing",
			in:   with(disputeRound(), func(in *Input) { in.Checks[1].Narrowed = true }),
		},
		{
			name: "a replacement quarantined this round retires nothing (rule 3)",
			in: with(disputeRound(), func(in *Input) {
				in.Confirmations = append(in.Confirmations, CheckRun{CheckDigest: dg("r"), Result: RunQuarantined,
					Token: "tok-c", Seq: 10})
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			if r := d.PerOutcome[0]; r.Result != ResultUnsure || r.Reason != ReasonDisputed {
				t.Errorf("outcome = %+v, want unsure (disputed)", r)
			}
			var want []string
			if tt.retire {
				want = []string{dg("a")}
			}
			if !slices.Equal(d.Retire, want) || d.ReplayRound != tt.retire {
				t.Errorf("Retire %v ReplayRound %v, want %v %v", d.Retire, d.ReplayRound, want, tt.retire)
			}
			if d.Fix != nil || d.Handoff == HandoffReady {
				t.Errorf("a dispute round fixed or handed off: %+v", d)
			}
		})
	}
}

// TestDisputeWithASecondCheckNeedsADiscriminatingReplacement is the review's
// case: a regressed check a and a passing, discriminating sibling b. With no
// dispute the checks disagree; a dispute whose replacement shows nothing at
// base leaves a standing, so no replay round can pass the outcome on b.
func TestDisputeWithASecondCheckNeedsADiscriminatingReplacement(t *testing.T) {
	in := with(disputeRound(), func(in *Input) {
		disputeBase(in, RunPass, false)
		in.Checks = append(in.Checks, StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
		in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("b"), Result: RunPass, Token: "tok-r", Seq: 5})
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("b"), OutcomeDigest: od("elo"),
			Answer: ReviewProves})
		in.Base.Runs = append(in.Base.Runs, CheckRun{CheckDigest: dg("b"), Result: RunFail, Token: "tok-b",
			AssertionStep: 1, AssertionID: "json /a"})
	})
	undisputed := with(in, func(in *Input) { in.Disputes = nil })
	if r := Decide(undisputed).PerOutcome[0]; r.Reason != ReasonChecksDisagree {
		t.Fatalf("no dispute: %+v, want checks-disagree (the case's premise)", r)
	}
	d := Decide(in)
	if len(d.Retire) > 0 || d.ReplayRound || d.PerOutcome[0].Reason != ReasonDisputed {
		t.Errorf("dispute on a replacement green at base: %+v Retire %v ReplayRound %v; want a standing",
			d.PerOutcome[0], d.Retire, d.ReplayRound)
	}
}

// TestChecksDisagreeNeedsADiscriminatingSibling pins rule 5 in disagree: a
// sibling that passes at head and at base too shows nothing about the
// change, so it is no vote against a check failing red by assertion that
// the evaluator agrees with.
func TestChecksDisagreeNeedsADiscriminatingSibling(t *testing.T) {
	in := with(deterministicFail(ClaimFail, 3), func(in *Input) {
		in.Replay[0].AssertionStep, in.Replay[0].AssertionID = 1, "json /a"
		in.Checks = append(in.Checks, StoredCheck{Digest: dg("b"), Outcome: "elo", Form: CheckFormHTTP, Valid: true})
		in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("b"), Result: RunPass, Token: "tok-r", Seq: 5})
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: dg("b"), OutcomeDigest: od("elo"),
			Answer: ReviewProves})
		in.Base = &BaseRuns{Required: true, Runs: []CheckRun{
			{CheckDigest: dg("a"), Result: RunFail, Token: "tok-b", AssertionStep: 1, AssertionID: "json /a"},
			{CheckDigest: dg("b"), Result: RunPass, Token: "tok-b"}}}
	})
	if r := Decide(in).PerOutcome[0]; r.Result != ResultFailed || r.Reason != ReasonReproduced {
		t.Errorf("sibling green at base: %+v, want failed (reproduced)", r)
	}
	in.Base.Runs[1] = CheckRun{CheckDigest: dg("b"), Result: RunFail, Token: "tok-b", AssertionStep: 1,
		AssertionID: "json /a"}
	if r := Decide(in).PerOutcome[0]; r.Reason != ReasonChecksDisagree {
		t.Errorf("sibling red at base: %+v, want checks-disagree", r)
	}
}

// TestDisputeUnderAnObjectionRetiresNothing: a replacement that passes
// where this round's evaluator says it saw the outcome fail (a fail
// verdict, or contrary entries) asserts less than the outcome: the
// saw-failure guard withholds its pass and it is narrowed this very round,
// so it could never pass the outcome. The disputed check stands; the same
// dispute with no objection still retires (no false fail).
func TestDisputeUnderAnObjectionRetiresNothing(t *testing.T) {
	objections := map[string]func(in *Input){
		"contrary entries": func(in *Input) { in.Verdicts[0].Contrary = []int32{4} },
		"a fail verdict":   func(in *Input) { in.Verdicts[0].Claim = ClaimFail },
	}
	for name, object := range objections {
		t.Run(name, func(t *testing.T) {
			d := Decide(with(disputeRound(), func(in *Input) {
				disputeBase(in, RunFail, true)
				object(in)
			}))
			if len(d.Retire) > 0 || d.ReplayRound {
				t.Errorf("Retire %v ReplayRound %v on a replacement narrowed this round; want a standing", d.Retire,
					d.ReplayRound)
			}
			if r := d.PerOutcome[0]; r.Result != ResultUnsure || r.Reason != ReasonDisputed {
				t.Errorf("outcome = %+v, want unsure (disputed)", r)
			}
			if !slices.Contains(d.Narrowed, dg("r")) || !slices.Contains(d.Objected, "elo") {
				t.Errorf("Narrowed %v Objected %v, want r narrowed and the outcome objected", d.Narrowed, d.Objected)
			}
		})
	}
	if d := Decide(with(disputeRound(), func(in *Input) { disputeBase(in, RunFail, true) })); !slices.Equal(d.Retire,
		[]string{dg("a")}) {
		t.Errorf("no objection: Retire %v, want a retired", d.Retire)
	}
}

// TestDisputeReplacementNotReproducedRetiresNothing: a replacement whose
// runs this round maul does not count (also proposed to show a defect, its
// confirmation not red) is no evidence either way, so it retires nothing.
func TestDisputeReplacementNotReproducedRetiresNothing(t *testing.T) {
	d := Decide(with(disputeRound(), func(in *Input) {
		in.Replay = append(in.Replay, CheckRun{CheckDigest: dg("r"), Result: RunPass, Token: "tok-r", Seq: 6})
		in.Confirmations[0].ExpectFail = true
	}))
	if len(d.Retire) > 0 || d.ReplayRound {
		t.Errorf("Retire %v ReplayRound %v on a replacement not reproduced; want a standing", d.Retire, d.ReplayRound)
	}
}
