// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// TestHumanPending: only what a review or command round would act on is a
// human's pending feedback: an approver's unedited CHANGES_REQUESTED review
// not yet consumed (inside its quiet window too), or an approver's unedited
// /maul revise on the pull request. An approval, a comment-only review, a
// non-approver's or an edited review, and any other command are not.
func TestHumanPending(t *testing.T) {
	review := func(id int64, login, state string) ghclient.Review {
		return ghclient.Review{ID: id, NodeID: fmt.Sprintf("review-%d", id), Author: actorOf(login), State: state,
			Body: "feedback"}
	}
	for _, tc := range []struct {
		name    string
		reviews []ghclient.Review
		edited  []string
		comment *struct{ login, body string }
		want    v1alpha1.AutoFixReason
	}{
		{"nothing", nil, nil, nil, ""},
		{"approved", []ghclient.Review{review(11, approver, "APPROVED")}, nil, nil, ""},
		{"commented", []ghclient.Review{review(12, approver, "COMMENTED")}, nil, nil, ""},
		{"changes requested", []ghclient.Review{review(13, approver, "CHANGES_REQUESTED")}, nil, nil,
			v1alpha1.AutoFixHumanReview},
		{"by a non-approver", []ghclient.Review{review(14, "outsider", "CHANGES_REQUESTED")}, nil, nil, ""},
		{"edited", []ghclient.Review{review(15, approver, "CHANGES_REQUESTED")}, []string{"review-15"}, nil, ""},
		{"revise command", nil, nil, &struct{ login, body string }{approver, "/maul revise please"},
			v1alpha1.AutoFixHumanCommand},
		{"revise by a non-approver", nil, nil, &struct{ login, body string }{"outsider", "/maul revise"}, ""},
		{"verify command", nil, nil, &struct{ login, body string }{approver, "/maul verify"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, acceptancePlan)
			name := e.reviewing()
			n := e.prNumber(name)
			e.clock.Advance(time.Second)
			e.gh.mu.Lock()
			for _, r := range tc.reviews {
				r.SubmittedAt = e.clock.Now()
				e.gh.reviews[n] = append(e.gh.reviews[n], r)
			}
			for _, node := range tc.edited {
				e.gh.reviewEdits[node] = true
			}
			e.gh.mu.Unlock()
			if tc.comment != nil {
				e.prCommand(n, tc.comment.login, tc.comment.body)
			}
			p := e.prVerifyPass(name)
			got, err := p.humanPending(context.Background(), p.in.Status.PullRequests)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("humanPending = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHumanPendingIgnoresAConsumedReview: a review a round already took is
// no longer pending.
func TestHumanPendingIgnoresAConsumedReview(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	n := e.prNumber(name)
	e.reviewOn(n, 1901, "Fix the route.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	p := e.prVerifyPass(name)
	got, err := p.humanPending(context.Background(), p.in.Status.PullRequests)
	if err != nil || got != "" {
		t.Fatalf("humanPending = %q, %v; want nothing", got, err)
	}
}

// TestHumanReasonExhaustive: every trigger has its halt reason: a human's
// round names what started it, maul's own rounds none.
func TestHumanReasonExhaustive(t *testing.T) {
	want := map[v1alpha1.IntentRunTrigger]v1alpha1.AutoFixReason{
		tReview: v1alpha1.AutoFixHumanReview, tCommand: v1alpha1.AutoFixHumanCommand, tChecks: "", tVerify: "",
	}
	for _, tr := range v1alpha1.IntentRunTriggers {
		w, ok := want[tr]
		if !ok {
			t.Errorf("no row for trigger %q", tr)
			continue
		}
		if got := humanReason(tr); got != w {
			t.Errorf("humanReason(%q) = %q, want %q", tr, got, w)
		}
	}
}
