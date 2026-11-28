// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"errors"
	"fmt"
)

// What maul says about a replan in review (--intent-replan-in-review): the
// reply on the issue once it is under way, the comment on each pull request
// it closes, and its refusals. Each names the actor as code, never a
// mention, and links only maul's own pull requests.

// ReplanInReviewNotice is the reply on the issue to a replan accepted in
// review, posted once its pull requests are closed and before the intent
// plans again.
type ReplanInReviewNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Actor is the approver who replanned.
	Actor string
	// Verb is the command's verb, or Label the trigger label it arrived as.
	Verb  string
	Label string
	// PlanRevision is the plan revision the replan replaces.
	PlanRevision int32
	// Closed are the pull requests maul closed; Merged are any a human
	// merged before maul could close them, which stay merged.
	Closed []IntentPullRequest
	Merged []IntentPullRequest
	// LeftOpen are those GitHub would not let maul close (or read), and
	// those in a repository the Project no longer lists: a person closes
	// them.
	LeftOpen []IntentPullRequest
}

// RenderReplanInReviewNotice renders a ReplanInReviewNotice.
func RenderReplanInReviewNotice(n ReplanInReviewNotice) (string, error) {
	return render("intent_notice_replan_review.md.tmpl", struct {
		Marker       string
		Asked        string
		Actor        string
		PlanRevision int32
		Closed       []statusPR
		Merged       []statusPR
		LeftOpen     []statusPR
	}{
		Marker:       NoticeMarker(n.Namespace, n.Intent, n.Key),
		Asked:        askedBy(n.Verb, n.Label),
		Actor:        oneLine(n.Actor),
		PlanRevision: n.PlanRevision,
		Closed:       linkedPRs(n.Closed),
		Merged:       linkedPRs(n.Merged),
		LeftOpen:     linkedPRs(n.LeftOpen),
	})
}

// ReplanClosedKey is the notice key of the comment maul posts on each pull
// request a replan in review closes, for the replan answered under key.
func ReplanClosedKey(key string) string { return "replan-" + key }

// ReplanClosedComment is the comment maul posts on a pull request before a
// replan in review closes it.
type ReplanClosedComment struct {
	// Namespace, Intent and Key make the comment's marker (NoticeMarker,
	// keyed ReplanClosedKey).
	Namespace string
	Intent    string
	Key       string
	// Actor is the approver who replanned.
	Actor string
	// Verb is the command's verb, or Label the trigger label it arrived as.
	Verb  string
	Label string
	// Branch is the pull request's head branch, left as it is.
	Branch string
}

// RenderReplanClosedComment renders a ReplanClosedComment.
func RenderReplanClosedComment(c ReplanClosedComment) (string, error) {
	return render("intent_replan_closed_pr.md.tmpl", struct {
		Marker string
		Asked  string
		Actor  string
		Branch string
	}{
		Marker: NoticeMarker(c.Namespace, c.Intent, c.Key),
		Asked:  replanBy(c.Verb, c.Label),
		Actor:  oneLine(c.Actor),
		Branch: oneLine(c.Branch),
	})
}

// ReplanRefusal is why a replan in review was refused.
type ReplanRefusal string

// Why a replan in review is refused.
const (
	// ReplanRefusedMerged: a pull request of the intent has merged; what
	// merged cannot be planned again.
	ReplanRefusedMerged ReplanRefusal = "merged"
	// ReplanRefusedAllMerged: every pull request of the intent merged after
	// the replan was taken up and before it closed them: the intent is
	// complete, and nothing is planned again.
	ReplanRefusedAllMerged ReplanRefusal = "all-merged"
	// ReplanRefusedCost: the intent is at its cost ceiling, which a new plan
	// would not lift.
	ReplanRefusedCost ReplanRefusal = "cost"
	// ReplanRefusedLimit: the intent has no plan revision left.
	ReplanRefusedLimit ReplanRefusal = "limit"
)

// ReplanRefusedNotice answers a replan in review maul refused.
type ReplanRefusedNotice struct {
	// Namespace, Intent and Key make the notice's marker (NoticeMarker).
	Namespace string
	Intent    string
	Key       string
	// Verb is the command's verb, or Label the trigger label it arrived as.
	Verb  string
	Label string
	// Reason is why it was refused.
	Reason ReplanRefusal
	// Merged are the pull requests already merged (ReplanRefusedMerged,
	// ReplanRefusedAllMerged).
	Merged []IntentPullRequest
	// SpentMicroUSD and CeilingMicroUSD are the spend and the Project's
	// per-intent ceiling (ReplanRefusedCost).
	SpentMicroUSD   int64
	CeilingMicroUSD int64
}

// errNoReplanRefusal: a refusal notice was asked for with no reason.
var errNoReplanRefusal = errors.New("render replan refusal: no reason")

// RenderReplanRefusedNotice renders a ReplanRefusedNotice.
func RenderReplanRefusedNotice(n ReplanRefusedNotice) (string, error) {
	switch n.Reason {
	case ReplanRefusedMerged, ReplanRefusedAllMerged, ReplanRefusedCost, ReplanRefusedLimit:
	default:
		return "", fmt.Errorf("%w %q", errNoReplanRefusal, n.Reason)
	}
	return render("intent_notice_replan_refused.md.tmpl", struct {
		Marker  string
		Asked   string
		Reason  string
		Merged  []statusPR
		Spent   string
		Ceiling string
	}{
		Marker:  NoticeMarker(n.Namespace, n.Intent, n.Key),
		Asked:   askedBy(n.Verb, n.Label),
		Reason:  string(n.Reason),
		Merged:  linkedPRs(n.Merged),
		Spent:   usd(n.SpentMicroUSD),
		Ceiling: usd(n.CeilingMicroUSD),
	})
}

// askedBy names a replan as it was asked, to open a sentence: the command,
// or adding the label.
func askedBy(verb, label string) string {
	if c := slashCommand(verb); c != "" {
		return c
	}
	return "Adding the " + code(oneLine(label)) + " label"
}

// replanBy names what asked for a replan inside a sentence: the command, or
// the label.
func replanBy(verb, label string) string {
	if c := slashCommand(verb); c != "" {
		return c
	}
	return "the " + code(oneLine(label)) + " label"
}
