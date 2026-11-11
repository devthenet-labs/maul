// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The cost ladder, minimal S3 (DESIGN 2.2 step 2, 6 S3): each new round
// runs the rung acceptance.StartRung picks.
//
//   - replay: the saved checks only, each alone with fresh run tokens, no
//     evaluate and no checkreview run, so no model spend at all. Only at
//     the heads and approved outcomes of the latest model round
//     (status.verification.modelRound), only when that round's evaluator
//     raised nothing (no fail verdict, no contrary entry, no anomaly:
//     Clear), only when no anomaly an earlier round found at those heads
//     stands unaccepted (G-B-LIB-1: it holds them, and a replay could not
//     pass), and only when every outcome but the manual ones is bound to
//     a stored check a round could pass it on. It is judged by
//     acceptance.Decide unchanged, on the store and this round's runs: it
//     passes exactly what a full round with the same evidence passes when
//     that round's evaluator raises nothing (the seeded property
//     TestReplayRoundNeverPassesWhatAFullRoundWouldNot), and with no
//     evaluator it never fails an outcome.
//   - full: everything else: round 1 (no model round), a replan, moved
//     heads (the scoped rung's case, not built), an objecting model round,
//     an uncovered outcome, /maul verify explore, a retry of a full round,
//     and the escalation.
//
// The escalation: a replay round judged unsure (Unconfirmed) asks for one
// automatic full round at its heads (status.verification.escalation),
// once per head set. A dispute's model-free round goes through the same
// rung: when a replay could not pass every outcome it is full, and spends
// the head set's escalation. So at one head set the automatic rounds are
// at most full (dispute) -> replay -> full (escalation, dispute) -> replay:
// a dispute is only a full round's, an escalation only once, and each
// automatic round counts against maxVerifications.

// rungFor is the rung of the round startEvaluation is about to create
// (kind) at hs, with checks the store as read, and why: a retry keeps its
// round's rung (one recorded before the rung was is full), a waiting
// request's explore and a due escalation are full.
func (p *pass) rungFor(kind verifyKind, hs headSet, checks []v1alpha1.SavedCheck) (v1alpha1.VerificationRung,
	acceptance.RungReason) {
	v, acc := p.in.Status.Verification, p.in.Status.Acceptance
	store := storeInput(acc, checks, "")
	in := acceptance.RungInput{HeadSet: hs.digest, OutcomesDigest: acc.OutcomesDigest,
		Explore:     v.Requested != nil && v.Requested.Explore,
		Escalate:    kind != verifyRetry && escalationAsked(v, hs.digest),
		OpenFinding: len(openAnomalies(heldAnomalies(v, hs.digest, 0))) > 0,
		Outcomes:    store.Outcomes, Checks: store.Checks, Reviews: store.Reviews}
	if kind == verifyRetry {
		in.Keep = acceptance.Rung(v.Rung)
		if v.Rung == "" {
			in.Keep = acceptance.RungFull
		}
	}
	if m := v.ModelRound; m != nil {
		in.Model = &acceptance.ModelRound{HeadSet: m.HeadSetDigest, OutcomesDigest: m.OutcomesDigest, Clear: m.Clear}
	}
	rung, why := acceptance.StartRung(in)
	return v1alpha1.VerificationRung(rung), why
}

// escalationDue reports the one full round an unsure replay round asked
// for (status.verification.escalation) as due at the same heads: the
// replay round is the current one, judged there, Unconfirmed, with no
// request waiting, within maxVerifications and below the evaluation
// ceiling (a full round spends; past it the verification stays
// Unconfirmed, and the hand-off says why). It is not spaced by
// verifyInterval while automatic fixes are halted, as a new head set's
// automatic round is: it is the replay round's own follow-up, once per head
// set, and that round already ran when it was asked for.
func (p *pass) escalationDue(v *v1alpha1.IntentVerification) bool {
	return escalationAsked(v, v.HeadSetDigest) && v.State == v1alpha1.VerificationUnconfirmed && v.Ref != nil &&
		v.Requested == nil && v.OutcomesJudgedAt(v.Round, v.HeadSetDigest) &&
		v.Automatic < p.proj.Spec.Limits.MaxVerificationsLimit() && !p.atEvaluationCeiling()
}

// escalationAsked reports the escalation the current round, a replay at hs,
// asked for and that has not run: the next round at hs is full whatever
// starts it, an approver's request included, so a request never replays
// again in its place and spends it unrun.
func escalationAsked(v *v1alpha1.IntentVerification, hs string) bool {
	e := v.Escalation
	return e != nil && e.Round == v.Round && e.HeadSetDigest == hs && v.HeadSetDigest == hs &&
		v.Rung == v1alpha1.RungReplay
}

// followUpDue reports an automatic round at v's heads still to come after
// its judged round: a dispute's (its round may then be full, past the
// evaluation ceiling it is answered as exhausted and the dispute dropped:
// evaluationExhausted), or the escalation. The hand-off waits for it.
func (p *pass) followUpDue(v *v1alpha1.IntentVerification) bool {
	return (len(v.Disputed) > 0 && v.Ref != nil && p.verifyBounded(v)) || p.escalationDue(v)
}

// recordRound records on nv, as the round of rung at hs is created, its
// rung, and when it is an automatic full round at the heads of the round
// before it (prevRound; a dispute's follow-up a replay could not settle),
// that it spends the head set's one escalation.
func recordRound(nv *v1alpha1.IntentVerification, rung v1alpha1.VerificationRung, followUp bool, prevRound int32,
	hs string) {
	nv.Rung = rung
	if followUp && rung == v1alpha1.RungFull && !escalatedAt(nv, hs) {
		nv.Escalation = &v1alpha1.VerificationEscalation{Round: prevRound, HeadSetDigest: hs}
	}
}

// escalatedAt reports the head set's one escalation asked for or spent.
func escalatedAt(v *v1alpha1.IntentVerification, hs string) bool {
	return v.Escalation != nil && v.Escalation.HeadSetDigest == hs
}

// recordLadder records on nv what j, its round's judgement, means for the
// next round's rung: a full round judged on its evaluator's report is the
// latest model round, clear or not; a replay round judged unsure asks for
// the head set's one escalation, unless it was asked for already.
func recordLadder(nv *v1alpha1.IntentVerification, acc *v1alpha1.IntentAcceptance, j roundJudgement) {
	if !j.replay && j.evaluation != nil && acc.OutcomesDigest != "" && nv.HeadSetDigest != "" && nv.Round > 0 {
		nv.ModelRound = &v1alpha1.VerificationModelRound{Round: nv.Round, HeadSetDigest: nv.HeadSetDigest,
			OutcomesDigest: acc.OutcomesDigest, Clear: j.clear}
	}
	if j.replay && nv.State == v1alpha1.VerificationUnconfirmed && !escalatedAt(nv, nv.HeadSetDigest) &&
		modelCouldSettle(nv, acc, j) {
		nv.Escalation = &v1alpha1.VerificationEscalation{Round: nv.Round, HeadSetDigest: nv.HeadSetDigest}
	}
}

// modelCouldSettle reports an unsure replay round a full round could
// settle: an outcome a model is asked about (not a manual one, not one an
// approver accepted) did not pass, or the environment was not clean. A
// round unsure only on outcomes a person closes escalates to nothing.
func modelCouldSettle(nv *v1alpha1.IntentVerification, acc *v1alpha1.IntentAcceptance, j roundJudgement) bool {
	if !j.decision.Env.Clean() {
		return true
	}
	for _, r := range nv.Outcomes {
		i := slices.IndexFunc(acc.Outcomes, func(o v1alpha1.AcceptanceOutcome) bool { return o.ID == r.ID })
		if i >= 0 && !acc.Outcomes[i].Manual && r.Result != v1alpha1.OutcomePassed &&
			r.Result != v1alpha1.OutcomeAccepted {
			return true
		}
	}
	return false
}
