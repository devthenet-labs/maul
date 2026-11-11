// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
)

// replayable is a round StartRung replays: a clear model round at the same
// heads and outcomes, and both outcomes bound to a stored check reviewed
// proves for their text.
func replayable() RungInput {
	return RungInput{
		HeadSet: "heads-1", OutcomesDigest: "outcomes-1",
		Model:    &ModelRound{HeadSet: "heads-1", OutcomesDigest: "outcomes-1", Clear: true},
		Outcomes: []OutcomeRef{{ID: "elo", Digest: od("elo")}, {ID: "keep", Digest: od("keep"), Keep: true}},
		Checks: []StoredCheck{{Digest: dg("a"), Outcome: "elo", Form: CheckFormHTTP, Valid: true},
			{Digest: dg("b"), Outcome: "keep", Form: CheckFormBrowser, Valid: true}},
		Reviews: []CheckReview{{CheckDigest: dg("a"), OutcomeDigest: od("elo"), Answer: ReviewProves},
			{CheckDigest: dg("b"), OutcomeDigest: od("keep"), Answer: ReviewProves}},
	}
}

func withRung(in RungInput, f func(*RungInput)) RungInput { f(&in); return in }

// TestStartRung is the ladder's table (DESIGN 2.2 step 2, minimal S3):
// replay only at the heads and outcomes of a model round whose evaluator
// raised nothing, with every outcome bound to a stored check that a round
// could pass it on; full otherwise.
func TestStartRung(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     RungInput
		rung   Rung
		reason RungReason
	}{
		{name: "unchanged heads, every outcome proven", in: replayable(), rung: RungReplay, reason: RungReasonReplay},
		{name: "round 1: no model round", in: withRung(replayable(), func(in *RungInput) { in.Model = nil }),
			rung: RungFull, reason: RungReasonNoModelRound},
		{name: "the heads moved", in: withRung(replayable(), func(in *RungInput) { in.HeadSet = "heads-2" }),
			rung: RungFull, reason: RungReasonHeadsChanged},
		{name: "a replan changed the outcomes",
			in:   withRung(replayable(), func(in *RungInput) { in.OutcomesDigest = "outcomes-2" }),
			rung: RungFull, reason: RungReasonOutcomesChanged},
		{name: "/maul verify explore", in: withRung(replayable(), func(in *RungInput) { in.Explore = true }),
			rung: RungFull, reason: RungReasonExplore},
		{name: "the escalation of an unsure replay",
			in:   withRung(replayable(), func(in *RungInput) { in.Escalate = true }),
			rung: RungFull, reason: RungReasonEscalation},
		{name: "a retry keeps its full round",
			in:   withRung(replayable(), func(in *RungInput) { in.Keep = RungFull }),
			rung: RungFull, reason: RungReasonRetry},
		{name: "a retry of a replay round replays",
			in:   withRung(replayable(), func(in *RungInput) { in.Keep = RungReplay }),
			rung: RungReplay, reason: RungReasonReplay},
		{name: "explore wins over a replay retry",
			in:   withRung(replayable(), func(in *RungInput) { in.Keep, in.Explore = RungReplay, true }),
			rung: RungFull, reason: RungReasonExplore},
		{name: "the model round's evaluator saw a failure or an anomaly",
			in:   withRung(replayable(), func(in *RungInput) { in.Model.Clear = false }),
			rung: RungFull, reason: RungReasonModelObjected},
		// G-B-LIB-1: a later model round at the same heads that raised
		// nothing does not clear an earlier round's open anomaly there.
		{name: "an open finding at the heads, under a clear model round",
			in:   withRung(replayable(), func(in *RungInput) { in.OpenFinding = true }),
			rung: RungFull, reason: RungReasonOpenFinding},
		{name: "a retry of a replay round at heads an open finding holds",
			in:   withRung(replayable(), func(in *RungInput) { in.Keep, in.OpenFinding = RungReplay, true }),
			rung: RungFull, reason: RungReasonOpenFinding},
		{name: "an outcome with no stored check",
			in:   withRung(replayable(), func(in *RungInput) { in.Checks = in.Checks[:1] }),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a check reviewed for another text",
			in: withRung(replayable(), func(in *RungInput) {
				in.Reviews = slices.Clone(in.Reviews)
				in.Reviews[1].OutcomeDigest = od("elo")
			}),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a check also answered unclear",
			in: withRung(replayable(), func(in *RungInput) {
				in.Reviews = append(slices.Clone(in.Reviews), CheckReview{CheckDigest: dg("b"),
					OutcomeDigest: od("keep"), Answer: ReviewUnclear})
			}),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a narrowed check",
			in: withRung(replayable(), func(in *RungInput) {
				in.Checks = slices.Clone(in.Checks)
				in.Checks[0].Narrowed = true
			}),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a retired check",
			in: withRung(replayable(), func(in *RungInput) {
				in.Checks = slices.Clone(in.Checks)
				in.Checks[0].Retired = true
			}),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a check saved red, not yet green",
			in: withRung(replayable(), func(in *RungInput) {
				in.Checks = slices.Clone(in.Checks)
				in.Checks[0].ExpectFail, in.Checks[0].FailedStep, in.Checks[0].FailedAssertion = true, 1, "json /a"
				in.Reviews = slices.Clone(in.Reviews)
				in.Reviews[0].ExpectFail, in.Reviews[0].FailedStep, in.Reviews[0].FailedAssertion = true, 1, "json /a"
			}),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a model-judged form",
			in: withRung(replayable(), func(in *RungInput) {
				in.Checks = slices.Clone(in.Checks)
				in.Checks[0].Form = "judged"
			}),
			rung: RungFull, reason: RungReasonUncovered},
		{name: "a manual outcome needs no check",
			in: withRung(replayable(), func(in *RungInput) {
				in.Outcomes = append(slices.Clone(in.Outcomes), OutcomeRef{ID: "look", Digest: od("look"),
					Manual: true})
			}),
			rung: RungReplay, reason: RungReasonReplay},
		{name: "only manual outcomes: nothing to replay",
			in: withRung(replayable(), func(in *RungInput) {
				in.Outcomes = []OutcomeRef{{ID: "look", Digest: od("look"), Manual: true}}
			}),
			rung: RungFull, reason: RungReasonUncovered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rung, reason := StartRung(tc.in)
			if rung != tc.rung || reason != tc.reason {
				t.Errorf("StartRung = %s (%s), want %s (%s)", rung, reason, tc.rung, tc.reason)
			}
		})
	}
}

// genRung is a random StartRung input over small pools, biased so that
// every precondition of the replay rung holds often enough for a replay to
// come out.
type genRung struct{ in RungInput }

func (genRung) Generate(r *rand.Rand, size int) reflect.Value {
	base := genDecide{}.Generate(r, size).Interface().(genDecide).in
	in := RungInput{Outcomes: base.Outcomes, Checks: base.Checks, Reviews: base.Reviews,
		HeadSet: pickOf(r, []string{"h1", "h1", "h2"}), OutcomesDigest: pickOf(r, []string{"o1", "o1", "o2"}),
		Explore: r.Intn(6) == 0, Escalate: r.Intn(6) == 0, Keep: pickOf(r, []Rung{"", "", RungFull, RungReplay})}
	if r.Intn(6) != 0 {
		in.Model = &ModelRound{HeadSet: pickOf(r, []string{"h1", "h1", "h2"}),
			OutcomesDigest: pickOf(r, []string{"o1", "o1", "o2"}), Clear: r.Intn(5) != 0}
	}
	// Bias: most outcomes get a valid, stored check in maul's form,
	// reviewed proves for their own text.
	for i, o := range in.Outcomes {
		if r.Intn(4) == 0 {
			continue
		}
		d := dg(fmt.Sprintf("cover-%d", i))
		in.Checks = append(in.Checks, StoredCheck{Digest: d, Outcome: o.ID, Form: CheckFormHTTP, Valid: true,
			Narrowed: r.Intn(10) == 0, Retired: r.Intn(10) == 0})
		in.Reviews = append(in.Reviews, CheckReview{CheckDigest: d, OutcomeDigest: o.Digest, Answer: ReviewProves})
	}
	return reflect.ValueOf(genRung{in})
}

// oracleRungReplay restates, independently of rung.go, what must hold of a
// round StartRung replays: not a retry of a full round, no explore, no
// escalation; a model round on record at exactly these heads and outcomes
// whose evaluator raised nothing; and every outcome but the manual ones,
// at least one of them, bound to a stored, unretired, unnarrowed check in
// maul's forms, valid, reviewed proves for its exact text and answered
// nothing else, and, if saved red, cleared and reviewed with the green
// question.
func oracleRungReplay(in RungInput) error {
	switch {
	case in.Keep == RungFull:
		return fmt.Errorf("a retry of a full round replays")
	case in.Explore:
		return fmt.Errorf("/maul verify explore replays")
	case in.Escalate:
		return fmt.Errorf("an escalation replays")
	case in.Model == nil:
		return fmt.Errorf("no model round, and a replay")
	case in.Model.HeadSet != in.HeadSet:
		return fmt.Errorf("heads moved since the model round, and a replay")
	case in.Model.OutcomesDigest != in.OutcomesDigest:
		return fmt.Errorf("outcomes changed since the model round, and a replay")
	case !in.Model.Clear:
		return fmt.Errorf("the model round's evaluator objected, and a replay")
	}
	evaluated := 0
	for _, o := range in.Outcomes {
		if o.Manual {
			continue
		}
		evaluated++
		if !slices.ContainsFunc(in.Checks, func(c StoredCheck) bool { return oracleProven(in, c, o) }) {
			return fmt.Errorf("outcome %s has no proven stored check, and a replay", o.ID)
		}
	}
	if evaluated == 0 {
		return fmt.Errorf("nothing to evaluate, and a replay")
	}
	return nil
}

func oracleProven(in RungInput, c StoredCheck, o OutcomeRef) bool {
	if !slices.Contains([]CheckForm{CheckFormHTTP, CheckFormBrowser, CheckFormExec, CheckFormRun}, c.Form) ||
		c.Outcome != o.ID || c.Retired || c.Narrowed || !c.Valid || o.Digest == "" {
		return false
	}
	proves, green, red := oracleReviewed(in, c, o)
	redSaved := slices.ContainsFunc(in.Checks, func(x StoredCheck) bool { return x.Digest == c.Digest && x.ExpectFail })
	if !redSaved {
		return proves
	}
	firstRed := in.Checks[slices.IndexFunc(in.Checks, func(x StoredCheck) bool {
		return x.Digest == c.Digest && x.ExpectFail
	})]
	cleared := firstRed.Cleared && !firstRed.NoRunData && green
	return proves && green && (red || cleared)
}

// oracleReviewed is what the cached reviews of c for o's exact text say:
// proves (at least one, all proves), and whether one asked the green
// question and one the red question about c's own failing assertion.
func oracleReviewed(in RungInput, c StoredCheck, o OutcomeRef) (proves, green, red bool) {
	for _, rv := range in.Reviews {
		if rv.CheckDigest != c.Digest || rv.OutcomeDigest != o.Digest {
			continue
		}
		if rv.Answer != ReviewProves {
			return false, false, false
		}
		proves = true
		green = green || !rv.ExpectFail
		red = red || (rv.ExpectFail && rv.FailedStep == c.FailedStep && rv.FailedAssertion == c.FailedAssertion)
	}
	return proves, green, red
}

// TestStartRungReplaysOnlyWhatItMay is StartRung's seeded property: a
// replay comes out only when every precondition of the replay rung holds
// (oracleRungReplay), and the property is not vacuous.
func TestStartRungReplaysOnlyWhatItMay(t *testing.T) {
	replays, fulls := 0, 0
	prop := func(g genRung) bool {
		rung, reason := StartRung(g.in)
		switch rung {
		case RungReplay:
			replays++
			if err := oracleRungReplay(g.in); err != nil {
				t.Logf("replay (%s): %v\n%+v", reason, err, g.in)
				return false
			}
		case RungFull:
			fulls++
			if reason == RungReasonReplay {
				t.Logf("full with the replay reason: %+v", g.in)
				return false
			}
		default:
			t.Logf("rung %q", rung)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(4000)); err != nil {
		t.Fatal(err)
	}
	if replays < 100 || fulls < 100 {
		t.Fatalf("replays %d, fulls %d: the property is near vacuous", replays, fulls)
	}
}
