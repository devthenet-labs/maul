// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// TestPlanCommentShowsTheAcceptanceSplit: the plan comment's header states
// what of the plan's acceptance criteria maul checks and what needs a human,
// before anyone approves it (design D1). Live, a plan with four criteria was
// posted with no such line. A plan with no criteria for a Project that
// verifies says so; criteria for a Project that is not verified are named
// unchecked; a plan with no criteria for a Project that does not verify
// carries no line, as before verification existed.
func TestPlanCommentShowsTheAcceptanceSplit(t *testing.T) {
	notVerifying := func(p *v1alpha1.Project) { p.Spec.Verification = nil }
	for _, tt := range []struct {
		name         string
		plan         string
		mutate       []func(*v1alpha1.Project)
		verification bool
		want         string
	}{
		{"criteria", acceptancePlan, nil, true,
			"**Acceptance:** 1 checked automatically, 1 needs you: sounds-good (only a human can judge it)."},
		{"no criteria", validPlan, nil, true, "**Acceptance:** the plan declares no acceptance criteria."},
		{"project does not verify", acceptancePlan, []func(*v1alpha1.Project){notVerifying}, true,
			"**Acceptance:** not checked: verification is off for this Project."},
		{"controller does not verify", acceptancePlan, nil, false,
			"**Acceptance:** not checked: verification is off for this Project."},
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

// TestPlanAimedAtNothingIsRefused: an acceptance criterion whose target is
// not a previewed repository of the Project refuses the plan when its run is
// collected, as the docs promise, so no approver is ever asked to approve a
// check aimed at nothing (before, it was approved and only then put the
// intent in NeedsReplan). The retry is told why.
func TestPlanAimedAtNothingIsRefused(t *testing.T) {
	for _, tt := range []struct{ name, plan, want string }{
		{"unpreviewed target", strings.Replace(acceptancePlan, "target: app", "target: api", 1),
			"criterion health targets api"},
		{"project repository without a preview", acceptancePlan, "criterion health targets app"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*v1alpha1.Project)
			if tt.plan == acceptancePlan {
				mutate = append(mutate, func(p *v1alpha1.Project) { p.Spec.Preview = nil })
			}
			e := newVerifyEnv(t, "", mutate...)
			e.verifying(true, true)
			e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
				if spec.Phase == "plan" {
					return planOutput(envelope.Stage{Outcome: envelope.OutcomeOK}, tt.plan)
				}
				return defaultOutput(spec)
			}
			name := e.newIntent(approver)
			e.drive(name, v1alpha1.IntentFailed, "")
			runs := e.runsOf(name, v1alpha1.IntentStagePlan)
			if len(runs) == 0 {
				t.Fatal("no plan run")
			}
			for _, r := range runs {
				if r.Status.Outcome != "report_invalid" || !strings.Contains(r.Status.Detail, tt.want) {
					t.Errorf("run %s = %s: %s, want report_invalid naming %q", r.Name, r.Status.Outcome,
						r.Status.Detail, tt.want)
				}
			}
			if len(runs) > 1 {
				if prev := runs[1].Spec.PreviousAttempt; prev == nil || !strings.Contains(prev.Detail, tt.want) {
					t.Errorf("the retry was not told why: %+v", prev)
				}
			}
			if n := len(e.gh.withMarker("maul:plan")); n != 0 {
				t.Errorf("a plan aimed at nothing was posted for approval (%d)", n)
			}
		})
	}
}
