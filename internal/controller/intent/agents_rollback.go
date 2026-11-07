// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// Switching verification off and on again (S2-B4). An agents round's
// verification has one status writer, the agents host, which is registered
// only with --intent-evaluator --intent-evaluator-agents: once the host is
// switched off (the rollback, chart
// intentController.verification.agents.enabled: false) nothing will ever
// settle a round it left open. The intent reconciler never writes that
// status; it judges, on its own record, that no host will settle the round
// and settles it itself (settleStranded): Unavailable, never a pass, a fail
// or a fix round, the hand-off released as couldn't confirm. The run
// scheduler cancels the round's evaluator once the Intent no longer has its
// verification in flight (evaluationEnded).
//
// A round already settled at the current heads is not the rollback's to
// undo (S2-B7): it stays as recorded while the host is off (settledAtHeads),
// so once the host returns the intent goes on as if it had never gone, and
// only a head move or an approver's /maul verify starts its next round.
//
// A Project that verifies again never leaves an intent Off (resumeOff): Off
// at recorded heads would otherwise hold for ever, for neither a round nor
// an approver's /maul verify starts from it. With the agents host off, Off
// is what an intent with no round settled at its current heads records
// (none yet, its heads moved, or an Off left by a release before S2-B7):
// what it was owed before the switch it is owed after.

// switchedOff records an intent of a Project that does not verify: Off
// (settleState), but with the agents host switched off, an open agents
// round no host will settle is settled Unavailable (settleStranded), and a
// round settled at the current heads keeps its state, its request consumed,
// so the next head move or an approver's /maul verify after the host
// returns starts the next round.
func (p *pass) switchedOff(ctx context.Context) (bool, error) {
	if reason, _ := p.set.verificationOff(p.proj); reason == v1alpha1.ReasonAgentsToolPodNotEnabled {
		v := p.in.Status.Verification
		switch {
		case v.Ref != nil && v.State == v1alpha1.VerificationEvaluating:
			return p.settleStranded(ctx)
		case p.settledAtHeads(v):
			return p.settleState(ctx, v.State)
		}
	}
	return p.settleState(ctx, v1alpha1.VerificationOff)
}

// settledAtHeads reports an agents verification that records a settled
// round (judged, being fixed, given up on, or unavailable, a stranded one
// included) at the surface's current heads, which the host's own head set
// says while it is off (agentsHost.heads reads only the Project and the
// Intent).
func (p *pass) settledAtHeads(v *v1alpha1.IntentVerification) bool {
	switch v.State {
	case v1alpha1.VerificationPassed, v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationFailing,
		v1alpha1.VerificationFixing, v1alpha1.VerificationExhausted, v1alpha1.VerificationUnavailable:
	default:
		return false
	}
	hs, ok := agentsHost{}.heads(p)
	return ok && hs.digest == v.HeadSetDigest
}

// settleStranded settles the open agents round of an intent whose host is
// switched off: its verification, unsettled, is deleted (dropInFlight; one
// the host settled before it went is kept for its evidence, unread), and
// the state is Unavailable with no judgement, any request consumed and the
// hand-off released as the unconfirmed rules release it (releaseIfDue). The
// reference stays, so no retry is owed at these heads.
func (p *pass) settleStranded(ctx context.Context) (bool, error) {
	ref := *p.in.Status.Verification.Ref
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "the agents host is off; its open round settles unavailable",
		slog.String("intent", p.in.Name), slog.String("verification", ref.Name))
	if err := p.answerNotRun(ctx, p.offNotRunWhy(), 0); err != nil {
		return false, err
	}
	p.dropInFlight(ctx, ref)
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.UnavailableReason, nv.Requested = v1alpha1.VerificationUnavailable, "", nil
		clearJudgement(nv)
		countManual(nv, cur.Status.Acceptance)
		p.releaseIfDue(cur)
		return nil
	})
}

// resumeOff starts verification over at the recorded heads on a pass that
// verifies an intent left Off while its Project did not (the controller,
// the Project or its tool host switched it off): what is on record predates
// the switch, so it is dropped as a moved head set drops it (syncHeadSet),
// an unsettled verification deleted, and the gate is due at these heads as
// for a head set never verified, an approver's request served first.
func (p *pass) resumeOff(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	if v.State != v1alpha1.VerificationOff {
		return false, nil
	}
	if v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State = v1alpha1.VerificationWaitingForCI
		nv.Ref, nv.Attempt, nv.InfraRetries = nil, 0, 0
		nv.NoDefect = nil
		clearJudgement(nv)
		countManual(nv, cur.Status.Acceptance)
		return nil
	})
}

// evaluationEnded reports an evaluate run whose round in no longer has in
// flight: the Intent's verification is another, or has settled (any state
// but Evaluating; superseded, retried, given up, or settled without it, as
// an agents round is when its host is switched off). Nothing it would
// report is read, so the run scheduler stops it.
func evaluationEnded(in *v1alpha1.Intent, run *v1alpha1.IntentRun) bool {
	if run.Spec.Stage != v1alpha1.IntentStageEvaluate || run.Spec.Evaluation == nil {
		return false
	}
	v := in.Status.Verification
	return v == nil || v.Ref == nil || v.Ref.UID != run.Spec.Evaluation.UID ||
		v.State != v1alpha1.VerificationEvaluating
}

// evaluationEndedDetail says why an evaluate run stopped unfinished.
const evaluationEndedDetail = "its evaluation round ended before the evaluator did; nothing it reports is used"
