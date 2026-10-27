// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// "The human wins" (acceptance verification §4.5): an approver's feedback
// always goes before maul's own acceptance fixes. humanPending is the one
// predicate both places that order them ask: startRound, which starts no
// verify fix and no verification while it holds (a review inside its quiet
// window included), and revising, which cancels a verify fix not yet
// launched, or halts automatic fixes while one runs, once it holds. It
// counts exactly what reviewRound and commandRound act on, through the same
// helpers (reviewRequests, prCommandCandidates and eligiblePRCommand), so
// the two can never disagree about what is a human's.

// humanPending is why a human's round is due on any of the Intent's open
// pull requests in prs, whether or not it could start yet: AutoFixHumanReview
// for an approver's unedited, unconsumed CHANGES_REQUESTED review (inside its
// quiet window too), AutoFixHumanCommand for an approver's unedited,
// unconsumed /maul revise or retry. "" when none is. An approval, a
// comment-only review, a non-approver's feedback and an edited one never
// count, nor does a pull request whose repository left the Project: its
// feedback never holds up the others' rounds.
func (p *pass) humanPending(ctx context.Context, prs []v1alpha1.IntentPullRequest) (v1alpha1.AutoFixReason, error) {
	for i := range prs {
		pr := &prs[i]
		if pr.State != prOpen {
			continue
		}
		if _, ok := p.approvedRepository(pr.Repository); !ok {
			continue
		}
		if due, err := p.reviewDue(ctx, pr); err != nil || due {
			if err != nil {
				return "", err
			}
			return v1alpha1.AutoFixHumanReview, nil
		}
		if due, err := p.commandDue(ctx, pr); err != nil || due {
			if err != nil {
				return "", err
			}
			return v1alpha1.AutoFixHumanCommand, nil
		}
	}
	return "", nil
}

// reviewDue reports a review round due on pr, its quiet window ignored:
// a review reviewRound would act on once the window passed.
func (p *pass) reviewDue(ctx context.Context, pr *v1alpha1.IntentPullRequest) (bool, error) {
	_, requests, err := p.reviewRequests(ctx, pr)
	return len(requests) > 0, err
}

// commandDue reports a command round due on pr: a command commandRound
// would act on.
func (p *pass) commandDue(ctx context.Context, pr *v1alpha1.IntentPullRequest) (bool, error) {
	cutoff := p.reviewCutoff(pr.Repository)
	comments, err := p.prCommandCandidates(ctx, pr, cutoff)
	if err != nil {
		return false, err
	}
	for _, c := range comments {
		eligible, err := p.eligiblePRCommand(ctx, pr, c, cutoff)
		if err != nil || eligible {
			return eligible, err
		}
	}
	return false, nil
}

// humanReason is the halt reason a revise round of trigger t gives
// automatic fixes when it starts: a human's names what started it; maul's
// own rounds (checks, verify) give none.
func humanReason(t v1alpha1.IntentRunTrigger) v1alpha1.AutoFixReason {
	switch t {
	case v1alpha1.IntentRunTriggerReview:
		return v1alpha1.AutoFixHumanReview
	case v1alpha1.IntentRunTriggerCommand:
		return v1alpha1.AutoFixHumanCommand
	case v1alpha1.IntentRunTriggerChecks, v1alpha1.IntentRunTriggerVerify:
		return ""
	default:
		// A round with no trigger predates triggers: an approver's, as
		// roundKindOf reads it.
		return v1alpha1.AutoFixHumanReview
	}
}

// haltForHumanRound halts cur's automatic fixes as run, a round of the
// approved plan revision that a human started, is entered: in the same
// write (§4.5 step 2). A round not later than the one an approver's
// /maul verify fix re-armed them at does not halt them again (H10).
func haltForHumanRound(cur *v1alpha1.Intent, run *v1alpha1.IntentRun, now metav1.Time) {
	v, ap := cur.Status.Verification, cur.Status.Approval
	reason := humanReason(run.Spec.Trigger)
	if v == nil || ap == nil || reason == "" || run.Spec.Inputs.PlanRevision != ap.PlanRevision {
		return
	}
	if r := v.AutoFix.Rearm; r != nil && run.Spec.Round <= r.Round {
		return
	}
	halt(v, reason, now)
}
