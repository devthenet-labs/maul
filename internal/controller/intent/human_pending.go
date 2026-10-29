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
	"github.com/devthenet-labs/maul/internal/ghclient"
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

// releaseForHuman is startRound's part of "the human wins" under a gated
// hand-off (SPEC S1-20): while the preview link is held and no round
// started, a human's pending feedback on prs (humanPending, its quiet
// window included) releases the link at once and halts automatic fixes, in
// one write; the human's round follows once its window ends. It reports
// whether it wrote, and whether the human's feedback holds up maul's own
// work (the verification's fix and gate) this pass.
func (p *pass) releaseForHuman(ctx context.Context, prs []v1alpha1.IntentPullRequest) (wrote, pending bool,
	err error) {
	reason, err := p.humanPending(ctx, prs)
	if err != nil || reason == "" {
		return false, false, err
	}
	if !p.linkHeld() {
		return false, true, nil
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "a human's feedback releases the preview link",
		slog.String("intent", p.in.Name), slog.String("reason", string(reason)))
	return true, true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		halt(cur.Status.Verification, reason, metav1.NewTime(p.now))
		return nil
	})
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

// prListing is one listing of a pull request's comments: those updated
// since since, as GitHub filters them.
type prListing struct {
	since    time.Time
	comments []*ghclient.Comment
}

// prComments are pull request pr's comments updated since since. A pass
// lists each pull request's comments once and every step reading them (the
// acceptance commands, commandRound, humanPending) shares that listing: a
// read from no earlier time is answered from it, filtered as GitHub would
// filter a listing of its own, so it costs no request. The steps run the
// acceptance commands' read, from the build, first.
func (p *pass) prComments(ctx context.Context, pr *v1alpha1.IntentPullRequest, since time.Time) (
	[]*ghclient.Comment, error) {
	key := prKey(*pr)
	if l, ok := p.prListed[key]; ok && !since.Before(l.since) {
		out := make([]*ghclient.Comment, 0, len(l.comments))
		for _, c := range l.comments {
			if !c.UpdatedAt.Before(since) {
				out = append(out, c)
			}
		}
		return out, nil
	}
	comments, err := p.r.GitHub.ListPullRequestComments(ctx, pr.Repository, pr.Number, since)
	if err != nil {
		return nil, err
	}
	if p.prListed == nil {
		p.prListed = map[string]prListing{}
	}
	p.prListed[key] = prListing{since: since, comments: comments}
	return slices.Clone(comments), nil
}

// notePRComment adds c, a comment maul just posted on pr, to the pass's
// listing of pr, so a later step reading it sees maul's reply.
func (p *pass) notePRComment(pr *v1alpha1.IntentPullRequest, c *ghclient.Comment) {
	key := prKey(*pr)
	if l, ok := p.prListed[key]; ok && c != nil {
		l.comments = append(slices.Clone(l.comments), c)
		p.prListed[key] = l
	}
}

// humanReason is the halt reason a revise round of trigger t gives
// automatic fixes when it starts: a human's names what started it; maul's
// own rounds (checks, verify) give none.
// A round with no trigger predates triggers: an approver's review, as
// roundKindOf reads it.
func humanReason(t v1alpha1.IntentRunTrigger) v1alpha1.AutoFixReason {
	switch {
	case roundKindOf(t) != roundHuman:
		return ""
	case acksCommand(t):
		return v1alpha1.AutoFixHumanCommand
	}
	return v1alpha1.AutoFixHumanReview
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
