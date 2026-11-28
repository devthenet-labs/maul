// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The context a replan in review adds to planContext: what review made of
// the replaced plan. Like the rest of the context file, each section is the
// controller's own rendering of data, visibly escaped and fenced, and bounded
// on its own: the pull requests (at most 8 lines of maul's own records, 2
// KiB), the approvers' review threads on them (maxReplanThreadsBytes, each
// pull request's at most maxEarlierFeedbackBytes, read by the same rules as
// a round's earlier feedback: approvers' items only, never maul's, never one
// edited) and the verification's results (maxReplanOutcomesBytes). With the
// 135 KiB planContext's other sections can take, about 170 KiB: far under the
// 1 MiB a ConfigMap and the Job's Secret may hold. The replan's own note is
// not here: it is an approver's comment on the issue, so it is in the
// request (feedbackSince).
const (
	maxReplanPullRequestsBytes = 2 << 10
	maxReplanThreadsBytes      = 24 << 10
	maxReplanOutcomesBytes     = 8 << 10
)

// reviewContext is the context of a replan in review being settled ("" for
// any other replan or revival): the replaced plan's pull requests, their
// approver review threads, and what verification found.
func (p *pass) reviewContext(ctx context.Context) string {
	rp := p.in.Status.Replan
	if rp == nil || len(p.in.Status.PullRequests) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## The pull requests of the previous plan\n\n")
	b.WriteString("The pull requests maul opened for plan r" + fmt.Sprint(rp.PlanRevision) + ", as they were " +
		"when it was replaced. maul closed the open ones; a merged one stays merged. The new plan's pull " +
		"requests are new ones, on new branches.\n\n")
	b.WriteString(fencedBounded(visibleFeedback(p.replanPullRequests()), maxReplanPullRequestsBytes) + "\n")
	if threads := p.replanThreads(ctx, rp); threads != "" {
		b.WriteString("\n## Approver review on them\n\n")
		b.WriteString("The approvers' reviews and comments on those pull requests, oldest first, as data. " +
			"They were written about the previous plan's code, not the request.\n\n")
		b.WriteString(threads + "\n")
	}
	if outcomes := p.replanOutcomes(); outcomes != "" {
		b.WriteString("\n## What verification found\n\n")
		b.WriteString("The latest verdict maul's evaluator reached on each acceptance outcome of the previous " +
			"plan, as data.\n\n")
		b.WriteString(outcomes + "\n")
	}
	return b.String()
}

// replanPullRequests lists the replaced plan's pull requests: link, state
// and head.
func (p *pass) replanPullRequests() string {
	lines := make([]string, 0, len(p.in.Status.PullRequests))
	for _, pr := range p.in.Status.PullRequests {
		lines = append(lines, fmt.Sprintf("- %s#%d (%s) %s at %s", repoSlug(pr.Repository), pr.Number, pr.State,
			pr.URL, pr.HeadSHA))
	}
	return strings.Join(lines, "\n")
}

// replanThreads is each pull request's approver thread since its build,
// within maxReplanThreadsBytes: a thread that does not fit is named as left
// out, and one GitHub would not give is earlierUnavailable. Best effort: the
// context never holds up the replan.
func (p *pass) replanThreads(ctx context.Context, rp *v1alpha1.IntentReplan) string {
	var parts []string
	total := 0
	for _, pr := range p.in.Status.PullRequests {
		head := fmt.Sprintf("### %s#%d", repoSlug(pr.Repository), pr.Number)
		thread, err := p.prThread(ctx, pr, rp.PlanRevision)
		if err != nil {
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "could not read a pull request's review for a replan",
				slog.String("intent", p.in.Name), slog.String("repository", pr.Repository),
				slog.Int64("number", pr.Number), slog.Any("error", err))
			thread = earlierUnavailable
		}
		if thread == "" {
			continue
		}
		part := head + "\n\n" + thread
		if total+len(part)+2 > maxReplanThreadsBytes {
			part = head + "\n\nLeft out: the review threads are bounded."
		}
		total += len(part) + 2
		parts = append(parts, part)
	}
	return strings.Join(parts, "\n\n")
}

// prThread is pr's approver thread since the first build of plan revision
// rev in its repository, all of it as earlier feedback (renderEarlier).
func (p *pass) prThread(ctx context.Context, pr v1alpha1.IntentPullRequest, rev int32) (string, error) {
	t := thread{proj: p.proj, lower: p.buildStart(pr.Repository, rev), upper: p.now}
	bot, err := p.r.GitHub.BotLogin(ctx, pr.Repository)
	if err != nil {
		return "", err
	}
	t.bot = bot
	reviews, err := p.r.GitHub.ListPullRequestReviews(ctx, pr.Repository, pr.Number)
	if err != nil {
		return "", err
	}
	items := t.reviews(reviews)
	inline, err := p.r.GitHub.ListPullRequestReviewComments(ctx, pr.Repository, pr.Number)
	if err != nil {
		return "", err
	}
	items = append(items, t.inline(inline)...)
	comments, err := p.r.GitHub.ListPullRequestComments(ctx, pr.Repository, pr.Number, t.lower)
	if err != nil {
		return "", err
	}
	items = append(items, t.comments(comments)...)
	v := verifier{p: p, repo: pr.Repository, approved: map[string]bool{}}
	return renderEarlier(ctx, dedupeFeedback(items), v.verify), nil
}

// buildStart is when the first build of plan revision rev in repoURL was
// created: no item of its pull request's thread is older.
func (p *pass) buildStart(repoURL string, rev int32) (at time.Time) {
	if builds := p.round(v1alpha1.IntentStageBuild, rev, repoURL); len(builds) > 0 {
		return builds[0].CreationTimestamp.Time
	}
	return at
}

// replanOutcomes is the latest verdict on each acceptance outcome of the
// replaced plan, fenced, or "" when verification judged none of them.
func (p *pass) replanOutcomes() string {
	acc, v := p.in.Status.Acceptance, p.in.Status.Verification
	if acc == nil || v == nil || len(v.Outcomes) == 0 || acc.PlanRevision != p.in.Status.Replan.PlanRevision {
		return ""
	}
	stated := map[string]string{}
	for _, o := range acc.Outcomes {
		stated[o.ID] = o.Outcome
	}
	var lines []string
	for _, r := range v.Outcomes {
		line := fmt.Sprintf("- %s: %s", r.ID, r.Result)
		if s := stated[r.ID]; s != "" {
			line += " (" + s + ")"
		}
		if r.Reason != "" {
			line += ": " + r.Reason
		}
		lines = append(lines, line)
	}
	return fencedBounded(visibleFeedback(strings.Join(lines, "\n")), maxReplanOutcomesBytes)
}
