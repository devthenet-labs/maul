// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

// issue55 is S1 demo bug 13's shape: a keep outcome whose saved check
// passed cleanly and was reviewed proves, and the evaluator's pass, whose
// own record names the requests where it saw the outcome as worded fail
// (ledger seqs 37 and 39: the exact GET body posted back was a 400).
func issue55() Input {
	return Input{
		Outcomes: []OutcomeRef{{ID: "create-round-trip-with-hits", Digest: od("rt"), Keep: true}},
		Checks: []StoredCheck{{Digest: dg("381bd01b"), Outcome: "create-round-trip-with-hits",
			Form: CheckFormHTTP, Valid: true}},
		Confirmations: []CheckRun{{CheckDigest: dg("381bd01b"), Result: RunPass, Token: "c9894f48", Seq: 69}},
		Reviews: []CheckReview{{CheckDigest: dg("381bd01b"), OutcomeDigest: od("rt"),
			Answer: ReviewProves}},
		Verdicts: []EvaluatorVerdict{{Outcome: "create-round-trip-with-hits", Claim: ClaimPass,
			Ledger: []int32{38, 47}, Contrary: []int32{37, 39}}},
	}
}

func TestDecideEvaluatorSawFailure(t *testing.T) {
	tests := []struct {
		name    string
		in      Input
		result  Result
		reason  string
		handoff Handoff
	}{
		{
			name:   "issue 55: a pass whose own record names where the outcome failed is unsure",
			in:     issue55(),
			result: ResultUnsure, reason: ReasonEvaluatorSawFailure, handoff: HandoffUnconfirmed,
		},
		{
			name: "issue 55 without the contrary entries: the proven check passes, as before",
			in: with(issue55(), func(in *Input) {
				in.Verdicts[0].Contrary = nil
			}),
			result: ResultPassed, reason: ReasonConfirmed, handoff: HandoffReady,
		},
		{
			name: "an unsure verdict naming where it saw the outcome fail: a proven check does not pass it",
			in: with(issue55(), func(in *Input) {
				in.Verdicts[0].Claim, in.Verdicts[0].Ledger = ClaimUnsure, nil
			}),
			result: ResultUnsure, reason: ReasonEvaluatorSawFailure, handoff: HandoffUnconfirmed,
		},
		{
			name: "the evaluator's fail: a proven green check does not pass the outcome",
			in: with(confirmedPass(), func(in *Input) {
				in.Verdicts[0].Claim = ClaimFail
			}),
			result: ResultUnsure, reason: ReasonEvaluatorSawFailure, handoff: HandoffUnconfirmed,
		},
		{
			name: "an unsure verdict naming nothing it saw fail: a proven check passes, as before",
			in: with(confirmedPass(), func(in *Input) {
				in.Verdicts[0].Claim, in.Verdicts[0].Ledger = ClaimUnsure, nil
			}),
			result: ResultPassed, reason: ReasonConfirmed, handoff: HandoffReady,
		},
		{
			name: "no verdict at all (a replay round): a proven check passes, as before",
			in: with(confirmedPass(), func(in *Input) {
				in.Verdicts = nil
			}),
			result: ResultPassed, reason: ReasonConfirmed, handoff: HandoffReady,
		},
		{
			name: "contrary entries never make a failure: a reproduced fail stays failed",
			in: with(deterministicFail(ClaimFail, 3), func(in *Input) {
				in.Verdicts[0].Contrary = []int32{3}
			}),
			result: ResultFailed, reason: ReasonReproduced, handoff: HandoffUnconfirmed,
		},
		{
			name: "contrary entries on a model-only pass leave it unsure for its own reason",
			in: with(confirmedPass(), func(in *Input) {
				in.Checks, in.Confirmations, in.Reviews = nil, nil, nil
				in.Verdicts[0].Contrary = []int32{4}
			}),
			result: ResultUnsure, reason: ReasonModelOnlyPass, handoff: HandoffUnconfirmed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			r := d.PerOutcome[0]
			if r.Result != tt.result || r.Reason != tt.reason || d.Handoff != tt.handoff {
				t.Errorf("got %s/%s handoff %s, want %s/%s handoff %s", r.Result, r.Reason, d.Handoff,
					tt.result, tt.reason, tt.handoff)
			}
			if tt.result != ResultFailed && d.Fix != nil {
				t.Errorf("a fix was requested: %+v", d.Fix)
			}
		})
	}
}

// genSawFailure is a generated round and the same round in which the
// evaluator says it saw some outcomes fail: their verdicts become fail or
// gain contrary entries.
type genSawFailure struct{ in, saw Input }

func (genSawFailure) Generate(r *rand.Rand, size int) reflect.Value {
	in := genDecide{}.Generate(r, size).Interface().(genDecide).in
	saw := in
	saw.Verdicts = nil
	for _, o := range in.Outcomes {
		v := EvaluatorVerdict{Outcome: o.ID, Claim: ClaimPass}
		if i := indexVerdict(in.Verdicts, o.ID); i >= 0 {
			v = in.Verdicts[i]
		}
		switch r.Intn(3) {
		case 0:
			v.Contrary = []int32{int32(1 + r.Intn(20))}
		case 1:
			v.Claim = ClaimFail
		}
		saw.Verdicts = append(saw.Verdicts, v)
	}
	return reflect.ValueOf(genSawFailure{in: in, saw: saw})
}

func indexVerdict(vs []EvaluatorVerdict, id string) int {
	for i, v := range vs {
		if v.Outcome == id {
			return i
		}
	}
	return -1
}

// TestDecideSawFailureOnlyWithholdsPasses is the guard's safety property:
// the evaluator saying it saw an outcome fail (a fail verdict, or contrary
// entries) can only turn a pass into unsure. Every other result, the fix
// and suspected requests, the replay round and the retirements are those
// of the same round judged on the original verdicts, wherever those
// verdicts did not already decide them; it never fails an outcome, never
// starts a fix.
func TestDecideSawFailureOnlyWithholdsPasses(t *testing.T) {
	withheld := 0
	prop := func(g genSawFailure) bool {
		if err := sawFailureOnlyWithholds(g, &withheld); err != nil {
			t.Logf("%v\ninput %+v", err, g.saw)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
	if withheld == 0 {
		t.Error("the generator never withheld a pass")
	}
}

func sawFailureOnlyWithholds(g genSawFailure, withheld *int) error {
	// The baseline: the same round with every changed verdict's fail
	// standing, so the fail paths (which need the evaluator's fail) are
	// compared like for like; only contrary entries and the pass guard
	// differ.
	base := g.saw
	base.Verdicts = make([]EvaluatorVerdict, len(g.saw.Verdicts))
	for i, v := range g.saw.Verdicts {
		v.Contrary = nil
		base.Verdicts[i] = v
	}
	a, b := decideUnguarded(base), Decide(g.saw)
	for i := range a.PerOutcome {
		x, y := a.PerOutcome[i], b.PerOutcome[i]
		switch {
		case x == y:
		case x.Result == ResultPassed && y.Result == ResultUnsure && y.Reason == ReasonEvaluatorSawFailure:
			*withheld++
		default:
			return fmt.Errorf("outcome %s: %+v became %+v", x.ID, x, y)
		}
	}
	if !reflect.DeepEqual(a.Fix, b.Fix) || !reflect.DeepEqual(a.Suspected, b.Suspected) ||
		a.ReplayRound != b.ReplayRound || !reflect.DeepEqual(a.Retire, b.Retire) {
		return fmt.Errorf("the requests changed: %+v became %+v", a, b)
	}
	if b.Handoff == HandoffReady && a.Handoff != HandoffReady {
		return fmt.Errorf("the hand-off became ready")
	}
	return nil
}

// decideUnguarded is Decide without the saw-failure guard, as before it.
func decideUnguarded(in Input) Decision {
	return decide(in, false)
}
