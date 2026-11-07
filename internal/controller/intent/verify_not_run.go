// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"strconv"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/templates"
)

// An approver's /maul verify that cannot run (S2-B8, and the S2-B4
// review's follow-up) is answered so, with why, never only acknowledged:
// one made while the intent's verification is off is answered at once and
// recorded nowhere (requestVerification, offWhy); one recorded and then
// consumed unrun, because verification went off before it ran or its round
// could not run, is answered by a notice of its own on the intent issue
// (answerNotRun) before the write that consumes it, so a failed post leaves
// it pending for the next pass. A round that cannot run at heads an
// earlier round already judged leaves that judgement standing
// (standingJudgement): nothing new was learned about those commits, and
// every surface goes on showing the state on record. The approver sends the
// request again once what was missing is back; nothing stays stuck.

// notRunKeyPrefix keys the notice answering an approver's /maul verify
// that was consumed without a run.
const notRunKeyPrefix = "verify-unrun-"

// offWhy is why the intent's verification is off, as a templates.NotRun
// constant; off is false while it is verified (pass.verifies, and the
// evaluator is one maul verified for the Project's harness).
func (p *pass) offWhy() (why string, off bool) {
	if p.verifies() {
		if p.set.evaluatorUnverified(p.proj) {
			return templates.NotRunHarnessUnverified, true
		}
		return "", false
	}
	if !p.set.Verification || !p.proj.Spec.VerificationEnabled() {
		return templates.NotRunNotEnabled, true
	}
	switch reason, _ := p.set.verificationOff(p.proj); reason {
	case v1alpha1.ReasonAgentsToolPodNotEnabled:
		return templates.NotRunToolPodOff, true
	case v1alpha1.ReasonSurfaceCombinationUnsupported:
		return templates.NotRunSurfaceCombination, true
	case ReasonPreviewsDisabled:
		return templates.NotRunPreviewsOff, true
	}
	return "", true // the tool host cannot verify it: the generic words
}

// offNotRunWhy is offWhy's reason alone, for a request consumed because
// verification is off.
func (p *pass) offNotRunWhy() string {
	why, _ := p.offWhy()
	return why
}

// standingJudgement is the round whose judgement of the current heads
// stands on record (0 when none does): a settled round's reference at the
// head set, and its outcomes judged by that round at those heads.
func standingJudgement(v *v1alpha1.IntentVerification) int32 {
	if v.Ref == nil || !v.OutcomesJudgedAt(v.Round, v.HeadSetDigest) {
		return 0
	}
	switch v.State {
	case v1alpha1.VerificationPassed, v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationFailing,
		v1alpha1.VerificationExhausted:
		return v.Round
	}
	return 0
}

// answerNotRun answers the approver's pending request, if any, that it did
// not run (why, a templates.NotRun constant; stands, the round whose
// judgement stands, 0 for none), once, on the intent issue. The modifiers
// are not named: a fix re-arm or a reset took effect when the request was
// recorded; only the check did not run.
func (p *pass) answerNotRun(ctx context.Context, why string, stands int32) error {
	req := p.in.Status.Verification.Requested
	if req == nil {
		return nil
	}
	key := notRunKeyPrefix + strconv.FormatInt(req.CommentID, 10)
	body, err := templates.RenderVerifyCommandNotice(templates.VerifyCommandNotice{Namespace: p.in.Namespace,
		Intent: p.in.Name, Key: key, Actor: req.By, Verb: action.VerbVerify, Outcome: templates.VerifyNotRun,
		NotRunWhy: why, Stands: stands})
	return p.notice(ctx, key, req.At.Time, body, err)
}

// cannotRerun reports, and logs, an approver's pending request for a round
// that cannot run (reason, a PreviewVerification reason; why, what is
// missing) at heads whose judgement stands: keepJudgement consumes it.
func (p *pass) cannotRerun(ctx context.Context, reason v1alpha1.PreviewVerificationReason, why string) bool {
	v := p.in.Status.Verification
	stands := standingJudgement(v)
	if v.Requested == nil || stands == 0 {
		return false
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "the requested agents round cannot run; the judgement at these heads stands",
		slog.String("intent", p.in.Name), slog.Int("round", int(stands)), slog.String("reason", string(reason)),
		slog.String("detail", why))
	return true
}

// keepJudgement consumes the approver's pending request at heads whose
// judgement stands (standingJudgement), answering that it did not run
// (why) and changing nothing else.
func (p *pass) keepJudgement(ctx context.Context, why string) (bool, error) {
	if err := p.answerNotRun(ctx, why, standingJudgement(p.in.Status.Verification)); err != nil {
		return false, err
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Verification.Requested = nil
		return nil
	})
}
