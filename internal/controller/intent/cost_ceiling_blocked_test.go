// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// setSuspend suspends or resumes the Project, as a human editing
// spec.suspend would.
func (e *env) setSuspend(suspend bool) {
	e.t.Helper()
	var proj v1alpha1.Project
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &proj); err != nil {
		e.t.Fatal(err)
	}
	proj.Spec.Suspend = suspend
	proj.Generation++
	if err := e.c.Update(context.Background(), &proj); err != nil {
		e.t.Fatal(err)
	}
}

// wantCeilingBlock fails t unless in is Blocked from from on the cost
// ceiling.
func wantCeilingBlock(t *testing.T, in *v1alpha1.Intent, from v1alpha1.IntentPhase) {
	t.Helper()
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted)
	if in.Status.Phase != v1alpha1.IntentBlocked || c == nil || c.Status != metav1.ConditionTrue ||
		c.Reason != "CostCeilingReached" {
		t.Fatalf("phase %s, conditions %+v; want Blocked on BudgetExhausted CostCeilingReached", in.Status.Phase,
			in.Status.Conditions)
	}
	if got := v1alpha1.IntentBlockedFrom(in); got != from {
		t.Fatalf("blocked from %s, want %s", got, from)
	}
}

// creatingCache is laggingCache that also counts every IntentRun the
// intent reconciler creates through it.
type creatingCache struct {
	laggingCache
	creates *int
}

func (c creatingCache) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*v1alpha1.IntentRun); ok {
		*c.creates++
	}
	return c.laggingCache.Create(ctx, obj, opts...)
}

// TestCostCeilingGatesResumedBuild: a build blocked on its image resumes
// with its next attempt only below the ceiling. The spend reached it while
// the intent was blocked: the resume creates no build and the intent stays
// Blocked from Building, now on the ceiling, until the ceiling is raised.
func TestCostCeilingGatesResumedBuild(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentBlocked, "") // no image: ImageRequired
	spent := in.Status.Usage.CostMicroUSD
	if spent <= 0 {
		t.Fatalf("the plan reported no cost (%d): the ceiling cannot be reached", spent)
	}
	e.setCeiling(spent) // a Project change: the image block is tried again
	e.settleActions(name)
	wantCeilingBlock(t, e.get(name), v1alpha1.IntentBuilding)
	if n := len(e.runsOf(name, v1alpha1.IntentStageBuild)); n != 1 {
		t.Fatalf("build runs = %d at the ceiling, want only the image-blocked one", n)
	}

	e.setCeiling(10000000)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if n := len(e.runsOf(name, v1alpha1.IntentStageBuild)); n != 2 {
		t.Errorf("build runs = %d once the ceiling was raised, want the second attempt", n)
	}
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Error("BudgetExhausted still True after the ceiling was raised")
	}
}

// TestCostCeilingGatesResumedPlan: a plan blocked on an unschedulable pod
// resumes with its next attempt only below the ceiling.
func TestCostCeilingGatesResumedPlan(t *testing.T) {
	e := newEnv(t, testProject())
	e.unschedulableFor("plan")
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentBlocked, repoImage)
	plan := e.runsOf(name, v1alpha1.IntentStagePlan)[0]
	plan.Status.Usage.CostUSD = "0.25"
	if err := e.c.Status().Update(context.Background(), &plan); err != nil {
		t.Fatal(err)
	}
	e.mustIntent(name) // the spend is recorded
	in := e.get(name)
	if in.Status.Usage.CostMicroUSD <= 0 {
		t.Fatalf("spend %d, want the plan's", in.Status.Usage.CostMicroUSD)
	}
	e.jobs.statusFn = nil
	e.setCeiling(in.Status.Usage.CostMicroUSD) // a Project change: the resource block lifts
	e.settleActions(name)
	wantCeilingBlock(t, e.get(name), v1alpha1.IntentPlanning)
	if n := len(e.runsOf(name, v1alpha1.IntentStagePlan)); n != 1 {
		t.Fatalf("plan runs = %d at the ceiling, want only the unschedulable one", n)
	}

	e.setCeiling(10000000)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if n := len(e.runsOf(name, v1alpha1.IntentStagePlan)); n != 2 {
		t.Errorf("plan runs = %d once the ceiling was raised, want the retry", n)
	}
}

// TestBlockedInReviewEndsOnPullRequest: an intent blocked from review, on
// whatever block, still follows its pull request: a merge makes it Merged,
// a close unmerged Closed, and the issue is closed either way. Nothing
// leaves it Blocked once its pull request has settled.
func TestBlockedInReviewEndsOnPullRequest(t *testing.T) {
	reviewDue := func(e *env, name string) {
		e.reviewOn(e.get(name).Status.PullRequests[0].Number, 501, "Add a test.")
		e.clock.Advance(3 * time.Minute)
	}
	ceilingFromReview := func(t *testing.T, e *env, name string) {
		e.setCeiling(e.get(name).Status.Usage.CostMicroUSD)
		reviewDue(e, name)
		wantCeilingBlock(t, e.drive(name, v1alpha1.IntentBlocked, repoImage), v1alpha1.IntentInReview)
	}
	for _, tc := range []struct {
		name   string
		limits func(*v1alpha1.Project)
		block  func(t *testing.T, e *env, name string)
		merged bool
		want   v1alpha1.IntentPhase
	}{
		{name: "cost ceiling, merged", block: ceilingFromReview, merged: true, want: v1alpha1.IntentMerged},
		{name: "cost ceiling, closed", block: ceilingFromReview, want: v1alpha1.IntentClosed},
		{name: "revision limit, merged", merged: true, want: v1alpha1.IntentMerged,
			limits: func(p *v1alpha1.Project) { p.Spec.Limits.MaxRevisions = new(int32(0)) },
			block: func(t *testing.T, e *env, name string) {
				reviewDue(e, name)
				in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
				if !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionRevisionLimitReached) {
					t.Fatalf("conditions = %+v, want RevisionLimitReached", in.Status.Conditions)
				}
			}},
		{name: "cost ceiling from revising, merged", merged: true, want: v1alpha1.IntentMerged,
			block: func(t *testing.T, e *env, name string) {
				reviewDue(e, name)
				in := e.drive(name, v1alpha1.IntentRevising, repoImage)
				e.setCeiling(in.Status.Usage.CostMicroUSD)
				e.gh.failNext("FastForwardRef", ghclient.ErrNotFastForward)
				wantCeilingBlock(t, e.drive(name, v1alpha1.IntentBlocked, repoImage), v1alpha1.IntentRevising)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProject()
			if tc.limits != nil {
				tc.limits(p)
			}
			e := newEnv(t, p)
			name := e.awaiting()
			e.gh.label(1, "maul:approved", approver)
			e.drive(name, v1alpha1.IntentInReview, repoImage)
			tc.block(t, e, name)

			e.gh.closePR(tc.merged)
			in := e.drive(name, tc.want, repoImage)
			want := prClosed
			if tc.merged {
				want = prMerged
			}
			if got := in.Status.PullRequests[0].State; got != want {
				t.Errorf("recorded pull request state %s, want %s", got, want)
			}
			if got := e.gh.state(); got != "closed" {
				t.Errorf("issue state = %s, want closed", got)
			}
		})
	}
}

// TestCostCeilingAdoptsLeaseThroughALaggingCache: round 2 is leased on app
// below the ceiling and the write entering Revising is lost; the spend then
// reaches the ceiling, and the next pass lists its runs from a cache that
// has not seen the lease while web has a review due. The lease, found live
// (errRoundLeased), is adopted: nothing is created and nothing blocks.
func TestCostCeilingAdoptsLeaseThroughALaggingCache(t *testing.T) {
	e := newMultiEnv(t)
	name := e.inReviewLinked()
	e.reviewOn(1, 961, "App: rename the handler.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.settleActions(name)

	e.reviewOn(1, 962, "App: and the route.")
	e.clock.Advance(3 * time.Minute)
	e.failStatusIf = func(in *v1alpha1.Intent) bool { return in.Status.Phase == v1alpha1.IntentRevising }
	for e.reviseRun(name, 2) == nil {
		_ = e.reconcileIntent(name)
		e.clock.Advance(time.Minute)
	}
	lease := e.reviseRun(name, 2)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.Rounds != 1 {
		t.Fatalf("after the lost write: phase %s, rounds %d", in.Status.Phase, in.Status.Rounds)
	}
	e.setCeiling(in.Status.Usage.CostMicroUSD)
	creates := 0
	e.intent.Client = creatingCache{laggingCache{Client: e.c, hidden: map[string]bool{lease.Name: true}}, &creates}
	e.reviewOn(2, 963, "Web: show the sha.")
	e.clock.Advance(3 * time.Minute)
	e.mustIntent(name)

	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentRevising || in.Status.Rounds != 2 || in.Status.ActiveRun == nil ||
		in.Status.ActiveRun.Name != lease.Name {
		t.Fatalf("phase %s, rounds %d, activeRun %+v; want app's lease of round 2 entered", in.Status.Phase,
			in.Status.Rounds, in.Status.ActiveRun)
	}
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Error("BudgetExhausted True: the leased round was refused")
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); creates != 0 || n != 2 {
		t.Errorf("%d runs created at the ceiling (%d revise runs), want none", creates, n)
	}
}

// TestCostCeilingAdoptsRetryAttempt: a round's retry attempt created below
// the ceiling, then unseen by a lagging cache once the spend reached it, is
// adopted by a live read alone: no create is even tried, and the round is
// not blocked.
func TestCostCeilingAdoptsRetryAttempt(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.reviewOn(in.Status.PullRequests[0].Number, 901, "Please add a test.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.gh.failNext("FastForwardRef", ghclient.ErrNotFastForward)
	for range 30 {
		e.mustIntent(name)
		if len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 2 {
			break
		}
		e.readyRepositories(repoImage)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 2 || runs[0].Status.Outcome != OutcomeHeadMoved {
		t.Fatalf("revise runs = %+v, want a head_moved attempt and its retry", runs)
	}
	retry := runs[1]
	e.setCeiling(e.get(name).Status.Usage.CostMicroUSD)
	creates := 0
	e.intent.Client = creatingCache{laggingCache{Client: e.c, hidden: map[string]bool{retry.Name: true}}, &creates}
	e.settleActions(name)

	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentRevising ||
		meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("phase %s, conditions %+v; want the retry adopted, not blocked", in.Status.Phase,
			in.Status.Conditions)
	}
	if in.Status.ActiveRun == nil || in.Status.ActiveRun.Name != retry.Name {
		t.Errorf("activeRun %+v, want the retry %s", in.Status.ActiveRun, retry.Name)
	}
	if creates != 0 {
		t.Errorf("%d creates tried at the ceiling, want the retry adopted by a read alone", creates)
	}
}

// TestCostCeilingWaitsWhileSuspended: a suspended Project launches nothing,
// so a round due at the ceiling neither leases a run nor blocks the intent
// while it stays suspended, as a plan or build launch waits; resumed, the
// intent is Blocked on the ceiling.
func TestCostCeilingWaitsWhileSuspended(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.setCeiling(in.Status.Usage.CostMicroUSD)
	e.setSuspend(true)
	e.reviewOn(in.Status.PullRequests[0].Number, 501, "Add a test.")
	e.clock.Advance(3 * time.Minute)
	e.settleActions(name)
	in = e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview ||
		meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionBudgetExhausted) {
		t.Fatalf("phase %s, conditions %+v while suspended; want it waiting in review", in.Status.Phase,
			in.Status.Conditions)
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
		t.Fatalf("%d revise runs at the ceiling", n)
	}

	e.setSuspend(false)
	wantCeilingBlock(t, e.drive(name, v1alpha1.IntentBlocked, repoImage), v1alpha1.IntentInReview)
}

// ceilingReplies are maul's replies on pull request n saying a command
// waits for the cost ceiling.
func (e *env) ceilingReplies(n int64) []string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []string
	for _, c := range e.gh.prComments[n] {
		if c.UserLogin == e.gh.bot && strings.Contains(c.Body, "limits.maxCostMicroUSD") {
			out = append(out, c.Body)
		}
	}
	return out
}

// TestCostCeilingAnswersPRCommand: a /maul revise arriving at the ceiling
// is answered on the pull request, once, saying the ceiling holds it and how
// to raise it, so the round that starts once it is raised surprises no one;
// that round is then acknowledged as any other.
func TestCostCeilingAnswersPRCommand(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	pr := in.Status.PullRequests[0]
	e.setCeiling(in.Status.Usage.CostMicroUSD)
	e.gh.mu.Lock()
	e.gh.prComments[pr.Number] = append(e.gh.prComments[pr.Number], &ghclient.Comment{ID: 940,
		NodeID: "comment-940", UserLogin: approver, UserID: actorOf(approver).ID, UserType: "User",
		Body: "/maul revise Please check the response header.", CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
	e.gh.mu.Unlock()
	e.clock.Advance(time.Second)

	wantCeilingBlock(t, e.drive(name, v1alpha1.IntentBlocked, repoImage), v1alpha1.IntentInReview)
	e.settleActions(name)
	e.setCeiling(e.get(name).Status.Usage.CostMicroUSD) // a Project change while still at the ceiling
	e.settleActions(name)
	replies := e.ceilingReplies(pr.Number)
	if len(replies) != 1 || !strings.Contains(replies[0], "<!-- maul:intent-pr-command-ceiling:"+name+":940 -->") {
		t.Fatalf("ceiling replies = %q, want one, marked for the command", replies)
	}

	e.setCeiling(10000000)
	e.driveUntil(name, func(*v1alpha1.Intent) bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 1 })
	e.gh.mu.Lock()
	started := 0
	for _, c := range e.gh.prComments[pr.Number] {
		if markerOf(c.Body) == fmt.Sprintf("<!-- maul:intent-pr-command:%s:940 -->", name) {
			started++
		}
	}
	e.gh.mu.Unlock()
	if started != 1 {
		t.Errorf("%d round-started replies once the ceiling was raised, want 1", started)
	}
}
