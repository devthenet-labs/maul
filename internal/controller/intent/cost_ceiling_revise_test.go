// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// setCeiling sets the Project's per-intent cost ceiling, as a human editing
// limits.maxCostMicroUSD would.
func (e *env) setCeiling(ceiling int64) {
	e.t.Helper()
	var proj v1alpha1.Project
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &proj); err != nil {
		e.t.Fatal(err)
	}
	proj.Spec.Limits.MaxCostMicroUSD = ceiling
	proj.Generation++
	if err := e.c.Update(context.Background(), &proj); err != nil {
		e.t.Fatal(err)
	}
}

// TestCostCeilingGatesReviseRounds: an intent in review whose spend is at
// its Project's ceiling starts no review, command or check-fix round. It is
// Blocked from InReview exactly as a plan or build launch would be
// (BudgetExhausted, CostCeilingReached) with no revise run created, and once
// the ceiling is raised the same feedback starts its round.
func TestCostCeilingGatesReviseRounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger v1alpha1.IntentRunTrigger
		checks  bool
		due     func(e *env, pr v1alpha1.IntentPullRequest)
	}{
		{name: "review round", trigger: v1alpha1.IntentRunTriggerReview,
			due: func(e *env, pr v1alpha1.IntentPullRequest) {
				e.gh.reviews[pr.Number] = []ghclient.Review{{ID: 501, NodeID: "review-501",
					Author: actorOf(approver), State: "CHANGES_REQUESTED", Body: "Add a test.",
					SubmittedAt: e.clock.Now()}}
				e.clock.Advance(3 * time.Minute)
			}},
		{name: "command round", trigger: v1alpha1.IntentRunTriggerCommand,
			due: func(e *env, pr v1alpha1.IntentPullRequest) {
				e.gh.prComments[pr.Number] = []*ghclient.Comment{{ID: 940, NodeID: "comment-940",
					UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
					Body:      "/maul revise Please check the response header.",
					CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
				e.clock.Advance(time.Second)
			}},
		{name: "check-fix round", trigger: v1alpha1.IntentRunTriggerChecks, checks: true,
			due: func(e *env, pr v1alpha1.IntentPullRequest) {
				e.gh.checks[pr.HeadSHA] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: pr.HeadSHA,
					Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
					Output: ghclient.CheckOutput{Title: "Go failed", Summary: "failure"}}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProject()
			if tc.checks {
				p.Spec.Checks.Fix = []string{"test"}
			}
			e := newEnv(t, p)
			name := e.awaiting()
			e.gh.label(1, "maul:approved", approver)
			in := e.drive(name, v1alpha1.IntentInReview, repoImage)
			spent := in.Status.Usage.CostMicroUSD
			if spent <= 0 {
				t.Fatalf("the plan and build reported no cost (%d): the ceiling cannot be reached", spent)
			}
			e.setCeiling(spent)
			tc.due(e, in.Status.PullRequests[0])

			in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
			c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted)
			if c == nil || !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) ||
				c.Reason != "CostCeilingReached" {
				t.Fatalf("conditions = %+v, want BudgetExhausted CostCeilingReached", in.Status.Conditions)
			}
			if from := v1alpha1.IntentBlockedFrom(in); from != v1alpha1.IntentInReview {
				t.Fatalf("blocked from %s, want InReview", from)
			}
			if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 || in.Status.Rounds != 0 {
				t.Fatalf("%d revise runs (rounds %d) created at the ceiling", n, in.Status.Rounds)
			}

			e.setCeiling(10000000)
			e.driveUntil(name, func(*v1alpha1.Intent) bool {
				return len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 1
			})
			if run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]; run.Spec.Trigger != tc.trigger {
				t.Fatalf("revise run trigger = %s, want %s", run.Spec.Trigger, tc.trigger)
			}
			if in := e.get(name); meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
				t.Error("BudgetExhausted still True after the ceiling was raised")
			}
		})
	}
}

// TestCostCeilingIdleReviewIsNotBlocked: a spend at the ceiling with no
// round due leaves the intent in review: the ceiling gates launches, and an
// intent with nothing to launch is not blocked.
func TestCostCeilingIdleReviewIsNotBlocked(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.setCeiling(in.Status.Usage.CostMicroUSD)
	for range 5 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview {
		t.Fatalf("phase = %s with no round due, want InReview", in.Status.Phase)
	}
}

// TestCostCeilingGatesReviseRetry: a round whose attempt failed retryably
// creates no further attempt once the spend is at the ceiling: the Intent is
// Blocked from Revising on BudgetExhausted, and the retry is created once the
// ceiling is raised.
func TestCostCeilingGatesReviseRetry(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.reviews[in.Status.PullRequests[0].Number] = []ghclient.Review{{ID: 901, NodeID: "review-901",
		Author: actorOf(approver), State: "CHANGES_REQUESTED", Body: "Please add a test.",
		SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.setCeiling(in.Status.Usage.CostMicroUSD)
	e.gh.failNext("FastForwardRef", ghclient.ErrNotFastForward)

	in = e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted); c == nil ||
		c.Reason != "CostCeilingReached" {
		t.Fatalf("conditions = %+v, want BudgetExhausted CostCeilingReached", in.Status.Conditions)
	}
	if from := v1alpha1.IntentBlockedFrom(in); from != v1alpha1.IntentRevising {
		t.Fatalf("blocked from %s, want Revising", from)
	}
	if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 1 ||
		runs[0].Status.Outcome != OutcomeHeadMoved {
		t.Fatalf("revise attempts = %+v, want the one head_moved attempt and no retry", runs)
	}

	e.setCeiling(10000000)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 2 ||
		runs[1].Status.Phase != v1alpha1.RunComplete {
		t.Fatalf("revise attempts = %+v, want the retry complete once the ceiling was raised", runs)
	}
}

// TestCostCeilingAdoptsLeasedRound: a round a pass leased before the spend
// reached the ceiling, whose status write was then lost, is adopted, never
// stranded behind the block. Here the cache has seen the lease
// (adoptPendingRound); TestCostCeilingAdoptsLeaseThroughALaggingCache covers
// a cache that has not (errRoundLeased).
func TestCostCeilingAdoptsLeasedRound(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.reviews[in.Status.PullRequests[0].Number] = []ghclient.Review{{ID: 901, NodeID: "review-901",
		Author: actorOf(approver), State: "CHANGES_REQUESTED", Body: "Please add a test.",
		SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	for range 20 {
		e.mustIntent(name)
		if len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 1 {
			break
		}
	}
	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentRevising || in.Status.Rounds != 1 {
		t.Fatalf("phase %s rounds %d, want round 1 entered", in.Status.Phase, in.Status.Rounds)
	}
	// The pass's status write is lost: the Intent is still InReview with no
	// round, though round 1's run is leased.
	in.Status.Phase, in.Status.Rounds, in.Status.ActiveRun = v1alpha1.IntentInReview, 0, nil
	if err := e.c.Status().Update(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	e.setCeiling(in.Status.Usage.CostMicroUSD)

	for range 5 {
		e.clock.Advance(time.Minute)
		if e.mustIntent(name); e.get(name).Status.Phase != v1alpha1.IntentInReview {
			break
		}
	}
	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentRevising || in.Status.Rounds != 1 ||
		meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("phase %s rounds %d conditions %+v; want the leased round adopted, not blocked",
			in.Status.Phase, in.Status.Rounds, in.Status.Conditions)
	}
}
