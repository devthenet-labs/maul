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

// The verification loop, with verification (the agent evaluator) on. In
// review, and in a round of it, every pass:
//
//  1. records the approved plan's acceptance outcomes once per approved
//     plan revision (syncAcceptance), from the plan bytes re-read under the
//     approval's digest, normalised, digested as approved; and re-arms
//     automatic fixes and the per-revision bounds. A plan stating slice-1
//     criteria, which nothing runs since S4, is not verified: under the
//     evaluator the intent gets one notice asking for a replan
//     (stopForReplan, DESIGN 7.6), the one GitHub write here;
//  2. branches on the pass's verify mode (verify_mode.go): Off for a
//     record nothing verifies (switchedOff), else the evaluator's own
//     rounds (syncEvaluation, evaluate_round.go): NotDeclared for a plan
//     with nothing to evaluate, consuming any request (it still records
//     the head set an acceptance binds to), the human-round halt, the head
//     set (syncHeadSet: when the combined change moved, the in-flight
//     PreviewVerification is deleted and the state starts over at the new
//     heads, and a head maul did not push halts automatic fixes), then the
//     round in flight, retried on maul's own failure at most
//     MaxVerificationAttempts before it is Unavailable.
//
// In review with no round started (startRound's last step) the gate
// (startVerification) creates the next PreviewVerification when one is due
// (verifyDue): the heads, every named check green, within the bounds.
// Nothing else here writes to GitHub or starts a round: the commit
// statuses and comments, and the verify-fix rounds, read the state this
// writes.

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
	if p.in.Status.Verification == nil {
		return false, nil
	}
	switch p.mode() {
	case modeOff, modeMismatch:
		return p.switchedOff(ctx)
	case modeOutcomes:
		// The agent evaluator's outcomes: its own rounds (evaluate_round.go).
		return p.syncEvaluation(ctx)
	case modeNone:
		// A record of another plan revision is never run against the
		// approved one's code.
	}
	return false, nil
}

// syncAcceptance records the approved plan revision's acceptance outcomes,
// once: an approved plan's own, re-read under the approval's digest. A new
// revision starts verification afresh (H11): automatic fixes armed, the
// per-revision bounds and the head set cleared, its in-flight request
// deleted; the round numbering, the reset generation and an approver's
// pending request carry over. A plan that cannot be read or no longer
// matches its digest records nothing (logged) and stops verification
// (stopUnreadable): the record on record, if any, is another revision's.
func (p *pass) syncAcceptance(ctx context.Context) (bool, error) {
	ap := p.in.Status.Approval
	if p.mode() != modeNone {
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

// verifyStopped is why verification of the approved plan revision is
// stopped with nothing on record ("" when it is not): its criteria cannot
// be read (stopUnreadable, reasonCriteriaUnreadable), or they are slice-1
// criteria the agent evaluator does not run (stopForReplan,
// reasonCriteriaReplan). The verification and the condition say which.
func (p *pass) verifyStopped() string {
	v := p.in.Status.Verification
	if p.mode() != modeNone || v == nil || v.State != v1alpha1.VerificationUnavailable {
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
// holds them to the approval's digest, and returns its acceptance outcomes
// as the Intent records them: normalised, with the digest of the list as
// the plan declared it (OutcomesDigest; Digest, a required field, holds
// the empty criteria list's). For a plan stating slice-1 criteria it
// returns report.ErrSliceOneCriteria under the agent evaluator, else a
// record of their digest alone, which nothing verifies.
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
	if err := parsed.CheckEvaluator(); err != nil {
		if p.set.evaluates(p.proj) {
			return nil, fmt.Errorf("plan r%d: %w", ap.PlanRevision, err)
		}
		// Not verified: a criteria-era record by its digest alone, as the
		// API server's pruning leaves one (recordCriteriaEra): never
		// run, judged or accepted, and read again for a replan should the
		// evaluator come to verify the Project.
		return &v1alpha1.IntentAcceptance{PlanRevision: ap.PlanRevision,
			Digest: acceptance.Digest(parsed.Acceptance)}, nil
	}
	outcomes, err := crdconv.OutcomesToCRD(acceptance.NormalizeOutcomes(parsed.Outcomes))
	if err != nil {
		return nil, err
	}
	return &v1alpha1.IntentAcceptance{PlanRevision: ap.PlanRevision, Digest: emptyCriteriaDigest,
		Outcomes: outcomes, OutcomesDigest: acceptance.OutcomesDigest(parsed.Outcomes)}, nil
}

// stopForReplan stops verification of an approved plan revision that
// states slice-1 criteria while the agent evaluator verifies the Project
// (DESIGN 7.6): it posts, once, the notice asking for a replan (keyed by the
// revision, so a retry or a restart finds the one it posted), then deletes
// the in-flight request, consumes any waiting one, drops the criteria on
// record (nothing runs them; off again, the plan is recorded by their
// digest alone, which nothing verifies), halts automatic fixes and reports
// Unavailable with the Intent's
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
	why := templates.NotRunNothingToCheck
	if state != v1alpha1.VerificationNotDeclared {
		why = p.offNotRunWhy()
	}
	if err := p.answerNotRun(ctx, why, 0); err != nil {
		return false, err
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
		// The reason a round recorded for not running is about the heads
		// it was recorded at, as a judgement is.
		nv.State, nv.Cause = state, ""
		nv.HeadSetDigest, nv.Heads = hs.digest, slices.Clone(hs.heads)
		nv.Ref, nv.Attempt, nv.InfraRetries, nv.RetryOwed = nil, 0, 0, false
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
	v.QuarantinedIDs = nil
}

// countManual counts the manual outcomes accepted at v's head set and those
// still open.
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
}

// unjudged settles an attempt that ended without a judgement: maul's own
// failure, a request the Preview no longer matched, or one that vanished.
// It is never charged to the intent: the round's next attempt is due
// (verifyRetry), or after the last the verification is Unavailable until
// the heads move or an approver asks again.
func (p *pass) unjudged(ctx context.Context) error { return p.unjudgedFor(ctx, nil) }

// unjudgedFor is unjudged for pv, the round that settled unjudged (nil when
// it is gone or was dropped). After the last attempt the verification
// records the cause pv's reason names (v1alpha1.CauseOf), which every
// surface then says: an agents round whose every attempt ended with the
// tool pod unable to build the head CI built green is Unconfirmed for that
// cause (SPEC-S2 S2-20), never Unavailable and never a fail.
func (p *pass) unjudgedFor(ctx context.Context, pv *v1alpha1.PreviewVerification) error {
	cause := v1alpha1.SettleNone
	if pv != nil {
		cause = v1alpha1.CauseOf(pv.Status.Reason)
	}
	buildFailed := pv != nil && pv.Spec.IsAgents() && cause == v1alpha1.SettleToolsBuildFailed
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		clearJudgement(nv)
		countManual(nv, cur.Status.Acceptance)
		if nv.Attempt >= v1alpha1.MaxVerificationAttempts {
			nv.State, nv.Cause, nv.RetryOwed = v1alpha1.VerificationUnavailable, cause, false
			if buildFailed {
				nv.State = v1alpha1.VerificationUnconfirmed
			}
			return nil
		}
		nv.Ref, nv.RetryOwed = nil, true
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

// startVerification is the gate (startRound's last step, nothing else
// started): under the agent evaluator, startEvaluation creates the next
// PreviewVerification when one is due; nothing else is verified.
func (p *pass) startVerification(ctx context.Context, prs []v1alpha1.IntentPullRequest) (bool, error) {
	if p.in.Status.Verification == nil || p.mode() != modeOutcomes {
		return false, nil
	}
	return p.startEvaluation(ctx, prs)
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

// previewNotLive reports a verification Unavailable because its Preview
// was not live at the heads within previewWaitMax.
func previewNotLive(v *v1alpha1.IntentVerification) bool {
	return v.State == v1alpha1.VerificationUnavailable &&
		v.Cause == v1alpha1.SettlePreviewNotLive
}

// createVerification creates the next PreviewVerification at hs and
// records it in state (Evaluating, for the evaluator's outcomes): a retry is its round's next attempt, anything else
// a new round's first. fill completes the spec with where it runs (the
// Preview it pins, or an agents workspace), what it runs and its timeout;
// an error from it creates nothing. One that already exists under the name
// (a status write lost after its create) is adopted when it is this
// Intent's at hs.
func (p *pass) createVerification(ctx context.Context, kind verifyKind, hs headSet,
	state v1alpha1.IntentVerificationState, fill func(*v1alpha1.PreviewVerificationSpec) error) error {
	v := p.in.Status.Verification
	round, attempt := v.Round+1, int32(1)
	if kind == verifyRetry {
		round, attempt = v.Round, v.Attempt+1
	}
	// An automatic round at the heads of a round on record is a follow-up
	// of it: a dispute's, or an escalation (recordRound).
	followUp, prevRound := kind == verifyAutomatic && v.Ref != nil && v.HeadSetDigest == hs.digest, v.Round
	if round > v1alpha1.MaxIntentRound || attempt > v1alpha1.MaxVerificationAttempts {
		return p.update(ctx, func(cur *v1alpha1.Intent) error {
			nv := cur.Status.Verification
			nv.State, nv.Cause, nv.Requested, nv.RetryOwed = v1alpha1.VerificationUnavailable, "", nil, false
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
			IntentRef:     v1alpha1.ObjectReference{Name: p.in.Name, UID: p.in.UID},
			HeadSetDigest: hs.digest, Heads: slices.Clone(hs.heads),
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
		nv.State, nv.Cause = state, ""
		nv.Round, nv.Attempt = pv.Spec.Round, pv.Spec.Attempt
		nv.Ref, nv.RetryOwed = &v1alpha1.ObjectReference{Name: pv.Name, UID: pv.UID}, false
		recordRound(nv, pv.Spec.Rung, followUp, prevRound, hs.digest)
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
