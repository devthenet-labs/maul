// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import "slices"

// The cost ladder's first rung (DESIGN 2.2 step 2, 6 S3; design C 3.2):
// which rung a new evaluation round runs. A full round replays the saved
// checks and then runs the evaluator (a model); a replay round only replays
// them, with no model at all, and Decide judges it unchanged. The scoped
// rung (a model that re-explores only what a diff touches) is not built
// yet: where it would run, a round is full.

// Rung is how much of an evaluation a round runs.
type Rung string

// Rungs.
const (
	RungFull   Rung = "full"
	RungReplay Rung = "replay"
)

// RungReason says why StartRung chose its rung.
type RungReason string

// Rung reasons, in the order StartRung tests them.
const (
	// RungReasonRetry: a retry of a full round's attempt is full too: its
	// model round has not run yet.
	RungReasonRetry RungReason = "retry"
	// RungReasonExplore: an approver's /maul verify explore.
	RungReasonExplore RungReason = "explore"
	// RungReasonEscalation: the one full round after an unsure replay round
	// at the same heads.
	RungReasonEscalation RungReason = "escalation"
	// RungReasonNoModelRound: no model round of these outcomes was judged
	// yet (round 1, or a new plan revision).
	RungReasonNoModelRound RungReason = "no-model-round"
	// RungReasonOutcomesChanged: the approved outcomes changed since the
	// last model round (a replan).
	RungReasonOutcomesChanged RungReason = "outcomes-changed"
	// RungReasonHeadsChanged: the heads moved since the last model round (a
	// fix or a human push; the scoped rung's case, full until it exists).
	RungReasonHeadsChanged RungReason = "heads-changed"
	// RungReasonModelObjected: the last model round's evaluator saw an
	// outcome fail or reported an anomaly (ModelRound.Clear false): only a
	// model can look again at what it raised.
	RungReasonModelObjected RungReason = "model-objected"
	// RungReasonUncovered: an outcome has no stored check a round could
	// pass it on, so a replay could only leave it unsure.
	RungReasonUncovered RungReason = "uncovered"
	// RungReasonReplay: everything a replay needs holds.
	RungReasonReplay RungReason = "replay"
)

// ModelRound is the latest full round judged on its evaluator's report, as
// the controller records it.
type ModelRound struct {
	// HeadSet and OutcomesDigest are the heads and the approved outcome
	// list it was judged at.
	HeadSet, OutcomesDigest string
	// Clear: its evaluator saw no outcome fail (no fail verdict, no
	// contrary entry) and reported no anomaly (ModelRoundClear).
	Clear bool
}

// RungInput is what StartRung decides on: the new round's heads and
// outcomes, the latest model round, what an approver or the loop asks for,
// and the store as Decide would read it (its checks with their records, the
// cached reviews).
type RungInput struct {
	// Keep is the rung of the round a retry continues; "" for a new round.
	Keep Rung
	// Explore: an approver's /maul verify explore asked for the full rung.
	Explore bool
	// Escalate: the round is the one escalation of an unsure replay round.
	Escalate bool
	// HeadSet and OutcomesDigest are the new round's.
	HeadSet, OutcomesDigest string
	// Model is the latest model round; nil for none.
	Model    *ModelRound
	Outcomes []OutcomeRef
	Checks   []StoredCheck
	Reviews  []CheckReview
}

// StartRung is the rung a new round runs, and why. It replays only when
// the heads and the approved outcomes are those of the latest model round,
// that round's evaluator raised nothing (Clear), and every outcome but the
// manual ones (at least one) is bound to a stored check Decide could pass
// it on (covered); a retry of a full round, /maul verify explore and the
// escalation of an unsure replay are full. Everything else is full.
func StartRung(in RungInput) (Rung, RungReason) {
	m := in.Model
	switch {
	case in.Keep == RungFull:
		return RungFull, RungReasonRetry
	case in.Explore:
		return RungFull, RungReasonExplore
	case in.Escalate:
		return RungFull, RungReasonEscalation
	case m == nil:
		return RungFull, RungReasonNoModelRound
	case m.OutcomesDigest != in.OutcomesDigest:
		return RungFull, RungReasonOutcomesChanged
	case m.HeadSet != in.HeadSet:
		return RungFull, RungReasonHeadsChanged
	case !m.Clear:
		return RungFull, RungReasonModelObjected
	case !covered(in):
		return RungFull, RungReasonUncovered
	}
	return RungReplay, RungReasonReplay
}

// covered reports every outcome but the manual ones, at least one, bound
// to a stored check Decide could pass it on: unretired, not narrowed, in
// one of maul's forms, valid and reviewed proves for its exact text (with
// the green question too, for a check saved red: decider.passes).
func covered(in RungInput) bool {
	d := index(Input{Outcomes: in.Outcomes, Checks: in.Checks, Reviews: in.Reviews}, true)
	evaluated := 0
	for _, o := range in.Outcomes {
		if o.Manual {
			continue
		}
		evaluated++
		if !slices.ContainsFunc(d.bound(o.ID), func(c StoredCheck) bool { return !c.Narrowed && d.passes(c, o) }) {
			return false
		}
	}
	return evaluated > 0
}

// ModelRoundClear reports a full round's input whose evaluator raised
// nothing a replay could not see again: no verdict of fail or with contrary
// entries (the outcome as worded seen to fail) and no anomaly, cited or
// not. A round that is not clear is followed by a full round at its heads.
func ModelRoundClear(in Input) bool {
	return len(in.Anomalies) == 0 && !slices.ContainsFunc(in.Verdicts, func(v EvaluatorVerdict) bool {
		return v.Claim == ClaimFail || len(v.Contrary) > 0
	})
}
