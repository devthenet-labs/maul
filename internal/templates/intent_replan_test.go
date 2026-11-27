// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestReplanInReviewGoldens pins what maul says about a replan in review:
// the reply on the issue, the comment on each pull request it closes, and
// each refusal.
func TestReplanInReviewGoldens(t *testing.T) {
	prs := testMultiPRs()
	prs[0].State, prs[1].State = "closed", "closed"
	cases := []struct {
		name   string
		render func() (string, error)
	}{
		{"intent_notice_replan_review.md", func() (string, error) {
			return RenderReplanInReviewNotice(ReplanInReviewNotice{
				Namespace: "maul", Intent: "marigold-3", Key: "comment-41", Actor: "alice", Verb: "replan",
				PlanRevision: 2, Closed: prs,
			})
		}},
		{"intent_notice_replan_review_label_merged.md", func() (string, error) {
			merged := testMultiPRs()
			return RenderReplanInReviewNotice(ReplanInReviewNotice{
				Namespace: "maul", Intent: "marigold-3", Key: "event-7", Actor: "alice", Label: "maul:intent",
				PlanRevision: 2, Closed: merged[1:], Merged: merged[:1],
			})
		}},
		{"intent_notice_replan_review_left_open.md", func() (string, error) {
			prs := testMultiPRs()
			prs[0].State, prs[1].State = "closed", "open"
			return RenderReplanInReviewNotice(ReplanInReviewNotice{
				Namespace: "maul", Intent: "marigold-3", Key: "comment-41", Actor: "alice", Verb: "replan",
				PlanRevision: 2, Closed: prs[:1], LeftOpen: prs[1:],
			})
		}},
		{"intent_replan_closed_pr_label.md", func() (string, error) {
			return RenderReplanClosedComment(ReplanClosedComment{
				Namespace: "maul", Intent: "marigold-3", Key: ReplanClosedKey("event-7"), Actor: "alice",
				Label: "maul:intent", Branch: "maul-intent/marigold-3-p4",
			})
		}},
		{"intent_replan_closed_pr.md", func() (string, error) {
			return RenderReplanClosedComment(ReplanClosedComment{
				Namespace: "maul", Intent: "marigold-3", Key: ReplanClosedKey("comment-41"), Actor: "alice",
				Verb: "replan", Branch: "maul-intent/marigold-3",
			})
		}},
		{"intent_notice_replan_refused_merged.md", func() (string, error) {
			return RenderReplanRefusedNotice(ReplanRefusedNotice{
				Namespace: "maul", Intent: "marigold-3", Key: "comment-41", Verb: "replan",
				Reason: ReplanRefusedMerged, Merged: testMultiPRs()[:1],
			})
		}},
		{"intent_notice_replan_refused_cost.md", func() (string, error) {
			return RenderReplanRefusedNotice(ReplanRefusedNotice{
				Namespace: "maul", Intent: "marigold-3", Key: "event-7", Label: "maul:intent",
				Reason: ReplanRefusedCost, SpentMicroUSD: 10_250_000, CeilingMicroUSD: 10_000_000,
			})
		}},
		{"intent_notice_replan_refused_limit.md", func() (string, error) {
			return RenderReplanRefusedNotice(ReplanRefusedNotice{
				Namespace: "maul", Intent: "marigold-3", Key: "comment-41", Verb: "replan",
				Reason: ReplanRefusedLimit,
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.render()
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tc.name, got)
		})
	}
}

// TestReplanInReviewWords: the actor is code, never a mention; the reply
// says the new plan needs a new approval and the branches are left; the
// pull request comment says where the new pull requests will be linked.
func TestReplanInReviewWords(t *testing.T) {
	done, err := RenderReplanInReviewNotice(ReplanInReviewNotice{
		Namespace: "maul", Intent: "marigold-3", Key: "comment-41", Actor: "alice", Verb: "replan",
		PlanRevision: 2, Closed: testMultiPRs()[1:],
	})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := RenderReplanClosedComment(ReplanClosedComment{
		Namespace: "maul", Intent: "marigold-3", Key: ReplanClosedKey("comment-41"), Actor: "alice",
		Verb: "replan", Branch: "maul-intent/marigold-3",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{done, closed} {
		if strings.Contains(body, "@alice") || !strings.Contains(body, "`alice`") {
			t.Errorf("the actor is not named as code:\n%s", body)
		}
	}
	for _, want := range []string{"plan r2", "new approval", "left as they are"} {
		if !strings.Contains(done, want) {
			t.Errorf("the reply does not say %q:\n%s", want, done)
		}
	}
	for _, want := range []string{"Closed by `/maul replan` from `alice`", "linked on the issue",
		"`maul-intent/marigold-3` is left as it is"} {
		if !strings.Contains(closed, want) {
			t.Errorf("the pull request comment does not say %q:\n%s", want, closed)
		}
	}
	if _, err := RenderReplanRefusedNotice(ReplanRefusedNotice{Namespace: "maul", Intent: "m-3", Key: "k",
		Verb: "replan"}); err == nil {
		t.Error("a refusal with no reason rendered")
	}
}
