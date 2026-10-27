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
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

const (
	splitLine    = "**Acceptance:** 1 checked automatically, 1 needs you: sounds-good (only a human can judge it)."
	noneLine     = "**Acceptance:** the plan declares no acceptance criteria."
	offLine      = "**Acceptance:** not checked: verification is off for this Project."
	notAskedLine = "**Acceptance:** not asked for: this Project verifies, but previews no repository a criterion " +
		"could target"
)

// unpreviewed is acceptancePlan with its http criterion aimed at api, which
// no Project here previews.
var unpreviewed = strings.Replace(acceptancePlan, "target: app", "target: api", 1)

var (
	notVerifying = func(p *v1alpha1.Project) { p.Spec.Verification = nil }
	noPreview    = func(p *v1alpha1.Project) { p.Spec.Preview = nil }
)

// TestPlanCommentShowsTheAcceptanceSplit: the plan comment's header states
// what of the plan's acceptance criteria maul checks and what needs a human,
// before anyone approves it (design D1). Live, a plan with four criteria was
// posted with no such line. A plan with no criteria for a Project that
// verifies says so; a Project that verifies but previews nothing says the
// planner could not be asked, and why; criteria for a Project that is not
// verified are named unchecked, kept rather than refused (an optional
// feature never fails an intent); a plan with no criteria for a Project that
// does not verify carries no line, as before verification existed.
func TestPlanCommentShowsTheAcceptanceSplit(t *testing.T) {
	for _, tt := range []struct {
		name         string
		plan         string
		mutate       []func(*v1alpha1.Project)
		verification bool
		want         string
	}{
		{"criteria", acceptancePlan, nil, true, splitLine},
		{"no criteria", validPlan, nil, true, noneLine},
		{"nothing previewed", validPlan, []func(*v1alpha1.Project){noPreview}, true, notAskedLine},
		{"nothing previewed, criteria anyway", acceptancePlan, []func(*v1alpha1.Project){noPreview}, true,
			notAskedLine},
		{"project does not verify", acceptancePlan, []func(*v1alpha1.Project){notVerifying}, true, offLine},
		{"project does not verify, target unpreviewed", unpreviewed,
			[]func(*v1alpha1.Project){notVerifying}, true, offLine},
		{"controller does not verify", acceptancePlan, nil, false, offLine},
		{"controller does not verify, target unpreviewed", unpreviewed, nil, false, offLine},
		{"neither", validPlan, []func(*v1alpha1.Project){notVerifying}, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newVerifyEnv(t, tt.plan, tt.mutate...)
			e.verifying(tt.verification, true)
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

// TestPlanAimedAtNothingIsRefused: when the plan will be verified, an
// acceptance criterion whose target is not a previewed repository of the
// Project refuses the plan when its run is collected, as the docs promise,
// so no approver is asked to approve a check aimed at nothing (before, it
// was approved and only then put the intent in NeedsReplan). The retry is
// told why. (Unverified, such a criterion is kept: see
// TestPlanCommentShowsTheAcceptanceSplit.)
func TestPlanAimedAtNothingIsRefused(t *testing.T) {
	const want = "criterion health targets api"
	e := newVerifyEnv(t, "")
	e.verifying(true, true)
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return planOutput(envelope.Stage{Outcome: envelope.OutcomeOK}, unpreviewed)
		}
		return defaultOutput(spec)
	}
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentFailed, "")
	runs := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(runs) < 2 {
		t.Fatalf("plan runs = %d, want a refused plan retried", len(runs))
	}
	for _, r := range runs {
		if r.Status.Outcome != "report_invalid" || !strings.Contains(r.Status.Detail, want) {
			t.Errorf("run %s = %s: %s, want report_invalid naming %q", r.Name, r.Status.Outcome, r.Status.Detail, want)
		}
	}
	if prev := runs[1].Spec.PreviousAttempt; prev == nil || !strings.Contains(prev.Detail, want) {
		t.Errorf("the retry was not told why: %+v", prev)
	}
	if n := len(e.gh.withMarker("maul:plan")); n != 0 {
		t.Errorf("a plan aimed at nothing was posted for approval (%d)", n)
	}
}

// TestPlanCollectRetriesATransientProjectRead: a plan run is collected
// against its Project, and a failure to read it that is not NotFound is
// transient: the pass returns the error and requeues, the plan is never
// refused as report_invalid, and no attempt is spent; once the read
// succeeds the plan is offered as usual.
func TestPlanCollectRetriesATransientProjectRead(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.verifying(true, true)
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
