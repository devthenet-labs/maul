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

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
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
	e.gh.label(1, "maul:approved", approver)
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
	off.gh.label(1, "maul:approved", approver)
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
// granted timeout, over a floor the binary starts with (any positive one:
// the floor is not tied to the stage timeouts).
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
	e.jobDeadlines(30*time.Minute, 4*time.Hour+30*time.Minute)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	spec, env := e.launchOf(t, "plan")
	if env["MAUL_INVESTIGATE_TIMEOUT"] != "10m0s" {
		t.Errorf("plan env timeout = %q, want 10m0s", env["MAUL_INVESTIGATE_TIMEOUT"])
	}
	if spec.Deadline != 30*time.Minute {
		t.Errorf("plan Job deadline = %s, want the 30m floor over its 25m", spec.Deadline)
	}
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	spec, env = e.launchOf(t, "build")
	if env["MAUL_REMEDIATE_TIMEOUT"] != "1h0m0s" {
		t.Errorf("build env timeout = %q, want 1h0m0s", env["MAUL_REMEDIATE_TIMEOUT"])
	}
	if spec.Deadline != 75*time.Minute {
		t.Errorf("build Job deadline = %s, want the timeout plus 15m", spec.Deadline)
	}
}

// TestLongStageStretchesOnlyItsOwnJobs: under the shipped 90m floor, an
// operator who raises the build stage to 3h gets a 3h15m build Job while the
// plan Job keeps 90m. The floor is not raised to the longest stage, which
// would hand every short stage the long one's deadline.
func TestLongStageStretchesOnlyItsOwnJobs(t *testing.T) {
	e := newEnv(t, testProject())
	for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
		s.Build.Timeout = 3 * time.Hour
	}
	e.jobDeadlines(90*time.Minute, 4*time.Hour+30*time.Minute)
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if spec, _ := e.launchOf(t, "plan"); spec.Deadline != 90*time.Minute {
		t.Errorf("plan Job deadline = %s, want the 90m floor", spec.Deadline)
	}
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if spec, _ := e.launchOf(t, "build"); spec.Deadline != 3*time.Hour+15*time.Minute {
		t.Errorf("build Job deadline = %s, want its 3h timeout plus 15m", spec.Deadline)
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
			got >= min(timeout+JobDeadlineMargin, ceiling) &&
			got <= max(floor, ceiling) &&
			(got == floor || got <= timeout+JobDeadlineMargin)
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

// roleSettings are testSettings with the agent evaluator on for a Project
// that verifies (evaluatingProject), counting cost.
func roleSettings() Settings {
	s := testSettings()
	s.Verification, s.Previews, s.Evaluator, s.CostBudgets = true, true, true, true
	s.Evaluate = StageCeiling{MaxTurns: DefaultEvaluateMaxTurns, TokenBudget: DefaultEvaluateTokenBudget,
		Timeout: DefaultEvaluateTimeout}
	s.CheckReview = StageCeiling{MaxTurns: DefaultCheckReviewMaxTurns, TokenBudget: DefaultCheckReviewTokenBudget,
		Timeout: DefaultCheckReviewTimeout}
	return s
}

// evaluatingProject is a Project the evaluator verifies under roleSettings.
func evaluatingProject() *v1alpha1.Project {
	p := testProject()
	p.Spec.Preview = &appPreview
	p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	return p
}

// TestRoleStagesHonourTheirLimits (S1-29): an evaluate or checkreview run
// is granted its stage's limits like any other stage (turns, tokens and the
// wall clock lowered, never raised), and its cost budget is held to the
// evaluation ceiling it is spent against, not the build ceiling: unset, it
// is that ceiling; set, never above it.
func TestRoleStagesHonourTheirLimits(t *testing.T) {
	s := roleSettings()
	p := evaluatingProject()
	p.Spec.Limits.MaxCostMicroUSD = 80000000
	p.Spec.Limits.MaxEvaluateCostMicroUSD = new(int64(4000000))
	p.Spec.Limits.Evaluate = v1alpha1.StageLimits{MaxTurns: 30, TokenBudget: 1000,
		Timeout: &metav1.Duration{Duration: 10 * time.Minute}}
	p.Spec.Limits.CheckReview = v1alpha1.StageLimits{MaxTurns: 9, MaxCostMicroUSD: 1000000,
		Timeout: &metav1.Duration{Duration: 3 * time.Hour}}

	g := s.grant(p, v1alpha1.IntentStageEvaluate)
	if g.MaxTurns != 30 || g.TokenBudget != 1000 || g.TimeoutMilliseconds != (10*time.Minute).Milliseconds() {
		t.Errorf("evaluate grant = %+v, want the Project's 30 turns, 1000 tokens and 10m", g)
	}
	if g.BudgetMicroUSD != 4000000 {
		t.Errorf("evaluate budget = %d, want the evaluation ceiling 4000000, not the build ceiling", g.BudgetMicroUSD)
	}
	g = s.grant(p, v1alpha1.IntentStageCheckReview)
	if g.MaxTurns != DefaultCheckReviewMaxTurns || g.TimeoutMilliseconds != DefaultCheckReviewTimeout.Milliseconds() {
		t.Errorf("checkreview grant = %+v, want the controller's turns and timeout (the Project's are higher)", g)
	}
	if g.BudgetMicroUSD != 1000000 {
		t.Errorf("checkreview budget = %d, want its stage's 1000000", g.BudgetMicroUSD)
	}
	s.EvaluationCostMax = 2000000
	if g := s.grant(p, v1alpha1.IntentStageEvaluate); g.BudgetMicroUSD != 2000000 {
		t.Errorf("evaluate budget = %d, want --intent-evaluation-cost-max 2000000", g.BudgetMicroUSD)
	}
	run := &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: v1alpha1.IntentStageEvaluate}}
	if got := s.runBudgetFor(run, p); got != 2000000 {
		t.Errorf("an unbudgeted evaluate run's budget = %d, want the evaluation ceiling 2000000", got)
	}
}

// TestRoleBudgetOverHourlyLimit (S1-29): RunBudgetOverHourlyLimit counts
// the evaluate and checkreview runs of a Project the evaluator verifies,
// naming the stage and the field that lowers it, and not those of one it
// does not.
func TestRoleBudgetOverHourlyLimit(t *testing.T) {
	for _, tt := range []struct {
		stage v1alpha1.IntentStage
		field string
		set   func(*v1alpha1.ProjectLimits)
	}{
		{v1alpha1.IntentStageEvaluate, "limits.evaluate.maxCostMicroUSD",
			func(l *v1alpha1.ProjectLimits) { l.Evaluate.MaxCostMicroUSD = 9000000 }},
		{v1alpha1.IntentStageCheckReview, "limits.checkReview.maxCostMicroUSD",
			func(l *v1alpha1.ProjectLimits) { l.CheckReview.MaxCostMicroUSD = 9000000 }},
	} {
		t.Run(string(tt.stage), func(t *testing.T) {
			s := roleSettings()
			s.BrokerCostPerHour = 16000000
			p := evaluatingProject()
			p.Spec.Limits.MaxCostMicroUSD = 10000000
			p.Spec.Limits.MaxEvaluateCostMicroUSD = new(int64(9000000))
			for _, st := range []*v1alpha1.StageLimits{&p.Spec.Limits.Plan, &p.Spec.Limits.Build,
				&p.Spec.Limits.Revise, &p.Spec.Limits.Evaluate, &p.Spec.Limits.CheckReview} {
				st.MaxCostMicroUSD = 1000000
			}
			tt.set(&p.Spec.Limits)
			reason, msg := s.refusal(p)
			if reason != ReasonRunBudgetOverHourlyLimit {
				t.Fatalf("refusal = %q, want %s", reason, ReasonRunBudgetOverHourlyLimit)
			}
			for _, want := range []string{string(tt.stage) + " run", "9000000", tt.field,
				"limits.maxEvaluateCostMicroUSD"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not say %q", msg, want)
				}
			}
			s.Evaluator = false
			if reason, _ := s.refusal(p); reason != "" {
				t.Errorf("refusal without the evaluator = %q, want none", reason)
			}
		})
	}
}

// TestRoleBudgetUnderRequestReserve (S1 bug 6): under the cost unit the
// egress broker reserves each request's whole output bound at the model's
// output price, so an evaluate or checkreview run budget smaller than two
// such reservations (at the stage's output bound, provider.*MaxOutputTokens,
// on the stage's model) would starve the run; the Project is not Ready,
// naming the stage, its budget, the least budget it needs and the field
// that raises it. The docs' own example budgets are Ready, and nothing is
// judged under the token unit, without the evaluator or on codex.
func TestRoleBudgetUnderRequestReserve(t *testing.T) {
	settings := func() Settings {
		s := roleSettings()
		s.EvaluateModel, s.CheckReviewModel = "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-5"
		return s
	}
	project := func(evaluate, checkReview int64) *v1alpha1.Project {
		p := evaluatingProject()
		p.Spec.Limits.Evaluate.MaxCostMicroUSD = evaluate
		p.Spec.Limits.CheckReview.MaxCostMicroUSD = checkReview
		return p
	}
	// claude-sonnet-5 output is $15 per million tokens: 8192 tokens reserve
	// 122880 micro-USD, 32000 reserve 480000.
	for _, tt := range []struct {
		name                string
		evaluate, review    int64
		stage, field, least string
		budget              string
	}{
		{"checkreview", 1500000, 200000, "checkreview run", "limits.checkReview.maxCostMicroUSD", "245760", "200000"},
		{"evaluate", 900000, 300000, "evaluate run", "limits.evaluate.maxCostMicroUSD", "960000", "900000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reason, msg := settings().refusal(project(tt.evaluate, tt.review))
			if reason != ReasonRunBudgetUnderRequestReserve {
				t.Fatalf("refusal = %q (%s), want %s", reason, msg, ReasonRunBudgetUnderRequestReserve)
			}
			for _, want := range []string{tt.stage, tt.budget, tt.least, tt.field, "claude-sonnet-5"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not say %q", msg, want)
				}
			}
		})
	}

	// docs/intents/evaluator.md's example limits, and the defaults.
	for name, p := range map[string]*v1alpha1.Project{
		"the documented example": project(1500000, 300000),
		"the defaults":           project(0, 0),
		"exactly two reserves":   project(960000, 245760),
	} {
		if reason, msg := settings().refusal(p); reason != "" {
			t.Errorf("%s: refusal = %q (%s), want none", name, reason, msg)
		}
	}
	tooSmall := project(1500000, 100000)
	for name, s := range map[string]func(*Settings){
		"the token unit":    func(s *Settings) { s.CostBudgets = false },
		"no evaluator":      func(s *Settings) { s.Evaluator = false },
		"an unpriced model": func(s *Settings) { s.CheckReviewModel = "" },
	} {
		st := settings()
		s(&st)
		if reason, _ := st.refusal(tooSmall); reason != "" {
			t.Errorf("%s: refusal = %q, want none", name, reason)
		}
	}
	codex := tooSmall.DeepCopy()
	codex.Spec.Harness = v1alpha1.ProjectHarnessCodex
	st := settings()
	st.Codex = RunHarness{ID: "codex", PlanModel: "openai/gpt-5.3-codex", BuildModel: "openai/gpt-5.3-codex"}
	st.EvaluateCodex = true
	if reason, _ := st.refusal(codex); reason != "" {
		t.Errorf("codex: refusal = %q, want none (codex runs carry no output bound of maul's)", reason)
	}

	// Through the reconciler: Ready is False with the reason.
	e := newEnv(t, tooSmall.DeepCopy())
	e.project.Settings = settings()
	e.reconcileProject()
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonRunBudgetUnderRequestReserve {
		t.Fatalf("Ready = %+v, want False with %s", c, ReasonRunBudgetUnderRequestReserve)
	}
}
