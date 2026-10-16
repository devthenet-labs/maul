// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"testing/quick"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
)

// costBudgets turns --broker-limit-unit cost on for every reconciler of the
// env, with an hourly limit of perHour micro-USD (0: none).
func (e *env) costBudgets(perHour int64) {
	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.CostBudgets, s.BrokerCostPerHour = true, perHour
	}
}

// jobDeadlines sets the intent Job deadline floor and ceiling on the run
// reconciler, the one that launches.
func (e *env) jobDeadlines(floor, ceiling time.Duration) {
	e.runs.Settings.JobDeadline, e.runs.Settings.JobDeadlineMax = floor, ceiling
}

// launchOf is the one Job launched for phase, with its stage env.
func (e *env) launchOf(t *testing.T, phase string) (jobs.Spec, map[string]string) {
	t.Helper()
	var found []string
	for name, s := range e.jobs.specs {
		if s.Phase == phase {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s Jobs launched, want 1", len(found), phase)
	}
	return e.jobs.specs[found[0]], e.jobs.envs[found[0]]
}

// TestStageCostBudgets: under the cost unit every run is granted its stage's
// budget (the Project's per-intent ceiling standing in for a stage that sets
// none), records it on its immutable spec and launches with it; under the
// token unit, the default, no run records one and no Job carries one.
func TestStageCostBudgets(t *testing.T) {
	p := testProject()
	p.Spec.Limits.MaxCostMicroUSD = 80000000
	p.Spec.Limits.Build = v1alpha1.StageLimits{MaxCostMicroUSD: 60000000}
	e := newEnv(t, p)
	e.costBudgets(0)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)

	for stage, want := range map[v1alpha1.IntentStage]int64{
		v1alpha1.IntentStagePlan:  80000000,
		v1alpha1.IntentStageBuild: 60000000,
	} {
		runs := e.runsOf(name, stage)
		if len(runs) != 1 || runs[0].Spec.Grant.BudgetMicroUSD != want {
			t.Errorf("%s runs %d, budget %+v; want one granted %d", stage, len(runs), runs, want)
		}
	}
	if s, _ := e.launchOf(t, "plan"); s.BudgetMicroUSD != 80000000 {
		t.Errorf("plan Job budget = %d, want the per-intent ceiling 80000000", s.BudgetMicroUSD)
	}
	if s, _ := e.launchOf(t, "build"); s.BudgetMicroUSD != 60000000 {
		t.Errorf("build Job budget = %d, want the stage's 60000000", s.BudgetMicroUSD)
	}

	off := newEnv(t, p)
	name = off.newIntent(approver)
	off.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	off.clock.Advance(time.Minute)
	off.gh.label(1, "patchy:approved", approver)
	off.drive(name, v1alpha1.IntentInReview, repoImage)
	for _, run := range off.intentRuns(name) {
		if run.Spec.Grant.BudgetMicroUSD != 0 {
			t.Errorf("run %s granted a budget under the token unit: %d", run.Name, run.Spec.Grant.BudgetMicroUSD)
		}
	}
	for _, s := range off.jobs.launched() {
		if s.BudgetMicroUSD != 0 {
			t.Errorf("a %s Job carries a budget under the token unit: %d", s.Phase, s.BudgetMicroUSD)
		}
	}
}

// TestRunBudgetForAnUnbudgetedRun: a run granted before the broker counted
// cost still launches with a budget once it does, its Project's for the
// stage, so no intent Job goes without one; a granted budget is the run's
// own and wins over the Project's current one.
func TestRunBudgetForAnUnbudgetedRun(t *testing.T) {
	p := testProject()
	p.Spec.Limits.Revise.MaxCostMicroUSD = 3000000
	run := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageRevise}}
	s := testSettings()
	if got := s.runBudgetFor(run, p); got != 0 {
		t.Errorf("budget under the token unit = %d, want none", got)
	}
	s.CostBudgets = true
	if got := s.runBudgetFor(run, p); got != 3000000 {
		t.Errorf("unbudgeted run's budget = %d, want the Project's 3000000", got)
	}
	run.Spec.Grant.BudgetMicroUSD = 2000000
	if got := s.runBudgetFor(run, p); got != 2000000 {
		t.Errorf("budgeted run's budget = %d, want its own 2000000", got)
	}
	p.Spec.Limits.Revise.MaxCostMicroUSD = 0
	run.Spec.Grant.BudgetMicroUSD = 0
	if got := s.runBudgetFor(run, p); got != v1alpha1.DefaultMaxCostMicroUSD {
		t.Errorf("budget with no stage or intent limit = %d, want the default ceiling", got)
	}
}

// TestStageTimeoutLowersTheGrant: a Project's stage timeout shortens the
// run's wall clock, in the grant and in the env agent-runner reads, and a
// longer one is clamped to the controller's; the Job's deadline follows the
// granted timeout.
func TestStageTimeoutLowersTheGrant(t *testing.T) {
	p := testProject()
	p.Spec.Limits.Plan.Timeout = &metav1.Duration{Duration: 10 * time.Minute}
	p.Spec.Limits.Build.Timeout = &metav1.Duration{Duration: 4 * time.Hour}
	s := testSettings()
	if g := s.grant(p, v1alpha1.IntentStagePlan); g.TimeoutMilliseconds != (10 * time.Minute).Milliseconds() {
		t.Errorf("plan grant timeout = %dms, want the Project's 10m", g.TimeoutMilliseconds)
	}
	if g := s.grant(p, v1alpha1.IntentStageBuild); g.TimeoutMilliseconds != time.Hour.Milliseconds() {
		t.Errorf("build grant timeout = %dms, want the controller's 1h", g.TimeoutMilliseconds)
	}
	if g := s.grant(p, v1alpha1.IntentStageRevise); g.TimeoutMilliseconds != (45 * time.Minute).Milliseconds() {
		t.Errorf("revise grant timeout = %dms, want the controller's 45m", g.TimeoutMilliseconds)
	}

	e := newEnv(t, p)
	e.jobDeadlines(0, 4*time.Hour+30*time.Minute)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	spec, env := e.launchOf(t, "plan")
	if env["PATCHY_INVESTIGATE_TIMEOUT"] != "10m0s" {
		t.Errorf("plan env timeout = %q, want 10m0s", env["PATCHY_INVESTIGATE_TIMEOUT"])
	}
	if spec.Deadline != 25*time.Minute {
		t.Errorf("plan Job deadline = %s, want the timeout plus 15m", spec.Deadline)
	}
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	spec, env = e.launchOf(t, "build")
	if env["PATCHY_REMEDIATE_TIMEOUT"] != "1h0m0s" {
		t.Errorf("build env timeout = %q, want 1h0m0s", env["PATCHY_REMEDIATE_TIMEOUT"])
	}
	if spec.Deadline != 75*time.Minute {
		t.Errorf("build Job deadline = %s, want the timeout plus 15m", spec.Deadline)
	}
}

// TestJobDeadline is the derivation's table: the stage timeout plus 15m,
// capped by the ceiling, never under the floor.
func TestJobDeadline(t *testing.T) {
	tests := []struct {
		name                    string
		floor, ceiling, timeout time.Duration
		want                    time.Duration
	}{
		{"defaults keep the floor", 90 * time.Minute, 270 * time.Minute, time.Hour, 90 * time.Minute},
		{"a long stage stretches it", 90 * time.Minute, 270 * time.Minute, 90 * time.Minute, 105 * time.Minute},
		{"the ceiling caps it", 90 * time.Minute, 4 * time.Hour, 4 * time.Hour, 4 * time.Hour},
		{"a ceiling under the floor stretches nothing", 2 * time.Hour, time.Hour, 3 * time.Hour, 2 * time.Hour},
		{"no floor", 0, 270 * time.Minute, 20 * time.Minute, 35 * time.Minute},
		{"no timeout keeps the floor", 90 * time.Minute, 270 * time.Minute, 0, 90 * time.Minute},
		{"nothing keeps the Config's", 0, 0, 0, 0},
	}
	for _, tt := range tests {
		s := Settings{JobDeadline: tt.floor, JobDeadlineMax: tt.ceiling}
		if got := s.jobDeadline(tt.timeout); got != tt.want {
			t.Errorf("%s: jobDeadline(%s) = %s, want %s", tt.name, tt.timeout, got, tt.want)
		}
	}
}

// TestJobDeadlineProperty: whatever the floor, ceiling and timeout, the
// deadline is at least the floor, at least the timeout plus 15m when the
// ceiling allows it, and never past both the floor and the ceiling, so a Job
// outlives its stage whenever the operator's limits let it.
func TestJobDeadlineProperty(t *testing.T) {
	prop := func(floorS, ceilingS, timeoutS uint16) bool {
		floor := time.Duration(floorS) * time.Second
		ceiling := time.Duration(ceilingS)*time.Second + time.Second
		timeout := time.Duration(timeoutS)*time.Second + time.Second
		got := Settings{JobDeadline: floor, JobDeadlineMax: ceiling}.jobDeadline(timeout)
		return got >= floor &&
			got >= min(timeout+jobDeadlineMargin, ceiling) &&
			got <= max(floor, ceiling) &&
			(got == floor || got <= timeout+jobDeadlineMargin)
	}
	if err := quick.Check(prop, quickConfig(11)); err != nil {
		t.Error(err)
	}
}

// TestRunBudgetOverHourlyLimit: under the cost unit with an hourly limit, a
// Project any of whose run budgets is more than half of it is not Ready,
// naming the stage; exactly half is Ready, and so is any budget under the
// token unit or with no hourly limit.
func TestRunBudgetOverHourlyLimit(t *testing.T) {
	ready := func(t *testing.T, p *v1alpha1.Project, cost bool, perHour int64) *metav1.Condition {
		t.Helper()
		e := newEnv(t, p)
		if cost {
			e.costBudgets(perHour)
		}
		e.reconcileProject()
		return meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
	}
	large := testProject()
	large.Spec.Limits.MaxCostMicroUSD = 100000000
	large.Spec.Limits.Build.MaxCostMicroUSD = 60000000
	large.Spec.Limits.Plan.MaxCostMicroUSD = 5000000
	large.Spec.Limits.Revise.MaxCostMicroUSD = 20000000

	c := ready(t, large.DeepCopy(), true, 100000000)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonRunBudgetOverHourlyLimit {
		t.Fatalf("Ready = %+v, want False with %s", c, ReasonRunBudgetOverHourlyLimit)
	}
	for _, want := range []string{"build run", "60000000", "100000000", "limits.build.maxCostMicroUSD"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q does not say %q", c.Message, want)
		}
	}
	for _, tt := range []struct {
		name    string
		cost    bool
		perHour int64
	}{
		{"exactly half", true, 120000000},
		{"no hourly limit", true, 0},
		{"the token unit", false, 0},
	} {
		if c := ready(t, large.DeepCopy(), tt.cost, tt.perHour); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("%s: Ready = %+v, want True", tt.name, c)
		}
	}
	// A stage that sets no budget runs on the per-intent ceiling, so that is
	// what is judged.
	plain := testProject()
	if c := ready(t, plain, true, 2*v1alpha1.DefaultMaxCostMicroUSD-1); c == nil ||
		c.Reason != ReasonRunBudgetOverHourlyLimit || !strings.Contains(c.Message, "plan run") {
		t.Errorf("Ready over the default ceiling = %+v, want the plan named", c)
	}
}
