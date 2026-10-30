// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/acceptance/crdconv"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The verification loop (acceptance verification, slice 1), with
// --intent-verification on. In review, and in a round of it, every pass:
//
//  1. records the approved plan's acceptance criteria once per approved
//     plan revision (syncAcceptance), from the plan bytes re-read under the
//     approval's digest, normalised, digested as approved; and re-arms
//     automatic fixes and the per-revision bounds. Under the agent
//     evaluator (Settings.evaluates) it records the plan's outcomes
//     instead, and a plan stating slice-1 criteria is not verified: the
//     intent gets one notice asking for a replan (stopForReplan, DESIGN
//     7.6), the one GitHub write here;
//  2. reports Off for a Project (or Preview) that does not verify and
//     NotDeclared for a plan with nothing to run, consuming any request
//     (the latter still records the head set an acceptance binds to);
//  3. halts automatic fixes once a human round of the approved revision
//     started after the latest /maul verify fix re-armed them;
//  4. supersedes (syncHeadSet): when the combined change moved, the
//     in-flight PreviewVerification is deleted and the state starts over at
//     the new heads, and a head maul did not push halts automatic fixes;
//  5. reads the latest PreviewVerification (one uncached get) and judges
//     what it observed (acceptance.Judge), or retries maul's own failure as
//     the round's next attempt, at most MaxVerificationAttempts, then
//     reports it Unavailable.
//
// In review with no round started (startRound's last step) the gate
// (startVerification) creates the next PreviewVerification when one is due
// (verifyDue): the Preview live at exactly the heads, every named check
// green, within the bounds. Nothing else here writes to GitHub or starts a round:
// the commit statuses and comments, and the verify-fix rounds, read the
// state this writes.

// reasonCriteriaUnreadable is the VerifyFailing reason of an Intent whose
// approved plan revision's acceptance criteria cannot be read: nothing is
// verified until they can, or a replan is approved.
const reasonCriteriaUnreadable = "CriteriaUnreadable"

// reasonCriteriaReplan is the VerifyFailing reason of an Intent whose
// approved plan states slice-1 criteria while the agent evaluator verifies
// its Project: nothing is verified until a plan stating outcomes is
// approved (DESIGN 7.6).
const reasonCriteriaReplan = "CriteriaReplan"

// syncVerification is the per-pass part of the loop: steps 1 to 5 above.
// It reports whether it wrote the Intent's status, which ends the pass.
// Failures to read what it judges from are logged and verify nothing this
// pass; they never hold up the human's rounds.
func (p *pass) syncVerification(ctx context.Context) (bool, error) {
	if !p.set.Verification || p.in.Status.Approval == nil {
		return false, nil
	}
	if changed, err := p.syncAcceptance(ctx); changed || err != nil {
		return changed, err
	}
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	if !p.acceptanceCurrent() || v == nil {
		// Criteria on record from another plan revision are never run
		// against the approved one's code.
		return false, nil
	}
	if !p.verifies() {
		return p.settleState(ctx, v1alpha1.VerificationOff)
	}
	if p.set.evaluates(p.proj) {
		// The agent evaluator's outcomes: its own rounds (evaluate_round.go).
		return p.syncEvaluation(ctx)
	}
	if len(automatable(acc.Criteria)) == 0 {
		// Nothing runs, but an approver's acceptance binds to the head set,
		// so it is tracked all the same.
		if hs, ok := previewHeadSet(p.proj, p.in); ok {
			if changed, err := p.syncHeadSet(ctx, hs, v1alpha1.VerificationNotDeclared); changed || err != nil {
				return changed, err
			}
		}
		return p.settleState(ctx, v1alpha1.VerificationNotDeclared)
	}
	if reason := p.humanRoundHalt(); reason != "" {
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			p.haltHuman(cur.Status.Verification, reason)
			return nil
		})
	}
	hs, ok := previewHeadSet(p.proj, p.in)
	if !ok {
		return false, nil
	}
	if changed, err := p.syncHeadSet(ctx, hs, v1alpha1.VerificationWaitingForCI); changed || err != nil {
		return changed, err
	}
	return p.settleVerification(ctx)
}

// syncAcceptance records the approved plan revision's acceptance criteria,
// once: an approved plan's own, re-read under the approval's digest. A new
// revision starts verification afresh (H11): automatic fixes armed, the
// per-revision bounds and the head set cleared, its in-flight request
// deleted; the round numbering, the reset generation and an approver's
// pending request carry over. A plan that cannot be read or no longer
// matches its digest records nothing (logged) and stops verification
// (stopUnreadable): the criteria on record, if any, are another revision's.
func (p *pass) syncAcceptance(ctx context.Context) (bool, error) {
	ap := p.in.Status.Approval
	if p.acceptanceCurrent() {
		return false, nil
	}
	rec, err := p.approvedAcceptance(ctx, ap)
	if errors.Is(err, report.ErrSliceOneCriteria) {
		return p.stopForReplan(ctx, ap)
	}
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the approved plan's acceptance criteria; nothing is verified",
			slog.String("intent", p.in.Name), slog.Int("plan_revision", int(ap.PlanRevision)), slog.Any("error", err))
		return p.stopUnreadable(ctx, ap)
	}
	if v := p.in.Status.Verification; v != nil && v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		if c := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionVerifyFailing); c != nil &&
			(c.Reason == reasonCriteriaUnreadable || c.Reason == reasonCriteriaReplan) {
			meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionVerifyFailing)
		}
		cur.Status.Acceptance = rec
		next := &v1alpha1.IntentVerification{}
		if prev := cur.Status.Verification; prev != nil {
			next.Round, next.Reset, next.Requested, next.CancelRun = prev.Round, prev.Reset, prev.Requested,
				prev.CancelRun
			// A link the humans have is never held again; the hand-off's
			// clock runs from the intent's first green.
			next.Handoff, next.FirstGreenAt = keepHandoff(prev.Handoff), prev.FirstGreenAt
		}
		since := metav1.NewTime(p.now)
		next.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed, Since: &since}
		cur.Status.Verification = next
		return nil
	})
}

// acceptanceCurrent reports that the criteria on record are the approved
// plan revision's: the only ones anything is verified or accepted against.
// Under the agent evaluator, criteria slice 1 recorded are not: they are
// read again (syncAcceptance), so an open intent approved before the
// evaluator was switched on is asked for a replan.
func (p *pass) acceptanceCurrent() bool {
	acc, ap := p.in.Status.Acceptance, p.in.Status.Approval
	return acc != nil && ap != nil && acc.PlanRevision == ap.PlanRevision &&
		(len(acc.Criteria) == 0 || !p.set.evaluates(p.proj))
}

// verifyStopped is why verification of the approved plan revision is
// stopped with nothing on record ("" when it is not): its criteria cannot
// be read (stopUnreadable, reasonCriteriaUnreadable), or they are slice-1
// criteria the agent evaluator does not run (stopForReplan,
// reasonCriteriaReplan). The verification and the condition say which.
func (p *pass) verifyStopped() string {
	v := p.in.Status.Verification
	if p.acceptanceCurrent() || v == nil || v.State != v1alpha1.VerificationUnavailable {
		return ""
	}
	c := meta.FindStatusCondition(p.in.Status.Conditions, v1alpha1.ConditionVerifyFailing)
	if c == nil || c.Status != metav1.ConditionTrue ||
		(c.Reason != reasonCriteriaUnreadable && c.Reason != reasonCriteriaReplan) {
		return ""
	}
	return c.Reason
}

// criteriaUnreadable reports an Intent stopUnreadable stopped: the approved
// plan revision's criteria are not on record, and the verification says
// why.
func (p *pass) criteriaUnreadable() bool { return p.verifyStopped() == reasonCriteriaUnreadable }

// stopUnreadable stops verification of an approved plan revision whose
// criteria cannot be read: the in-flight request is deleted, any waiting
// request consumed, automatic fixes halted, the state Unavailable, and the
// Intent's VerifyFailing condition says why, once. Nothing is verified,
// judged or accepted against the criteria of another revision; the status
// and the comments report it (criteriaUnreadable). A later pass that reads
// the criteria records them and starts afresh.
func (p *pass) stopUnreadable(ctx context.Context, ap *v1alpha1.IntentApproval) (bool, error) {
	if v := p.in.Status.Verification; p.criteriaUnreadable() && v.Ref == nil && v.Requested == nil {
		return false, nil
	}
	if v := p.in.Status.Verification; v != nil && v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		next := &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnavailable}
		if prev := cur.Status.Verification; prev != nil {
			next.Round, next.Reset, next.CancelRun = prev.Round, prev.Reset, prev.CancelRun
		}
		halt(next, v1alpha1.AutoFixCriterionError, metav1.NewTime(p.now))
		cur.Status.Verification = next
		setCondition(cur, v1alpha1.ConditionVerifyFailing, metav1.ConditionTrue, reasonCriteriaUnreadable,
			fmt.Sprintf("the acceptance criteria of the approved plan r%d cannot be read, so nothing is verified; "+
				"the plan cannot change while its pull requests are open: /maul cancel and a new issue start over",
				ap.PlanRevision))
		return nil
	})
}

// approvedAcceptance reads the approved plan's bytes from its ConfigMap,
// holds them to the approval's digest, and returns its acceptance criteria
// as the Intent records them: normalised, with the digest of the list as
// the plan declared it. Under the agent evaluator it returns the plan's
// outcomes the same way (normalised, OutcomesDigest), or, for a plan
// stating slice-1 criteria, report.ErrSliceOneCriteria.
func (p *pass) approvedAcceptance(ctx context.Context, ap *v1alpha1.IntentApproval) (*v1alpha1.IntentAcceptance,
	error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: p.in.Namespace, Name: planConfigMapName(p.in.Name, ap.PlanRevision)}
	if err := p.r.APIReader.Get(ctx, key, &cm); err != nil {
		return nil, fmt.Errorf("read plan r%d: %w", ap.PlanRevision, err)
	}
	raw := []byte(cm.Data[keyPlan])
	if got := digest(raw); got != ap.PlanDigest {
		return nil, fmt.Errorf("plan r%d: %w: %s holds %s, not the approved %s", ap.PlanRevision, errPlanChanged,
			cm.Name, got, ap.PlanDigest)
	}
	parsed, err := report.ParsePlan(raw)
	if err != nil {
		return nil, fmt.Errorf("parse plan r%d: %w", ap.PlanRevision, err)
	}
	if p.set.evaluates(p.proj) {
		if err := parsed.CheckEvaluator(); err != nil {
			return nil, fmt.Errorf("plan r%d: %w", ap.PlanRevision, err)
		}
		outcomes, err := crdconv.OutcomesToCRD(acceptance.NormalizeOutcomes(parsed.Outcomes))
		if err != nil {
			return nil, err
		}
		return &v1alpha1.IntentAcceptance{PlanRevision: ap.PlanRevision, Digest: acceptance.Digest(nil),
			Outcomes: outcomes, OutcomesDigest: acceptance.OutcomesDigest(parsed.Outcomes)}, nil
	}
	criteria, err := crdconv.CriteriaToCRD(acceptance.Normalize(parsed.Acceptance))
	if err != nil {
		return nil, err
	}
	return &v1alpha1.IntentAcceptance{PlanRevision: ap.PlanRevision, Digest: acceptance.Digest(parsed.Acceptance),
		Criteria: criteria}, nil
}

// stopForReplan stops verification of an approved plan revision that
// states slice-1 criteria while the agent evaluator verifies the Project
// (DESIGN 7.6): it posts, once, the notice asking for a replan (keyed by the
// revision, so a retry or a restart finds the one it posted), then deletes
// the in-flight request, consumes any waiting one, drops the criteria on
// record (the evaluator never runs them; off again, slice 1 reads them
// afresh), halts automatic fixes and reports Unavailable with the Intent's
// VerifyFailing condition saying why. Nothing is verified until a new plan
// is approved, whose outcomes syncAcceptance then records.
func (p *pass) stopForReplan(ctx context.Context, ap *v1alpha1.IntentApproval) (bool, error) {
	if v := p.in.Status.Verification; p.verifyStopped() == reasonCriteriaReplan && v.Ref == nil &&
		v.Requested == nil {
		return false, nil
	}
	key := templates.CriteriaReplanKey(ap.PlanRevision)
	body, err := templates.RenderCriteriaReplanNotice(templates.CriteriaReplanNotice{Namespace: p.in.Namespace,
		Intent: p.in.Name, Key: key, PlanRevision: ap.PlanRevision, TriggerLabel: p.trigger()})
	if err := p.notice(ctx, key, ap.At.Time, body, err); err != nil {
		return false, err
	}
	if v := p.in.Status.Verification; v != nil && v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		next := &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnavailable}
		if prev := cur.Status.Verification; prev != nil {
			next.Round, next.Reset, next.CancelRun = prev.Round, prev.Reset, prev.CancelRun
		}
		halt(next, v1alpha1.AutoFixCriterionError, metav1.NewTime(p.now))
		cur.Status.Acceptance = nil
		cur.Status.Verification = next
		setCondition(cur, v1alpha1.ConditionVerifyFailing, metav1.ConditionTrue, reasonCriteriaReplan,
			fmt.Sprintf("the approved plan r%d states acceptance criteria, which the agent evaluator does not "+
				"run, so nothing is verified; the plan cannot change while its pull requests are open: /maul cancel "+
				"and a new issue start over with outcomes", ap.PlanRevision))
		return nil
	})
}

// settleState records a state nothing runs in (Off, NotDeclared), consuming
// any pending request: there is nothing it could run.
func (p *pass) settleState(ctx context.Context, state v1alpha1.IntentVerificationState) (bool, error) {
	v := p.in.Status.Verification
	if v.State == state && v.Requested == nil {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Verification.State = state
		cur.Status.Verification.Requested = nil
		return nil
	})
}

// halt halts automatic fixes for reason, unless they are halted already. A
// human's own action (humanHalt) also releases a held preview link: the
// human wins (handoff.go).
func halt(v *v1alpha1.IntentVerification, reason v1alpha1.AutoFixReason, now metav1.Time) {
	if humanHalt(reason) {
		releaseHandoff(v, v1alpha1.HandoffReasonHuman, now, false)
	}
	if v.AutoFix.State == v1alpha1.AutoFixHalted {
		return
	}
	since := now
	v.AutoFix.State, v.AutoFix.Reason, v.AutoFix.Since = v1alpha1.AutoFixHalted, reason, &since
}

// humanRoundHalt is why automatic fixes must halt for a human round: a
// review or command round of the approved plan revision later than the
// round an approver's /maul verify fix re-armed them at (H10). "" when
// they are halted already or no such round exists. The latest such round
// names the reason.
func (p *pass) humanRoundHalt() v1alpha1.AutoFixReason {
	v, ap := p.in.Status.Verification, p.in.Status.Approval
	if v == nil || ap == nil || v.AutoFix.State == v1alpha1.AutoFixHalted {
		return ""
	}
	var after int32
	if v.AutoFix.Rearm != nil {
		after = v.AutoFix.Rearm.Round
	}
	var reason v1alpha1.AutoFixReason
	latest := after
	for _, run := range p.runs {
		if run.Spec.Stage != v1alpha1.IntentStageRevise || run.Spec.Inputs.PlanRevision != ap.PlanRevision ||
			run.Spec.Round <= latest {
			continue
		}
		// maul's own rounds (checks, verify) are never a human's.
		if r := humanReason(run.Spec.Trigger); r != "" {
			reason, latest = r, run.Spec.Round
		}
	}
	return reason
}

// syncHeadSet supersedes the recorded verification when the combined
// change moved to hs (§4.7): the in-flight PreviewVerification is deleted,
// the state starts over at hs in state (waiting for its checks, or
// NotDeclared for a plan with nothing to run), and a component
// whose revision changed to a head maul did not push halts automatic fixes
// (HumanPush) and releases a held link, unless an approver re-armed them
// after GitHub recorded that push (rearmedAfter): then it releases the
// link alone. The commit statuses are re-posted on every head from the new
// head-set digest.
func (p *pass) syncHeadSet(ctx context.Context, hs headSet, state v1alpha1.IntentVerificationState) (bool, error) {
	v := p.in.Status.Verification
	if v.HeadSetDigest == hs.digest {
		return false, nil
	}
	pushed := p.humanPushes(v.Heads, hs.heads)
	rearmed, err := p.rearmedAfter(ctx, pushed)
	if err != nil {
		return false, err
	}
	if v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State = state
		nv.HeadSetDigest, nv.Heads = hs.digest, slices.Clone(hs.heads)
		nv.Ref, nv.Attempt, nv.InfraRetries = nil, 0, 0
		// A no-defect conclusion is about the heads it judged.
		nv.NoDefect = nil
		clearJudgement(nv)
		switch {
		case len(pushed) == 0:
		case rearmed:
			p.releaseForPush(nv)
		default:
			p.haltHuman(nv, v1alpha1.AutoFixHumanPush)
		}
		countManual(nv, cur.Status.Acceptance)
		return nil
	})
}

// humanPushes are the components of heads whose revision is not what it
// was in old and is not the head maul last pushed in its repository: a
// human pushed to its pull request's branch. A component with no pull
// request runs its recorded default-branch head, which no one pushes to
// through the intent.
func (p *pass) humanPushes(old, heads []v1alpha1.VerificationHead) []v1alpha1.VerificationHead {
	var pushed []v1alpha1.VerificationHead
	for _, h := range heads {
		if slices.ContainsFunc(old, func(o v1alpha1.VerificationHead) bool {
			return o.Component == h.Component && o.Revision == h.Revision
		}) {
			continue
		}
		if p.pullRequest(h.Repository) == nil {
			continue
		}
		if latest := p.latestPushedRun(h.Repository); latest == nil || latest.Status.PushedCommit != h.Revision {
			pushed = append(pushed, h)
		}
	}
	return pushed
}

// rearmedAfter reports that automatic fixes are armed by an approver's
// /maul verify fix posted after GitHub recorded the push of every head in
// pushed: the approver re-armed them with those pushes in place, so the
// pushes do not halt them again. maul sees a push only at its next poll,
// which can come after a re-arm made seconds after the push (S1 demo,
// bug 2).
//
// The order is GitHub's alone: the push's time from the repository
// activity API (GitHub.PushedAt) against the command comment's created_at,
// both stamped by GitHub, so no clock skew of maul's enters it, and no
// commit date, which the pusher sets, does. A push in the re-arm's second,
// a head GitHub shows no push to, or a refusal to show it halts: the human
// wins. An error that may pass is returned, deciding nothing.
func (p *pass) rearmedAfter(ctx context.Context, pushed []v1alpha1.VerificationHead) (bool, error) {
	af := p.in.Status.Verification.AutoFix
	if len(pushed) == 0 || af.State == v1alpha1.AutoFixHalted || af.Rearm == nil {
		return false, nil
	}
	branch := branchName(p.in.Name)
	for _, h := range pushed {
		at, ok, err := p.r.GitHub.PushedAt(ctx, h.Repository, branch, h.Revision)
		if ghclient.IsNotFound(err) || ghclient.IsForbidden(err) {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "GitHub would not show when a head was pushed; the push halts "+
				"automatic fixes", slog.String("intent", p.in.Name), slog.String("repository", h.Repository),
				slog.String("revision", h.Revision), slog.Any("error", err))
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read when %s was pushed to %s: %w", h.Revision, h.Repository, err)
		}
		if !ok || !af.Rearm.At.After(at) {
			return false, nil
		}
	}
	return true, nil
}

// clearJudgement drops a verification's judged result.
func clearJudgement(v *v1alpha1.IntentVerification) {
	v.Passed, v.Failed, v.Quarantined, v.NotRun = 0, 0, 0, 0
	v.Failing, v.QuarantinedIDs = nil, nil
}

// countManual counts the manual criteria (or outcomes) accepted at v's head
// set and those still open, and the criteria this slice cannot run.
func countManual(v *v1alpha1.IntentVerification, acc *v1alpha1.IntentAcceptance) {
	v.ManualOpen, v.ManualAccepted, v.NotRun = 0, 0, 0
	if acc == nil {
		return
	}
	for _, o := range acc.Outcomes {
		switch {
		case !o.Manual:
		case outcomeAccepted(acc, o.ID, v.HeadSetDigest):
			v.ManualAccepted++
		default:
			v.ManualOpen++
		}
	}
	for _, c := range acc.Criteria {
		switch c.Kind {
		case v1alpha1.AcceptanceManual:
			if slices.ContainsFunc(acc.Accepted, func(r v1alpha1.AcceptanceRecord) bool {
				return r.ID == c.ID && r.HeadSetDigest == v.HeadSetDigest
			}) {
				v.ManualAccepted++
			} else {
				v.ManualOpen++
			}
		case v1alpha1.AcceptanceCommand:
			v.NotRun++
		case v1alpha1.AcceptanceHTTP:
		}
	}
}

// settleVerification reads the latest PreviewVerification while it is in
// flight and settles it once preview-controller has: judged (Observed, or
// EnvFailed charged to the application), or, for an attempt that ended
// without a judgement, the round's next attempt, or Unavailable after the
// last.
func (p *pass) settleVerification(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	if v.Ref == nil || v.State != v1alpha1.VerificationVerifying {
		return false, nil
	}
	pv, err := p.readVerification(ctx, *v.Ref)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "read the intent's verification; judged next pass",
			slog.String("intent", p.in.Name), slog.String("verification", v.Ref.Name), slog.Any("error", err))
		return false, nil
	}
	if pv != nil && (pv.Spec.HeadSetDigest != v.HeadSetDigest ||
		pv.Spec.CriteriaDigest != p.in.Status.Acceptance.Digest) {
		p.dropInFlight(ctx, *v.Ref)
		pv = nil
	}
	if pv == nil {
		return true, p.unjudged(ctx)
	}
	switch pv.Status.Phase {
	case v1alpha1.VerificationObserved:
		j := judge(p.in.Status.Acceptance.Criteria, pv)
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			j.apply(cur.Status.Verification)
			countManual(cur.Status.Verification, cur.Status.Acceptance)
			return nil
		})
	case v1alpha1.VerificationEnvFailed:
		parts := crdconv.PartFacts(pv.Status.Environment)
		if acceptance.ClassifyEnvironment(parts) == acceptance.EnvAppFailure {
			return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
				nv := cur.Status.Verification
				clearJudgement(nv)
				nv.State = v1alpha1.VerificationFailing
				countManual(nv, cur.Status.Acceptance)
				return nil
			})
		}
		return true, p.unjudged(ctx)
	case v1alpha1.VerificationInfra, v1alpha1.VerificationStale:
		return true, p.unjudged(ctx)
	case v1alpha1.VerificationPending, v1alpha1.VerificationRunning, "":
		return false, nil
	}
	return false, nil
}

// unjudged settles an attempt that ended without a judgement: maul's own
// failure, a request the Preview no longer matched, or one that vanished.
// It is never charged to the intent: the round's next attempt is due
// (verifyRetry), or after the last the verification is Unavailable until
// the heads move or an approver asks again.
func (p *pass) unjudged(ctx context.Context) error {
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		clearJudgement(nv)
		countManual(nv, cur.Status.Acceptance)
		if nv.Attempt >= v1alpha1.MaxVerificationAttempts {
			nv.State, nv.UnavailableReason = v1alpha1.VerificationUnavailable, ""
			return nil
		}
		nv.Ref = nil
		nv.InfraRetries++
		return nil
	})
}

// readVerification reads the PreviewVerification ref names, uncached. nil
// when it is gone or is not this Intent's (its UID, its controller owner,
// its intentRef): another's is never judged or deleted.
func (p *pass) readVerification(ctx context.Context, ref v1alpha1.ObjectReference) (*v1alpha1.PreviewVerification,
	error) {
	var pv v1alpha1.PreviewVerification
	err := p.r.APIReader.Get(ctx, types.NamespacedName{Namespace: p.in.Namespace, Name: ref.Name}, &pv)
	switch {
	case kerrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if (ref.UID != "" && pv.UID != ref.UID) || !controlledBy(pv.OwnerReferences, p.in.UID) ||
		pv.Spec.IntentRef.UID != p.in.UID {
		return nil, nil
	}
	return &pv, nil
}

// dropInFlight deletes the PreviewVerification ref names when it is this
// Intent's and has not settled: superseded, its result can no longer mean
// anything. One that settled is kept for its evidence (retention deletes
// it). Best effort: a failure is logged, and preview-controller marks a
// request its Preview no longer matches Stale anyway.
func (p *pass) dropInFlight(ctx context.Context, ref v1alpha1.ObjectReference) {
	pv, err := p.readVerification(ctx, ref)
	if err != nil || pv == nil {
		return
	}
	switch pv.Status.Phase {
	case v1alpha1.VerificationObserved, v1alpha1.VerificationEnvFailed, v1alpha1.VerificationInfra,
		v1alpha1.VerificationStale:
		return
	case v1alpha1.VerificationPending, v1alpha1.VerificationRunning, "":
	}
	p.deleteVerification(ctx, pv)
}

// deleteVerification deletes pv, pinned by UID; best effort.
func (p *pass) deleteVerification(ctx context.Context, pv *v1alpha1.PreviewVerification) {
	uid := pv.UID
	if err := p.r.Delete(ctx, pv, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil &&
		!kerrors.IsNotFound(err) && !kerrors.IsConflict(err) {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "delete a verification",
			slog.String("intent", p.in.Name), slog.String("verification", pv.Name), slog.Any("error", err))
	}
}

// judgement is acceptance.Judge's verdicts on a verification's
// observations, counted.
type judgement struct {
	passed, failed, quarantined int32
	failing                     []v1alpha1.CriterionState
	quarantinedIDs              []string
}

// judge judges pv's observations of the approved automatable criteria in
// plan order. Each criterion is judged against its own one observation,
// under pv's run tokens; a criterion with no observation, or more than one,
// or one that does not convert, fails (acceptance.Judge's id check).
func judge(approved []v1alpha1.AcceptanceCriterion, pv *v1alpha1.PreviewVerification) judgement {
	var j judgement
	rt := acceptance.RunTokens{Round: pv.Spec.Round, Attempt: pv.Spec.Attempt, Run: pv.Spec.Run}
	for _, c := range automatable(approved) {
		v := judgeOne(c, pv.Status.Observations, rt)
		switch v.Outcome {
		case acceptance.OutcomePass:
			j.passed++
		case acceptance.OutcomeQuarantined:
			j.quarantined++
			j.quarantinedIDs = append(j.quarantinedIDs, c.ID)
		case acceptance.OutcomeFail:
			j.failed++
			j.failing = append(j.failing, v1alpha1.CriterionState{ID: c.ID, Outcome: v1alpha1.VerdictFail,
				Fingerprint: acceptance.Fingerprint([]acceptance.Verdict{v}, nil)})
		}
	}
	return j
}

// judgeOne judges one criterion against its observation in obs.
func judgeOne(c v1alpha1.AcceptanceCriterion, obs []v1alpha1.CriterionObservation,
	rt acceptance.RunTokens) acceptance.Verdict {
	failed := acceptance.Verdict{ID: c.ID, Outcome: acceptance.OutcomeFail, Reason: acceptance.ReasonNotObservable}
	cs, err := crdconv.CriteriaFromCRD([]v1alpha1.AcceptanceCriterion{c})
	if err != nil || len(cs) != 1 {
		return failed
	}
	var found []v1alpha1.CriterionObservation
	for _, o := range obs {
		if o.ID == c.ID {
			found = append(found, o)
		}
	}
	var o acceptance.CriterionObservation
	if len(found) == 1 {
		if converted, err := crdconv.ObservationFromCRD(found[0]); err == nil {
			o = converted
		}
	}
	return acceptance.Judge(cs[0], o, rt)
}

// apply writes j onto v as its judged state: Passed when nothing failed
// (quarantined criteria included, never counted as passed), else Failing.
func (j judgement) apply(v *v1alpha1.IntentVerification) {
	v.Passed, v.Failed, v.Quarantined = j.passed, j.failed, j.quarantined
	v.Failing, v.QuarantinedIDs = j.failing, j.quarantinedIDs
	v.State = v1alpha1.VerificationPassed
	if j.failed > 0 {
		v.State = v1alpha1.VerificationFailing
	}
}

// startVerification is the gate (startRound's last step, nothing else
// started): when a verification is due at the recorded head set, it checks
// the criteria can run as approved, the Preview is live at exactly the
// heads and every named check is green, and creates the PreviewVerification;
// otherwise it records what it waits for.
func (p *pass) startVerification(ctx context.Context, prs []v1alpha1.IntentPullRequest) (bool, error) {
	if !p.set.Verification || !p.verifies() {
		return false, nil
	}
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	if !p.acceptanceCurrent() || v == nil {
		return false, nil
	}
	if p.set.evaluates(p.proj) {
		return p.startEvaluation(ctx, prs)
	}
	criteria := automatable(acc.Criteria)
	hs, ok := previewHeadSet(p.proj, p.in)
	if len(criteria) == 0 || !ok || hs.digest != v.HeadSetDigest {
		return false, nil
	}
	kind, err := p.verifyDue(ctx, v)
	if err != nil || kind == verifyNotDue {
		return false, err
	}
	if kind == verifyRequestRefused {
		p.r.log().LogAttrs(ctx, slog.LevelInfo, "maxVerifications is spent; the verification request is dropped",
			slog.String("intent", p.in.Name), slog.Int64("comment", v.Requested.CommentID))
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Verification.Requested = nil
			return nil
		})
	}
	if id := unrunnable(criteria, hs); id != "" {
		return true, p.needsReplan(ctx, id)
	}
	preview := p.livePreview(ctx, v)
	if preview == nil {
		return p.waitPreview(ctx)
	}
	green, err := p.checksGreen(ctx, prs)
	if err != nil || !green {
		if err != nil {
			return false, err
		}
		return p.waitVerification(ctx, v1alpha1.VerificationWaitingForCI)
	}
	return true, p.createVerification(ctx, kind, preview, hs, v1alpha1.VerificationVerifying,
		func(spec *v1alpha1.PreviewVerificationSpec) error {
			spec.Criteria, spec.CriteriaDigest = criteria, p.in.Status.Acceptance.Digest
			spec.Timeout = metav1.Duration{Duration: p.set.verifyTimeout(p.proj)}
			return nil
		})
}

// waitVerification records what a due verification waits for, unless a
// judged result is on record (it stands until the next one).
func (p *pass) waitVerification(ctx context.Context, state v1alpha1.IntentVerificationState) (bool, error) {
	v := p.in.Status.Verification
	if v.Ref != nil || v.State == state {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Verification.State = state
		return nil
	})
}

// waitPreview records that a due verification waits for the Preview to be
// live at its heads, for at most previewWaitMax from when the wait began.
// Past it the verification is Unavailable (PreviewNotLive) and stays so,
// reported as an error, until the Preview is live (the gate then verifies)
// or the heads move. A verification in flight or judged stands, as in
// waitVerification.
func (p *pass) waitPreview(ctx context.Context) (bool, error) {
	v := p.in.Status.Verification
	switch {
	case v.Ref != nil, previewNotLive(v):
		return false, nil
	case v.State != v1alpha1.VerificationWaitingForPreview || v.PreviewWaitSince == nil:
		since := metav1.NewTime(p.now)
		return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.Verification.State = v1alpha1.VerificationWaitingForPreview
			cur.Status.Verification.PreviewWaitSince = &since
			return nil
		})
	case p.now.Sub(v.PreviewWaitSince.Time) < previewWaitMax:
		return false, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelWarn, "the preview was not live at the heads in time; verification unavailable",
		slog.String("intent", p.in.Name), slog.Duration("waited", p.now.Sub(v.PreviewWaitSince.Time)))
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.UnavailableReason = v1alpha1.VerificationUnavailable, v1alpha1.VerificationUnavailablePreviewNotLive
		return nil
	})
}

// previewNotLive reports a verification Unavailable because its Preview
// was not live at the heads within previewWaitMax.
func previewNotLive(v *v1alpha1.IntentVerification) bool {
	return v.State == v1alpha1.VerificationUnavailable &&
		v.UnavailableReason == v1alpha1.VerificationUnavailablePreviewNotLive
}

// needsReplan records that criterion id can no longer run as approved: no
// fix round can change an approved criterion, so automatic fixes halt
// (CriterionError) and only a replan helps. A pending request is consumed.
func (p *pass) needsReplan(ctx context.Context, id string) error {
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "an acceptance criterion's target is not previewed; it needs a replan",
		slog.String("intent", p.in.Name), slog.String("criterion", id))
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.Requested = v1alpha1.VerificationNeedsReplan, nil
		halt(nv, v1alpha1.AutoFixCriterionError, metav1.NewTime(p.now))
		setCondition(cur, v1alpha1.ConditionVerifyFailing, metav1.ConditionTrue,
			string(v1alpha1.AutoFixCriterionError),
			"an approved acceptance criterion's target is not a previewed component of this Project, so it "+
				"cannot run as approved; the plan cannot change while its pull requests are open: /maul cancel and "+
				"a new issue start over")
		return nil
	})
}

// createVerification creates the next PreviewVerification against preview
// at hs and records it in state (Verifying for slice 1's criteria,
// Evaluating for the evaluator's outcomes): a retry is its round's next
// attempt, anything else a new round's first. fill completes the spec with
// what it runs and its timeout; an error from it creates nothing. One that
// already exists under the name (a status write lost after its create) is
// adopted when it is this Intent's at hs.
func (p *pass) createVerification(ctx context.Context, kind verifyKind, preview *v1alpha1.Preview, hs headSet,
	state v1alpha1.IntentVerificationState, fill func(*v1alpha1.PreviewVerificationSpec) error) error {
	v := p.in.Status.Verification
	round, attempt := v.Round+1, int32(1)
	if kind == verifyRetry {
		round, attempt = v.Round, v.Attempt+1
	}
	if round > v1alpha1.MaxIntentRound || attempt > v1alpha1.MaxVerificationAttempts {
		return p.update(ctx, func(cur *v1alpha1.Intent) error {
			nv := cur.Status.Verification
			nv.State, nv.UnavailableReason, nv.Requested = v1alpha1.VerificationUnavailable, "", nil
			return nil
		})
	}
	run, err := newRunToken()
	if err != nil {
		return err
	}
	trigger := verificationByAutomatic
	if kind == verifyRequest {
		trigger = verificationByRequest
	}
	pv := &v1alpha1.PreviewVerification{
		ObjectMeta: metav1.ObjectMeta{
			Name: v1alpha1.PreviewVerificationName(p.in.Name, round, attempt), Namespace: p.in.Namespace,
			Labels:          map[string]string{v1alpha1.LabelIntent: p.in.Name},
			Annotations:     map[string]string{annotationVerificationTrigger: trigger},
			OwnerReferences: []metav1.OwnerReference{intentOwner(p.in)},
		},
		Spec: v1alpha1.PreviewVerificationSpec{
			IntentRef:         v1alpha1.ObjectReference{Name: p.in.Name, UID: p.in.UID},
			PreviewRef:        v1alpha1.ObjectReference{Name: preview.Name, UID: preview.UID},
			PreviewGeneration: preview.Generation, HeadSetDigest: hs.digest, Heads: slices.Clone(hs.heads),
			Round: round, Attempt: attempt, Run: run,
		},
	}
	if err := fill(&pv.Spec); err != nil {
		return err
	}
	if err := p.r.Create(ctx, pv); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return fmt.Errorf("create verification %s: %w", pv.Name, err)
		}
		if pv, err = p.adoptVerification(ctx, pv.Name, hs); err != nil {
			if errors.Is(err, errStaleVerification) {
				return nil // deleted; a later pass creates it afresh
			}
			return err
		}
	}
	p.pruneVerifications(ctx)
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.UnavailableReason = state, ""
		nv.Round, nv.Attempt = pv.Spec.Round, pv.Spec.Attempt
		nv.Ref = &v1alpha1.ObjectReference{Name: pv.Name, UID: pv.UID}
		clearJudgement(nv)
		countManual(nv, cur.Status.Acceptance)
		if kind != verifyRetry {
			nv.InfraRetries = 0
		}
		if kind == verifyAutomatic {
			at := metav1.NewTime(p.now)
			nv.Automatic++
			nv.LastAutomaticAt = &at
		}
		if state == v1alpha1.VerificationEvaluating {
			p.startHandoff(nv)
		}
		if kind != verifyAutomatic {
			nv.Requested = nil // a retry serves a waiting request too
		}
		return nil
	})
}

// adoptVerification reads the PreviewVerification name that already exists
// and returns it when it is this Intent's request at hs. One of this
// Intent's at other heads is deleted (the next pass creates afresh); one
// that is not this Intent's is an error, never adopted.
func (p *pass) adoptVerification(ctx context.Context, name string, hs headSet) (*v1alpha1.PreviewVerification,
	error) {
	pv, err := p.readVerification(ctx, v1alpha1.ObjectReference{Name: name})
	if err != nil {
		return nil, fmt.Errorf("read verification %s: %w", name, err)
	}
	if pv == nil {
		return nil, fmt.Errorf("verification %s exists and is not this intent's", name)
	}
	if pv.Spec.HeadSetDigest != hs.digest || !pv.DeletionTimestamp.IsZero() {
		p.deleteVerification(ctx, pv)
		return nil, fmt.Errorf("verification %s: %w", name, errStaleVerification)
	}
	return pv, nil
}

// errStaleVerification: a PreviewVerification under the name the next one
// takes is this Intent's, from other heads; it is deleted and the
// verification created on a later pass.
var errStaleVerification = errors.New("an earlier verification holds the name")

// pruneVerifications deletes all but the newest verificationRetention of
// the Intent's PreviewVerifications, by round and attempt. Best effort.
func (p *pass) pruneVerifications(ctx context.Context) {
	pvs, err := p.ownVerifications(ctx)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "list the intent's verifications to prune them",
			slog.String("intent", p.in.Name), slog.Any("error", err))
		return
	}
	slices.SortFunc(pvs, func(a, b v1alpha1.PreviewVerification) int {
		if a.Spec.Round != b.Spec.Round {
			return int(b.Spec.Round - a.Spec.Round)
		}
		return int(b.Spec.Attempt - a.Spec.Attempt)
	})
	for i := verificationRetention; i < len(pvs); i++ {
		p.deleteVerification(ctx, &pvs[i])
	}
}

// dropVerifications deletes every PreviewVerification of an Intent whose
// review ended (H12): nothing more is verified or reported. Best effort;
// the Intent's deletion takes any left.
func (p *pass) dropVerifications(ctx context.Context) {
	if !p.set.Verification {
		return
	}
	pvs, err := p.ownVerifications(ctx)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "list the ended intent's verifications",
			slog.String("intent", p.in.Name), slog.Any("error", err))
		return
	}
	for i := range pvs {
		p.deleteVerification(ctx, &pvs[i])
	}
}
