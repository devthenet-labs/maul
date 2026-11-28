// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// supersededNothing is the detail of a run a replan in review stopped.
const supersededNothing = "a replan in review superseded the run's plan; nothing was pushed"

// runningRound starts a review round on the intent's pull request and holds
// its Job running; release lets it finish.
func (e *env) runningRound(name string) (release func()) {
	e.t.Helper()
	e.reviewOn(1, 801, "Rename the endpoint to /v1/version.")
	e.clock.Advance(3 * time.Minute)
	job := jobs.NameFor(v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 1, "app", 1), KindIntent, 1)
	e.jobs.mu.Lock()
	e.jobs.status[job] = jobs.Status{}
	e.jobs.mu.Unlock()
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		r := e.reviseRun(name, 1)
		return r != nil && r.Status.JobRef != nil
	})
	return func() {
		e.jobs.mu.Lock()
		delete(e.jobs.status, job)
		e.jobs.mu.Unlock()
	}
}

// TestReplanStopsTheRoundInFlight (critique 3): once a replan in review is
// recorded, the round running against the old plan's pull request pushes
// nothing: no commit, the branch where it was, the run aborted saying why;
// and so for a run of a plan revision that is no longer the approved one
// (the replan's Planning write made).
func TestReplanStopsTheRoundInFlight(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.Intent)
	}{
		{"the replan is recorded", func(in *v1alpha1.Intent) {
			in.Status.Replan = &v1alpha1.IntentReplan{PlanRevision: in.Status.Approval.PlanRevision,
				Trigger: v1alpha1.IntentAction{Source: v1alpha1.IntentActionCommand, EventID: 9, Login: approver,
					At: metav1.NewTime(time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC))}}
		}},
		{"another plan revision is approved", func(in *v1alpha1.Intent) {
			in.Status.Approval.PlanRevision++
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			name := e.reviewing()
			release := e.runningRound(name)
			commits, head := len(e.gh.commits), e.gh.branches[legacyBranch(name)]

			e.setStatus(name, tc.mutate)
			release()
			e.runRuns()
			run := e.reviseRun(name, 1)
			if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeAborted ||
				run.Status.Detail != supersededNothing {
				t.Fatalf("round = %s %s %q, want aborted: %q", run.Status.Phase, run.Status.Outcome,
					run.Status.Detail, supersededNothing)
			}
			if len(e.gh.commits) != commits || e.gh.branches[legacyBranch(name)] != head {
				t.Fatalf("commits %d -> %d, branch %s -> %s; want nothing pushed", commits, len(e.gh.commits), head,
					e.gh.branches[legacyBranch(name)])
			}
		})
	}
}

// TestReplanHoldsBackAPendingRound: a round leased before the replan was
// recorded, and not yet launched, is never launched.
func TestReplanHoldsBackAPendingRound(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.reviewing()
	e.reviewOn(1, 801, "Rename the endpoint to /v1/version.")
	e.clock.Advance(3 * time.Minute)
	e.driveUntil(name, func(*v1alpha1.Intent) bool { return e.reviseRun(name, 1) != nil })
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Replan = &v1alpha1.IntentReplan{PlanRevision: in.Status.Approval.PlanRevision,
			Trigger: v1alpha1.IntentAction{Source: v1alpha1.IntentActionCommand, EventID: 9, Login: approver,
				At: metav1.NewTime(time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC))}}
	})
	e.readyRepositories(repoImage)
	e.runRuns()
	run := e.reviseRun(name, 1)
	if run.Status.JobRef != nil {
		t.Fatalf("round launched a Job (%s) after the replan was recorded", run.Status.JobRef.Name)
	}
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Detail != supersededNothing {
		t.Errorf("round = %s %q, want aborted: %q", run.Status.Phase, run.Status.Detail, supersededNothing)
	}
}

// TestPushGateRefusesASupersededPlan: the gate every push passes reads the
// Intent uncached, so a push decided from a cache that lags the replan's
// record is still refused, as an ended push; the run of the approved plan
// revision, and a plan run, pass.
func TestPushGateRefusesASupersededPlan(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.reviewing()
	in := e.get(name)
	approved := in.Status.Approval.PlanRevision
	run := func(stage v1alpha1.IntentStage, rev int32) *v1alpha1.IntentRun {
		r := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{
			IntentRef: v1alpha1.ObjectReference{Name: name, UID: in.UID}, Stage: stage,
			Repository: v1alpha1.IntentRunRepository{URL: appRepoURL},
			Inputs:     v1alpha1.IntentRunInputs{PlanRevision: rev},
		}}
		r.Namespace = testNS
		return r
	}
	ctx := context.Background()
	if err := e.runs.pushGate(ctx, run(v1alpha1.IntentStageBuild, approved)); err != nil {
		t.Fatalf("pushGate of the approved plan's build = %v, want it let through", err)
	}
	if err := e.runs.pushGate(ctx, run(v1alpha1.IntentStageBuild, approved+1)); !errors.Is(err, errSuperseded) ||
		!errors.Is(err, errIntentEnded) {
		t.Errorf("pushGate of another plan revision's build = %v, want errSuperseded, an ended push", err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Replan = &v1alpha1.IntentReplan{PlanRevision: approved}
	})
	if err := e.runs.pushGate(ctx, run(v1alpha1.IntentStageBuild, approved)); !errors.Is(err, errSuperseded) {
		t.Errorf("pushGate while the replan is recorded = %v, want errSuperseded", err)
	}
	if planSuperseded(e.get(name), run(v1alpha1.IntentStagePlan, 0)) {
		t.Error("a plan run is superseded; it makes the plan")
	}
}
