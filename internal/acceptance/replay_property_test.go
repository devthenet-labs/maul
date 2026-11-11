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

// The replay rung's safety (minimal S3): a replay round runs no model, and
// Decide judges it unchanged, on the store as it stands and this round's
// replay, base runs and environment. "The same evidence" is exactly those
// facts: the store's checks with their narrowing and clearance records,
// the cached reviews, the replay runs, the base runs and the environment. A
// full round at the same heads has the same facts plus its evaluator's
// claims (and new checks it proposes, which are new evidence and not the
// same evidence).
//
//  1. Same evidence, an evaluator that raises nothing against an outcome:
//     the replay round decides every outcome exactly as the full round.
//  2. Whatever a full round's evaluator does claim (a fail, contrary
//     entries, disputes, retirements, anomalies), it never adds a pass the
//     replay round would not give: claims only withhold.
//  3. A replay round never fails an outcome, never asks for a fix, a
//     suspected defect, a retirement or a narrowing: with no evaluator,
//     no failure is ever agreed.
//
// Together: a replay round never passes an outcome a full round with the
// same evidence would not, unless that round's evaluator objects; and
// StartRung replays only after a model round at the same heads whose
// evaluator raised nothing (ModelRound.Clear, TestStartRungReplaysOnlyWhatItMay),
// so the objection a full round could add is the one the last model round
// at these heads did not make.

// genReplayRound is one round's evidence judged three ways: as a replay
// round (no claims, no confirmations), as a full round whose evaluator
// raises nothing (pass or unsure verdicts, with or without citations, and
// any anomalies), and as a full round whose evaluator claims anything.
type genReplayRound struct{ replay, quiet, any Input }

func (genReplayRound) Generate(r *rand.Rand, size int) reflect.Value {
	base := genDecide{}.Generate(r, size).Interface().(genDecide).in
	replay := base
	replay.Confirmations, replay.Verdicts, replay.Disputes, replay.Retired, replay.Anomalies = nil, nil, nil, nil, nil
	// A replay round replays the whole store it was handed.
	replay.ReplayRequired = nil
	for _, c := range replay.Checks {
		if !c.Retired {
			replay.ReplayRequired = append(replay.ReplayRequired, c.Digest)
		}
	}
	quiet := replay
	for _, o := range replay.Outcomes {
		if r.Intn(4) == 0 {
			continue
		}
		v := EvaluatorVerdict{Outcome: o.ID, Claim: pickOf(r, []Claim{ClaimPass, ClaimPass, ClaimUnsure})}
		if r.Intn(3) != 0 {
			v.Ledger = []int32{int32(1 + r.Intn(20))}
		}
		quiet.Verdicts = append(quiet.Verdicts, v)
	}
	if r.Intn(3) == 0 {
		quiet.Anomalies = []AnomalyRef{{Summary: "a page error", Route: "/", Ledger: []int32{2}}}
	}
	anyClaims := replay
	genClaims(r, &anyClaims)
	for range r.Intn(3) {
		d := CheckDispute{CheckDigest: pickOf(r, genDigests), Ledger: []int32{1}}
		if r.Intn(2) == 0 {
			d.Replacement = pickOf(r, genDigests)
		}
		anyClaims.Disputes = append(anyClaims.Disputes, d)
	}
	for range r.Intn(2) {
		anyClaims.Retired = append(anyClaims.Retired, Retirement{CheckDigest: pickOf(r, genDigests), Reason: "r"})
	}
	if r.Intn(3) == 0 {
		anyClaims.Anomalies = []AnomalyRef{{Summary: "a console error", Route: "/", Ledger: []int32{3}}}
	}
	return reflect.ValueOf(genReplayRound{replay: replay, quiet: quiet, any: anyClaims})
}

// TestReplayRoundNeverPassesWhatAFullRoundWouldNot is the property above,
// over generated rounds (seeded).
func TestReplayRoundNeverPassesWhatAFullRoundWouldNot(t *testing.T) {
	passes, unsure := 0, 0
	prop := func(g genReplayRound) bool {
		replay, quiet, anyClaims := Decide(g.replay), Decide(g.quiet), Decide(g.any)
		// 3: no failure, fix, suspicion, retirement or narrowing.
		if replay.Fix != nil || replay.Suspected != nil || replay.ReplayRound || len(replay.Retire) > 0 ||
			len(replay.Narrowed) > 0 {
			t.Logf("replay round decided %+v", replay)
			return false
		}
		for i, rr := range replay.PerOutcome {
			if err := sameAsFull(rr, quiet.PerOutcome[i], anyClaims.PerOutcome[i]); err != nil {
				t.Logf("%v\nreplay %+v\nquiet %+v", err, g.replay, g.quiet)
				return false
			}
			switch rr.Result {
			case ResultPassed:
				passes++
			case ResultUnsure:
				unsure++
			case ResultFailed, ResultQuarantined:
			}
		}
		// A ready replay round is a ready quiet full round, anomalies aside.
		if replay.Handoff == HandoffReady && len(g.quiet.Anomalies) == 0 && quiet.Handoff != HandoffReady {
			t.Logf("replay ready, quiet full round %s", quiet.Handoff)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(6000)); err != nil {
		t.Fatal(err)
	}
	if passes < 100 || unsure < 100 {
		t.Fatalf("passes %d, unsure %d: the property is near vacuous", passes, unsure)
	}
}

// sameAsFull holds one outcome's replay result rr to properties 1-3: never
// failed, the result of the quiet full round q, and passed wherever the
// full round a whose evaluator claims anything passed it.
func sameAsFull(rr, q, a OutcomeResult) error {
	switch {
	case rr.Result == ResultFailed:
		return fmt.Errorf("replay round failed %s (%s)", rr.ID, rr.Reason)
	case q.ID != rr.ID || q.Result != rr.Result || (rr.Result == ResultPassed && q.Basis != rr.Basis):
		return fmt.Errorf("outcome %s: replay %s (%s %s), quiet full round %s (%s %s)", rr.ID, rr.Result,
			rr.Reason, rr.Basis, q.Result, q.Reason, q.Basis)
	case a.Result == ResultPassed && rr.Result != ResultPassed:
		return fmt.Errorf("outcome %s: a full round's claims passed it (%s), the replay round did not (%s %s)",
			rr.ID, a.Basis, rr.Result, rr.Reason)
	}
	return nil
}

// TestReplayAfterAnObjectionIsFull closes the gap property 2 leaves: a
// full round whose evaluator saw an outcome fail, or reported an anomaly,
// is not Clear, so the next round at its heads is full, never a replay
// that would pass what that evaluator objected to.
func TestReplayAfterAnObjectionIsFull(t *testing.T) {
	prop := func(g genReplayRound) bool {
		objected := len(g.any.Anomalies) > 0 || slices.ContainsFunc(g.any.Verdicts, func(v EvaluatorVerdict) bool {
			return v.Claim == ClaimFail || len(v.Contrary) > 0
		})
		next := RungInput{HeadSet: "h", OutcomesDigest: "o", Outcomes: g.any.Outcomes, Checks: g.any.Checks,
			Reviews: g.any.Reviews,
			Model:   &ModelRound{HeadSet: "h", OutcomesDigest: "o", Clear: ModelRoundClear(g.any)}}
		rung, _ := StartRung(next)
		return !objected || rung == RungFull
	}
	if err := quick.Check(prop, quickConfig(4000)); err != nil {
		t.Fatal(err)
	}
}
