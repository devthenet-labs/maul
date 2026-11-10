// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

const notAskedLine = "**Acceptance:** not asked for: this Project verifies, but previews no repository a " +
	"criterion could target"

var (
	notVerifying = func(p *v1alpha1.Project) { p.Spec.Verification = nil }
	noPreview    = func(p *v1alpha1.Project) { p.Spec.Preview = nil }
)

// TestPlanCommentShowsTheAcceptanceSplit: the plan comment's header says,
// before anyone approves it (design D1), when a Project that verifies
// previews nothing its outcomes could be checked on, so the planner could
// not be asked, and why; a plan of a Project or a controller that does not
// verify carries no acceptance line, as before verification existed (the
// outcomes an asked plan states are TestEvaluatorPlanCommentListsOutcomes').
func TestPlanCommentShowsTheAcceptanceSplit(t *testing.T) {
	for _, tt := range []struct {
		name         string
		plan         string
		mutate       []func(*v1alpha1.Project)
		verification bool
		want         string
	}{
		{"nothing previewed", validPlan, []func(*v1alpha1.Project){noPreview}, true, notAskedLine},
		{"nothing previewed, outcomes anyway", outcomesPlan, []func(*v1alpha1.Project){noPreview}, true,
			notAskedLine},
		{"project does not verify", outcomesPlan, []func(*v1alpha1.Project){notVerifying}, true, ""},
		{"controller does not verify", outcomesPlan, nil, false, ""},
		{"neither", validPlan, []func(*v1alpha1.Project){notVerifying}, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newVerifyEnv(t, tt.plan, tt.mutate...)
			e.verifying(tt.verification, true)
			e.evaluating(tt.verification)
			e.awaiting()
			plans := e.gh.withMarker("maul:plan")
			if len(plans) != 1 {
				t.Fatalf("plan comments = %d, want 1", len(plans))
			}
			body := plans[0].Body
			if tt.want == "" {
				if strings.Contains(body, "**Acceptance:**") {
					t.Errorf("plan comment carries an acceptance line:\n%s", body)
				}
				return
			}
			if !strings.Contains(body, tt.want) {
				t.Errorf("plan comment lacks %q:\n%s", tt.want, body)
			}
		})
	}
}

// TestPlanCollectRetriesATransientProjectRead: a plan run is collected
// against its Project, and a failure to read it that is not NotFound is
// transient: the pass returns the error and requeues, the plan is never
// refused as report_invalid, and no attempt is spent; once the read
// succeeds the plan is offered as usual.
func TestPlanCollectRetriesATransientProjectRead(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan)
	e.evaluatorOn(0.4)
	ctx := context.Background()
	failing := false
	e.getHook = func(obj client.Object) error {
		if _, ok := obj.(*v1alpha1.Project); ok && failing {
			return kerrors.NewServiceUnavailable("the API server is restarting")
		}
		return nil
	}
	name := e.newIntent(approver)
	// Launch the plan run, then fail every Project read while it is
	// collected.
	e.passUntil(name, func() bool {
		e.readyRepositories("")
		e.runRuns()
		e.jobs.mu.Lock()
		defer e.jobs.mu.Unlock()
		return len(e.jobs.specs) > 0
	})
	plan := e.runsOf(name, v1alpha1.IntentStagePlan)[0]
	if plan.Status.Outcome != "" {
		t.Fatalf("plan run %s settled before its collection was tested: %+v", plan.Name, plan.Status)
	}
	// The reconcile's earlier reads (intentEnded) already requeue on a
	// transient error, so collect the plan directly: its own read of the
	// Project is what is under test.
	spec := e.specsOf("plan")[0]
	failing = true
	err := e.runs.collectPlan(ctx, &plan, defaultOutput(spec).Events, nil, jobs.Status{})
	if err == nil || !kerrors.IsServiceUnavailable(err) {
		t.Errorf("collectPlan = %v, want the transient read's error, to requeue", err)
	}
	got := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(got) != 1 || got[0].Status.Outcome != "" {
		t.Fatalf("a transient Project read settled the plan: %+v", got[0].Status)
	}
	failing = false
	e.drive(name, v1alpha1.IntentAwaitingApproval, "")
	runs := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(runs) != 1 || runs[0].Status.Outcome != "ok" {
		t.Errorf("plan runs = %d, first %s, want the one plan offered", len(runs), runs[0].Status.Outcome)
	}
}
