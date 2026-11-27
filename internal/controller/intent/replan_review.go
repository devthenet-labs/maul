// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// A replan in review (--intent-replan-in-review, Settings.ReplanInReview):
// an approver's /maul replan, or the trigger label re-applied, while the
// intent is InReview or Revising, or Blocked from either, replaces the
// approved plan in place. It is refused, answered once and consumed, when a
// pull request of the intent has merged (read live: what merged cannot be
// planned again), when the intent is at its cost ceiling (a new plan would be
// stopped by it before it was written), or when no input revision is left.
//
// Accepted, it is recorded first (status.replan, with the trigger consumed
// and status.planBranchesFrom set on the first one), and every pass that
// finds it recorded resumes it before anything else, the poll included, so
// no step of review ever reads maul's own closes as a human's. Its steps,
// each idempotent and each ending the pass with a status write:
//
//  1. wait for the runs of the old plan to settle: the run reconciler stops
//     every one (planSuperseded), and no round pushes once it is recorded;
//  2. drop the verifications, and post a final error maul/acceptance on
//     every pull request head maul posted one on (withdrawAcceptance);
//  3. read each open pull request live, post one comment on it and close it
//     (closeForReplan), recording each one's state;
//  4. post the one reply on the issue, then plan again (startPlanning, the
//     replan the trigger: the new plan's runs are attributed to the
//     replanner), with the plan, its pull requests, the approvers' review
//     threads and the verification results as context (reviewContext),
//     every per-plan count started again and the review's leftovers
//     cleared.
//
// The new plan needs its own approval (writeBack removes any approve label
// before posting it, and an approval binds the plan and input digests) and is
// built on a branch of its own (v1alpha1.IntentBranch), since pushes are
// create-only. The intent-wide spend keeps counting.

// replanRunWait is how long the replan waits before it looks again at a
// run of the old plan that has not settled.
const replanRunWait = 5 * time.Second

// supersededByReplan is the final maul/acceptance a replan in review leaves
// on a pull request head maul posted one on.
var supersededByReplan = acceptanceState{statusError, "superseded by a replan"}

// replanInReview reports a replan the phase takes in review: the flag on,
// the intent InReview or Revising (or Blocked from either), and a plan
// approved.
func (p *pass) replanInReview() bool {
	return p.set.ReplanInReview && p.followsPullRequests() && p.in.Status.Approval != nil &&
		p.in.Status.Input != nil
}

// settleReplanInReview answers an approver's replan in review: refused with
// a notice, or recorded for continueReplan. Either way it is consumed.
func (p *pass) settleReplanInReview(ctx context.Context, a humanAction, rec v1alpha1.IntentAction) error {
	n, err := p.replanRefusal(ctx)
	if err != nil {
		return err
	}
	if n != nil {
		n.Namespace, n.Intent, n.Key = p.in.Namespace, p.in.Name, a.key()
		if a.source == v1alpha1.IntentActionLabel {
			n.Label = a.label
		} else {
			n.Verb = a.verb
		}
		body, err := templates.RenderReplanRefusedNotice(*n)
		if err := p.notice(ctx, a.key(), a.at, body, err); err != nil {
			return err
		}
		return p.update(ctx, func(cur *v1alpha1.Intent) error {
			cur.Status.LastTrigger = &rec
			return nil
		})
	}
	replaced, next := p.in.Status.Approval.PlanRevision, p.in.Status.Input.Revision+1
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "replan in review accepted", slog.String("intent", p.in.Name),
		slog.String("by", a.actor.Login), slog.Int("replaces_plan", int(replaced)))
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Replan = &v1alpha1.IntentReplan{Trigger: rec, PlanRevision: replaced}
		cur.Status.LastTrigger = &rec
		if cur.Status.PlanBranchesFrom == 0 {
			cur.Status.PlanBranchesFrom = next
		}
		cur.Status.ActiveRun = nil
		return nil
	})
}

// replanRefusal is the notice refusing a replan in review, nil when it may
// be taken: no input revision left, the spend at the ceiling, or a pull
// request merged (its record, or read live now).
func (p *pass) replanRefusal(ctx context.Context) (*templates.ReplanRefusedNotice, error) {
	if p.in.Status.Input.Revision >= v1alpha1.MaxIntentRound {
		return &templates.ReplanRefusedNotice{Reason: templates.ReplanRefusedLimit}, nil
	}
	if p.atCeiling() {
		return &templates.ReplanRefusedNotice{Reason: templates.ReplanRefusedCost,
			SpentMicroUSD: p.sharedSpend(), CeilingMicroUSD: maxCostMicroUSD(p.proj)}, nil
	}
	var merged []templates.IntentPullRequest
	for _, pr := range p.in.Status.PullRequests {
		isMerged := pr.State == prMerged
		if !isMerged {
			live, err := p.r.GitHub.GetPullRequest(ctx, pr.Repository, pr.Number)
			switch {
			case err != nil && p.leftProject(pr.Repository):
				// Out of reach: its record decides.
			case err != nil:
				return nil, fmt.Errorf("read pull request %s#%d: %w", repoSlug(pr.Repository), pr.Number, err)
			default:
				isMerged = live.Merged
			}
		}
		if isMerged {
			merged = append(merged, templates.IntentPullRequest{Repository: repoSlug(pr.Repository),
				Number: pr.Number, URL: pr.URL, State: prMerged})
		}
	}
	if len(merged) > 0 {
		return &templates.ReplanRefusedNotice{Reason: templates.ReplanRefusedMerged, Merged: merged}, nil
	}
	return nil, nil
}

// continueReplan takes the recorded replan in review its next step.
func (p *pass) continueReplan(ctx context.Context) (ctrl.Result, error) {
	if p.oldPlanRunning() {
		return ctrl.Result{RequeueAfter: replanRunWait}, nil
	}
	p.dropVerifications(ctx)
	if changed, err := p.withdrawAcceptance(ctx); changed || err != nil {
		return ctrl.Result{}, err
	}
	if changed, err := p.closeForReplan(ctx); changed || err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, p.planAfterReplan(ctx)
}

// oldPlanRunning reports a run of the replaced plan (any but a plan run)
// that has not settled: the run reconciler is stopping it.
func (p *pass) oldPlanRunning() bool {
	return slices.ContainsFunc(p.runs, func(run *v1alpha1.IntentRun) bool {
		return run.Spec.Stage != v1alpha1.IntentStagePlan && !runSettled(run)
	})
}

// withdrawAcceptance posts supersededByReplan on every open pull request's
// head maul posted a maul/acceptance on, once (by its digest), and records
// it. changed reports the status write.
func (p *pass) withdrawAcceptance(ctx context.Context) (bool, error) {
	if p.statusesUnavailable() {
		return false, nil
	}
	prs := slices.Clone(p.in.Status.PullRequests)
	changed := false
	for i := range prs {
		pr := &prs[i]
		if pr.State != prOpen || pr.AcceptanceStatusID == 0 || pr.AcceptanceHead == "" ||
			p.leftProject(pr.Repository) {
			continue
		}
		d := acceptanceDigest(pr.AcceptanceHead, "", supersededByReplan)
		if d == pr.AcceptanceDigest {
			continue
		}
		id, err := p.r.GitHub.CreateCommitStatus(ctx, pr.Repository, pr.AcceptanceHead, supersededByReplan.state,
			acceptanceContext, supersededByReplan.description, p.verifyTargetURL())
		if err != nil {
			return false, fmt.Errorf("withdraw maul/acceptance on %s#%d: %w", repoSlug(pr.Repository), pr.Number, err)
		}
		pr.AcceptanceStatusID, pr.AcceptanceDigest = id, d
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.PullRequests = prs
		return nil
	})
}

// closeForReplan reads every pull request still recorded open, live: an
// open one gets the replan's one comment and is closed; a merged one (a
// human merged it after the replan was recorded) stays merged. Each one's
// state is recorded. A pull request in a repository that left the Project
// is left as it is. changed reports the status write.
func (p *pass) closeForReplan(ctx context.Context) (bool, error) {
	rp := p.in.Status.Replan
	prs := slices.Clone(p.in.Status.PullRequests)
	changed := false
	for i := range prs {
		pr := &prs[i]
		if pr.State != prOpen || p.leftProject(pr.Repository) {
			continue
		}
		live, err := p.r.GitHub.GetPullRequest(ctx, pr.Repository, pr.Number)
		switch {
		case ghclient.IsNotFound(err):
			pr.State = prClosed
		case err != nil:
			return false, fmt.Errorf("read pull request %s#%d: %w", repoSlug(pr.Repository), pr.Number, err)
		case live.Merged:
			at := metav1.NewTime(live.MergedAt)
			pr.State, pr.MergedAt = prMerged, &at
		case live.State == prOpen:
			if err := p.closeOne(ctx, *pr, rp); err != nil {
				return false, err
			}
			pr.State = prClosed
		default:
			pr.State = prClosed
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.PullRequests = prs
		return nil
	})
}

// closeOne posts the replan's comment on pr, once, and closes it.
func (p *pass) closeOne(ctx context.Context, pr v1alpha1.IntentPullRequest, rp *v1alpha1.IntentReplan) error {
	key := templates.ReplanClosedKey(actionKey(rp.Trigger))
	c := templates.ReplanClosedComment{Namespace: p.in.Namespace, Intent: p.in.Name, Key: key,
		Actor: rp.Trigger.Login, Branch: p.branch()}
	if rp.Trigger.Source == v1alpha1.IntentActionLabel {
		c.Label = p.trigger()
	} else {
		c.Verb = replanVerb
	}
	body, err := templates.RenderReplanClosedComment(c)
	if err != nil {
		return err
	}
	if err := p.postOnceOnPullRequest(ctx, pr, key, body); err != nil {
		return fmt.Errorf("comment on %s#%d before closing it: %w", repoSlug(pr.Repository), pr.Number, err)
	}
	if err := p.r.GitHub.ClosePullRequest(ctx, pr.Repository, pr.Number); err != nil {
		return fmt.Errorf("close %s#%d: %w", repoSlug(pr.Repository), pr.Number, err)
	}
	return nil
}

// planAfterReplan posts the replan's reply on the issue, once, and plans
// again: the replan is the new input revision's trigger, and the Planning
// write clears the replan and what review left on the intent.
func (p *pass) planAfterReplan(ctx context.Context) error {
	rp := p.in.Status.Replan
	key := actionKey(rp.Trigger)
	n := templates.ReplanInReviewNotice{Namespace: p.in.Namespace, Intent: p.in.Name, Key: key,
		Actor: rp.Trigger.Login, PlanRevision: rp.PlanRevision}
	if rp.Trigger.Source == v1alpha1.IntentActionLabel {
		n.Label = p.trigger()
	} else {
		n.Verb = replanVerb
	}
	for _, pr := range p.in.Status.PullRequests {
		t := templates.IntentPullRequest{Repository: repoSlug(pr.Repository), Number: pr.Number, URL: pr.URL,
			State: pr.State}
		switch pr.State {
		case prMerged:
			n.Merged = append(n.Merged, t)
		case prClosed:
			n.Closed = append(n.Closed, t)
		}
	}
	body, err := templates.RenderReplanInReviewNotice(n)
	if err := p.notice(ctx, key, rp.Trigger.At.Time, body, err); err != nil {
		return err
	}
	trigger := rp.Trigger
	rounds := p.highestRound()
	if err := p.startPlanning(ctx, nil, &trigger, func(cur *v1alpha1.Intent) {
		clearReview(cur, rounds)
	}); err != nil {
		return err
	}
	// What the reconciler remembers of the old pull requests is not the new
	// plan's: a restart would forget it the same way.
	p.r.forget(p.in.Name)
	return nil
}

// replanVerb is the verb a replan command carries.
const replanVerb = "replan"

// clearReview is what the Planning write of a replan in review clears: the
// replan itself, the counts of the replaced plan (status.rounds continues,
// raised to rounds so no new round takes a name an old run holds, and every
// round notice still owed is dropped: its pull request is closed), the
// default-branch heads its preview ran, the legacy checks observation, and
// the blocks review set (resolved: the new plan is held to none of them).
// status.verification and status.acceptance stay: the new plan's approval
// starts verification afresh (syncAcceptance), with the round numbering
// carried on.
func clearReview(cur *v1alpha1.Intent, rounds int32) {
	cur.Status.Replan = nil
	cur.Status.Rounds = max(cur.Status.Rounds, rounds)
	cur.Status.RoundNoticesThrough = cur.Status.Rounds
	cur.Status.Revisions, cur.Status.CheckFixes, cur.Status.VerifyFixes = 0, 0, 0
	cur.Status.ChecksObservedHeadSHA, cur.Status.ChecksObservedProjectGeneration = "", 0
	cur.Status.PreviewBases = nil
	cur.Status.CredentialHold = nil
	for _, c := range blockingConditions {
		if meta.IsStatusConditionTrue(cur.Status.Conditions, c) {
			setCondition(cur, c, metav1.ConditionFalse, "Replanned", "the plan was replaced by a replan in review")
		}
	}
}

// highestRound is the highest revise round any run of the intent holds.
func (p *pass) highestRound() int32 {
	var top int32
	for _, run := range p.runs {
		if run.Spec.Stage == v1alpha1.IntentStageRevise {
			top = max(top, run.Spec.Round)
		}
	}
	return top
}

// actionKey is the notice key of a recorded action (humanAction.key).
func actionKey(a v1alpha1.IntentAction) string {
	return humanAction{source: a.Source, id: a.EventID}.key()
}
