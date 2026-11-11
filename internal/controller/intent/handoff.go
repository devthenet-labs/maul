// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/runnerguard"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The hand-off (DESIGN 7.1, 7.2; SPEC S1-20): when the humans get the
// preview link. Under a gated hand-off (the default, --intent-default-handoff
// or the Project's verification.handoff) an intent the agent evaluator
// verifies holds the link, and the word "ready", until the evaluation
// decides: it is released labelled "ready" once every outcome is confirmed
// (or accepted by an approver), or "maul couldn't confirm this" once maul
// gives up (unsure, an anomaly, out of rounds, its evaluation budget, an
// environment that failed, its sandbox not enforced) or handoffMax passes
// from when CI first went green. A human always wins: an approver's review
// or command on a pull request, a push maul did not make, or /maul preview
// releases it at once, and a human's feedback also halts automatic fixes.
// An early hand-off releases at once, and its texts are slice 1's.
//
// status.verification.handoff records the mode and the release, once; the
// label the link carries is derived from the verification each time it is
// shown (handoffView), so it follows a later judgement.

// handoffMode is the hand-off mode of p's intents: early for a Project
// with no verified evaluator, else the Project's, else the controller's
// default, gated when that is unset.
func (s Settings) handoffMode(p *v1alpha1.Project) v1alpha1.HandoffMode {
	if s.evaluatorUnverified(p) {
		return v1alpha1.HandoffEarly
	}
	def := s.DefaultHandoff
	if def == "" {
		def = v1alpha1.HandoffGated
	}
	return p.Spec.HandoffMode(def)
}

// handoffMax bounds a gated hand-off: the Project's verification.handoffMax,
// else --intent-handoff-max, else the default.
func (s Settings) handoffMax(p *v1alpha1.Project) time.Duration {
	if p.Spec.Verification != nil && p.Spec.Verification.HandoffMax != nil {
		return p.Spec.Verification.HandoffMax.Duration
	}
	if s.HandoffMax > 0 {
		return s.HandoffMax
	}
	return v1alpha1.DefaultHandoffMax
}

// handsOff reports an intent whose preview link the hand-off governs: the
// evaluator verifies it, and its approved plan has an outcome to evaluate.
func (p *pass) handsOff() bool {
	return p.mode() == modeOutcomes && evaluable(p.in.Status.Acceptance.Outcomes) && p.in.Status.Verification != nil
}

// handoffModeNow is the mode in force: the one recorded, else the
// settings'.
func (p *pass) handoffModeNow() v1alpha1.HandoffMode {
	if h := p.in.Status.Verification.Handoff; h != nil {
		return h.Mode
	}
	return p.set.handoffMode(p.proj)
}

// linkHeld reports a preview link held by a gated hand-off not released.
func (p *pass) linkHeld() bool {
	if !p.handsOff() || p.handoffModeNow() != v1alpha1.HandoffGated {
		return false
	}
	h := p.in.Status.Verification.Handoff
	return h == nil || !h.Released
}

// handoffView is the hand-off as the preview line and comments show it:
// nil when it changes nothing (no evaluator, an early hand-off); held while
// held; once released, ready while every outcome is confirmed, "couldn't
// confirm" (and why) once the evaluation gave up or ran out of time, and
// "still checking" otherwise.
func (p *pass) handoffView() *templates.PreviewHandoff {
	if p.set.agentsHostOff(p.proj) {
		return p.hostOffHandoffView()
	}
	if !p.handsOff() || p.handoffModeNow() != v1alpha1.HandoffGated {
		return nil
	}
	v, acc := p.in.Status.Verification, p.in.Status.Acceptance
	h := v.Handoff
	switch {
	case h == nil || !h.Released:
		return &templates.PreviewHandoff{State: templates.HandoffHeld}
	case !p.atHeads(v):
		// A judgement of other heads than the pull requests' now (SM-1):
		// maul checks the new ones once the head set follows.
		return &templates.PreviewHandoff{State: templates.HandoffChecking}
	case outcomesReady(acc, v):
		return &templates.PreviewHandoff{State: templates.HandoffReady}
	}
	if why := p.gaveUp(acc, v); why != "" {
		return unconfirmedView(acc, v, why)
	}
	if h.Reason == v1alpha1.HandoffReasonOutOfTime {
		return &templates.PreviewHandoff{State: templates.HandoffUnconfirmed, Why: templates.HandoffWhyTime}
	}
	return &templates.PreviewHandoff{State: templates.HandoffChecking}
}

// hostOffHandoffView is handoffView for a link-less Project whose agents
// host is switched off (S2-B6): nothing checks its intents now, so,
// whatever the hand-off's mode (a nil view would say maul checks them),
// it is ready only while every outcome is confirmed (or accepted), and
// otherwise maul couldn't confirm it, for why it gave up (at the heads it
// judged, a hand-off released out of time too) or else because it could
// not run its checks; never held or still checking. A round
// settled before the switch keeps its state (S2-B7) and its judgement,
// and an Off left by an earlier release keeps the judgement too, so it is
// ready while that judgement is (judgementReady) and the pull request is
// still at the heads it judged: nothing supersedes the record while the
// host is off, so a push is never shown ready. nil without a verification
// of the approved plan's outcomes.
func (p *pass) hostOffHandoffView() *templates.PreviewHandoff {
	v, acc := p.in.Status.Verification, p.in.Status.Acceptance
	if v == nil || p.mode() == modeNone || !evaluable(acc.Outcomes) {
		return nil
	}
	judged := p.atHeads(v)
	if judged && (outcomesReady(acc, v) || v.State == v1alpha1.VerificationOff && judgementReady(acc, v)) {
		return &templates.PreviewHandoff{State: templates.HandoffReady}
	}
	why := p.gaveUp(acc, v)
	if why == "" && judged && v.State == v1alpha1.VerificationOff {
		// An Off left at the heads its round judged keeps the cause that
		// round recorded for not running (settledWhy's).
		why = causeWhy(v.Cause)
	}
	if h := v.Handoff; why == "" && judged && h != nil && h.Reason == v1alpha1.HandoffReasonOutOfTime {
		// The reason on record at the heads it judged, as handoffView
		// says it: the checks ran, and the hand-off ran out of time.
		why = templates.HandoffWhyTime
	}
	if why == "" {
		why = templates.HandoffWhyUnavailable
	}
	return unconfirmedView(acc, v, why)
}

// gaveUp is why the evaluation settled without confirming every outcome
// and will not go on by itself (a templates.HandoffWhy constant), "" while
// it goes on (evaluating, waiting, a fix, a replay round or an escalation
// due) or is
// ready.
func (p *pass) gaveUp(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) string {
	if outcomesReady(acc, v) {
		return ""
	}
	if why := settledWhy(v); why != "" {
		return why
	}
	switch v.State {
	case v1alpha1.VerificationUnconfirmed:
		return p.unconfirmedWhy(acc, v)
	case v1alpha1.VerificationExhausted:
		return templates.HandoffWhyRounds
	case v1alpha1.VerificationFailing:
		if v.AutoFix.State != v1alpha1.AutoFixHalted {
			return ""
		}
		switch v.AutoFix.Reason {
		case v1alpha1.AutoFixCostCeiling:
			return templates.HandoffWhyBudget
		case v1alpha1.AutoFixMaxVerifyFixes, v1alpha1.AutoFixRepeatedFailure:
			return templates.HandoffWhyRounds
		}
		return templates.HandoffWhyFailing
	case v1alpha1.VerificationUnavailable, v1alpha1.VerificationNeedsReplan:
		if p.sandboxRefused() {
			return templates.HandoffWhySandbox
		}
		return templates.HandoffWhyUnavailable
	}
	return ""
}

// unconfirmedWhy is gaveUp for a round that judged and settled
// Unconfirmed: "" while its replay round is due.
func (p *pass) unconfirmedWhy(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) string {
	switch {
	case p.followUpDue(v):
		return "" // a dispute's round, or the escalation, is due
	case suspectedPending(v):
		// The defect maul suspects waits for a person: it leads, before the
		// outcomes in doubt and any anomaly (gate D-B11).
		return templates.HandoffWhySuspected
	case p.atEvaluationCeiling():
		return templates.HandoffWhyBudget
	case len(v.Disputed) > 0:
		return templates.HandoffWhyRounds
	case len(openAnomalies(v.Anomalies)) > 0 && !outcomesInDoubt(acc, v):
		// Every outcome confirmed or accepted: the anomaly is the reason
		// (review H2). With an outcome in doubt, the outcomes lead and the
		// anomaly comes second (gate D-B6).
		return templates.HandoffWhyAnomaly
	case environmentWhy(v) != "":
		return environmentWhy(v)
	case doubtedOutcomes(acc, v) > 0:
		return templates.HandoffWhyEvaluatorDoubts
	}
	return templates.HandoffWhyUnsure
}

// unconfirmedView is the hand-off maul could not confirm for why: with the
// outcomes maul suspects a defect in named first (gate D-B11), the
// outcomes the evaluator judged not to hold counted, and a look-around
// anomaly named after a reason that leads with the outcomes in doubt
// (gate D-B6).
func unconfirmedView(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification,
	why string) *templates.PreviewHandoff {
	h := &templates.PreviewHandoff{State: templates.HandoffUnconfirmed, Why: why}
	switch why {
	case templates.HandoffWhySuspected:
		if v.Suspected != nil {
			h.Suspected = slices.Clone(v.Suspected.Outcomes)
		}
		fallthrough
	case templates.HandoffWhyEvaluatorDoubts:
		h.Doubted = doubtedOutcomes(acc, v)
		fallthrough
	case templates.HandoffWhyEnvironment, templates.HandoffWhyInfra, templates.HandoffWhyUnsure:
		h.Anomaly = v.State == v1alpha1.VerificationUnconfirmed && len(openAnomalies(v.Anomalies)) > 0
	}
	return h
}

// doubtedOutcomes counts v's outcomes, of the judgement of its current
// round at its heads, that the evaluator judged not to hold but no check
// maul re-ran shows failing (acceptance.ReasonModelOnlyFail), less those
// an approver accepted. The reason is matched, never shown.
func doubtedOutcomes(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) int32 {
	var n int32
	for _, r := range v.JudgedOutcomes() {
		if r.Reason == acceptance.ReasonModelOnlyFail && effectiveResult(acc, v, r) == v1alpha1.OutcomeUnsure {
			n++
		}
	}
	return n
}

// outcomesInDoubt reports an approved outcome v's judgement at its heads
// does not confirm and no approver accepted.
func outcomesInDoubt(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) bool {
	return confirmedCount(acc, v) < int32(len(acc.Outcomes))
}

// settledWhy is why a round settled without a judgement, as the hand-off
// says it (causeWhy of the cause the verification recorded: no accepted
// image, trees not fetched, the head not built, its tool pod lost or its
// sandbox open, a replay cut short); "" for any other state, and for a
// cause the state's own words say (gaveUp).
func settledWhy(v *v1alpha1.IntentVerification) string {
	if v.State != v1alpha1.VerificationUnconfirmed && v.State != v1alpha1.VerificationUnavailable {
		return ""
	}
	return causeWhy(v.Cause)
}

// causeWhy is c's hand-off reason (templates.CauseWords), whatever the
// state; "" for SettleNone and SettlePreviewNotLive, which have none of
// their own. The reason is the same with or without a preview.
func causeWhy(c v1alpha1.SettleCause) string { return templates.CauseWords(c, false).Why }

// environmentWhy is why a judgement's environment kept it unconfirmed,
// whatever its outcomes' results (review H2): HandoffWhyInfra when maul's
// own machinery failed (the class Infra, or any class not known clean, or
// an outcome left unsure by it), which wins, as it does when the
// environment is classified; HandoffWhyEnvironment when a part of the
// change failed (AppFailure, or an outcome left unsure by it); "" for a
// clean one (review H3).
func environmentWhy(v *v1alpha1.IntentVerification) string {
	results := v.JudgedOutcomes()
	var infra, app bool
	if c := httpstep.EnvClass(v.OutcomesEnvironment); len(results) > 0 && c != "" && !c.Clean() {
		infra, app = c != httpstep.EnvAppFailure, c == httpstep.EnvAppFailure
	}
	for _, r := range results {
		switch r.Reason {
		case acceptance.ReasonInfra:
			infra = true
		case acceptance.ReasonEnvFailed:
			app = true
		}
	}
	switch {
	case infra:
		return templates.HandoffWhyInfra
	case app:
		return templates.HandoffWhyEnvironment
	}
	return ""
}

// sandboxRefused reports that the latest evaluate run ended because its
// sandbox was not enforced (the prepare init's strict probe).
func (p *pass) sandboxRefused() bool {
	for _, run := range p.runs {
		if run.Spec.Stage == v1alpha1.IntentStageEvaluate && run.Status.Detail == runnerguard.SandboxReason {
			return true
		}
	}
	return false
}

// handoffDue is the reason a held link of acc and v is released now: ready,
// once maul gave up, or once handoffMax passed from CI's first green; ""
// while it is held.
func (p *pass) handoffDue(acc *v1alpha1.IntentAcceptance, v *v1alpha1.IntentVerification) v1alpha1.HandoffReason {
	switch {
	case outcomesReady(acc, v):
		return v1alpha1.HandoffReasonReady
	case suspectedPending(v):
		// A suspected defect waits for a person (fourth review): held, as
		// long as the hand-off's own time bound allows.
		if v.FirstGreenAt != nil && p.now.Sub(v.FirstGreenAt.Time) >= p.set.handoffMax(p.proj) {
			return v1alpha1.HandoffReasonOutOfTime
		}
		return ""
	case p.gaveUp(acc, v) != "":
		return v1alpha1.HandoffReasonUnconfirmed
	case v.FirstGreenAt != nil && p.now.Sub(v.FirstGreenAt.Time) >= p.set.handoffMax(p.proj):
		return v1alpha1.HandoffReasonOutOfTime
	}
	return ""
}

// syncHandoff releases a held link when it is due (handoffDue). It reports
// whether it wrote. A human's release is the halt's (haltHuman,
// releaseForHuman, releaseForPush): in the same write as the human's round
// or push, or as the halt that waits for its quiet window.
func (p *pass) syncHandoff(ctx context.Context) (bool, error) {
	if !p.handsOff() {
		return false, nil
	}
	if !p.linkHeld() {
		return false, nil
	}
	reason := p.handoffDue(p.in.Status.Acceptance, p.in.Status.Verification)
	if reason == "" {
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "the preview link is handed over",
		slog.String("intent", p.in.Name), slog.String("reason", string(reason)))
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		// A link held before any evaluation recorded its hand-off (a round
		// that could not start: no Preview in time, no accepted image, no
		// trees) gets its record here, so the release is written once and
		// the next pass goes on, never rewriting a hand-off that is not
		// there.
		p.recordHeld(cur.Status.Verification)
		releaseHandoff(cur.Status.Verification, reason, metav1.NewTime(p.now), false)
		return nil
	})
}

// startHandoff records, as the intent's first evaluation is created (CI
// green at its first heads, the gate passed), the hand-off's mode and the
// start of its handoffMax clock (status.verification.firstGreenAt); an
// early hand-off is released at once. Later evaluations change neither.
func (p *pass) startHandoff(v *v1alpha1.IntentVerification) {
	now := metav1.NewTime(p.now)
	if v.FirstGreenAt == nil {
		v.FirstGreenAt = &now
	}
	if v.Handoff != nil {
		return
	}
	v.Handoff = &v1alpha1.HandoffState{Mode: p.set.handoffMode(p.proj)}
	if v.Handoff.Mode == v1alpha1.HandoffEarly {
		releaseHandoff(v, v1alpha1.HandoffReasonEarly, now, true)
	}
}

// releaseIfDue releases cur's held link in a write that changed its
// verification (a judgement), when the change makes it due: the hand-off
// follows the judgement at once, never a pass later.
func (p *pass) releaseIfDue(cur *v1alpha1.Intent) {
	v := cur.Status.Verification
	if v == nil || v.Handoff == nil || v.Handoff.Released || v.Handoff.Mode != v1alpha1.HandoffGated {
		return
	}
	if reason := p.handoffDue(cur.Status.Acceptance, v); reason != "" {
		releaseHandoff(v, reason, metav1.NewTime(p.now), false)
	}
}

// releaseHandoff releases v's held link for reason at now, once. early
// releases a hand-off of either mode; otherwise only a gated one.
func releaseHandoff(v *v1alpha1.IntentVerification, reason v1alpha1.HandoffReason, now metav1.Time, early bool) {
	h := v.Handoff
	if h == nil || h.Released || (!early && h.Mode != v1alpha1.HandoffGated) {
		return
	}
	at := now
	h.Released, h.ReleasedAt, h.Reason = true, &at, reason
}

// haltHuman halts v's automatic fixes for reason, a human's own action
// (humanHalt), at p.now, releasing a held link at once: a link held before
// the first evaluation recorded its hand-off (startHandoff) gets its record
// here, so the release stands, and startHandoff then keeps it.
func (p *pass) haltHuman(v *v1alpha1.IntentVerification, reason v1alpha1.AutoFixReason) {
	p.recordHeld(v)
	halt(v, reason, metav1.NewTime(p.now))
}

// releaseForPush releases v's held link for a push maul did not make that
// automatic fixes go on through (an approver re-armed them after it): the
// human wins the link either way.
func (p *pass) releaseForPush(v *v1alpha1.IntentVerification) {
	p.recordHeld(v)
	releaseHandoff(v, v1alpha1.HandoffReasonHuman, metav1.NewTime(p.now), false)
}

// recordHeld gives v the record of the gated hand-off holding p's link
// when none is recorded yet: before the first evaluation the link is held
// (linkHeld) with nothing recorded to release.
func (p *pass) recordHeld(v *v1alpha1.IntentVerification) {
	if v != nil && v.Handoff == nil && p.linkHeld() {
		v.Handoff = &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated}
	}
}

// humanHalt reports a halt a human's own action caused: their feedback, a
// push, a /maul verify stop. It releases a held link (halt).
func humanHalt(reason v1alpha1.AutoFixReason) bool {
	switch reason {
	case v1alpha1.AutoFixHumanReview, v1alpha1.AutoFixHumanCommand, v1alpha1.AutoFixHumanPush,
		v1alpha1.AutoFixHumanStop:
		return true
	}
	return false
}

// keepHandoff is what of a hand-off a new approved plan revision keeps: a
// released one stays released (the humans have the link); a held one
// starts over.
func keepHandoff(h *v1alpha1.HandoffState) *v1alpha1.HandoffState {
	if h == nil || !h.Released {
		return nil
	}
	return h.DeepCopy()
}
