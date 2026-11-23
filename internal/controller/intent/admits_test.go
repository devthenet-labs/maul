// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/resourceclass"
)

// TestProjectAdmitsPublished: every pass publishes what the controller
// admits for the Project, the resource classes the operator defines among
// it, by name with their sizes as the operator wrote them.
func TestProjectAdmitsPublished(t *testing.T) {
	e := newEnv(t, testProject())
	e.classes(testClasses(t))
	e.reconcileProject()
	p := e.getProject()
	if p.Status.Admits == nil {
		t.Fatal("status.admits is not published")
	}
	want := e.project.Settings.withDefaults().admits(p, e.project.Classes)
	if !equality.Semantic.DeepEqual(p.Status.Admits, want) {
		t.Errorf("status.admits = %+v, want %+v", p.Status.Admits, want)
	}
	wantClasses := []v1alpha1.ProjectResourceClass{
		{Name: "large", Requests: v1alpha1.ProjectResourceQuantities{CPU: "4", Memory: "8Gi"},
			Limits: v1alpha1.ProjectResourceQuantities{Memory: "10Gi"}},
		{Name: "medium", Requests: v1alpha1.ProjectResourceQuantities{CPU: "2", Memory: "4Gi"},
			Limits: v1alpha1.ProjectResourceQuantities{CPU: "3", Memory: "5Gi"}},
	}
	if !slices.Equal(p.Status.Admits.ResourceClasses, wantClasses) {
		t.Errorf("resourceClasses = %+v, want %+v", p.Status.Admits.ResourceClasses, wantClasses)
	}
	wantModels := []string{"anthropic/claude-sonnet-5", "anthropic/claude-opus-5"}
	if !slices.Equal(p.Status.Admits.Models, wantModels) {
		t.Errorf("models = %v, want %v", p.Status.Admits.Models, wantModels)
	}
	if got := p.Status.Admits.Stages.Build; got.MaxTurns != 150 || got.TokenBudget != 800000 ||
		timeoutOf(got.Timeout) != time.Hour {
		t.Errorf("build ceiling = %+v, want the controller's", got)
	}

	// A second pass with nothing changed writes nothing.
	rv := p.ResourceVersion
	e.reconcileProject()
	if got := e.getProject().ResourceVersion; got != rv {
		t.Errorf("an unchanged pass wrote the status (resourceVersion %s to %s)", rv, got)
	}
}

// TestProjectAdmitsDelegation: status.admits says whether a Project's
// spec.delegation switches take effect: subagents while
// --intent-build-subagents is on and the Project is not on codex, workflows
// only beside them under --intent-build-workflows. The published pass
// carries them, so the settings page can lock a switch the controller does
// not allow.
func TestProjectAdmitsDelegation(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		harness              v1alpha1.ProjectHarness
		subagents, workflows bool // the flags
		want                 [2]bool
	}{
		{"both flags off", "", false, false, [2]bool{}},
		{"subagents on", "", true, false, [2]bool{true, false}},
		{"both on", v1alpha1.ProjectHarnessClaude, true, true, [2]bool{true, true}},
		{"workflows on alone, which serve refuses", "", false, true, [2]bool{}},
		{"both on, on codex", v1alpha1.ProjectHarnessCodex, true, true, [2]bool{}},
	} {
		s := testSettings()
		s.BuildSubagents, s.BuildWorkflows = tt.subagents, tt.workflows
		p := testProject()
		p.Spec.Harness = tt.harness
		if a := s.admits(p, nil); [2]bool{a.Subagents, a.Workflows} != tt.want {
			t.Errorf("%s: admits subagents, workflows = %v, %v; want %v", tt.name, a.Subagents, a.Workflows, tt.want)
		}
	}
	e := newEnv(t, testProject())
	e.project.Settings.BuildSubagents, e.project.Settings.BuildWorkflows = true, true
	e.reconcileProject()
	if a := e.getProject().Status.Admits; a == nil || !a.Subagents || !a.Workflows {
		t.Errorf("published status.admits = %+v, want subagents and workflows", a)
	}
}

// TestProjectAdmitsIsWhatTheControllerAdmits pins status.admits to the
// Settings methods that judge a Project's runs, so the settings page and
// its admission policy, which judge a change by it, cannot drift from the
// controller: for claude and codex Projects, the evaluator on and off, the
// broker counting tokens or cost.
//
//   - models: exactly admittedModels, and a Project naming one is never
//     refused (modelRefusal) while one naming anything else is;
//   - default models: the stage flags' models (flagModel);
//   - stages: the ceilings grant clamps a Project's limits to, for every
//     stage the controller runs for the harness, and zero for one it does
//     not run;
//   - maxRunCostMicroUSD: the largest budget budgetOverHourly lets through;
//   - runBudgetFloors: the least budget budgetUnderReserve lets through,
//     per admitted model it judges;
//   - subagents and workflows: what grant records on a build and a revise
//     run of the Project opted in to both;
//   - the rest copied from Settings.
//
// Seeded, so the gate is deterministic.
func TestProjectAdmitsIsWhatTheControllerAdmits(t *testing.T) {
	g := admitsGen{rng: rand.New(rand.NewSource(0xad3175))}
	var floorsJudged, overJudged, codexOn, evaluated int
	var delegation [2][2]int // [subagents, workflows][admitted]
	for i := range 400 {
		s, p := g.settings(), g.project()
		h := p.Spec.Harness
		if h.IsCodex() && s.Codex.ID != "" {
			codexOn++
		}
		if s.evaluatorRuns(h) && !s.harnessOff(h) {
			evaluated++
		}
		a := s.admits(p, nil)
		ctx := fmt.Sprintf("case %d (harness %q, codex on %v, evaluator %v, evaluate codex %v)", i, h,
			s.Codex.ID != "", s.Evaluator, s.EvaluateCodex)
		checkModels(t, ctx, s, p, a)
		checkStages(t, ctx, g.rng, s, p, a)
		if a.MaxEvaluateCostMicroUSD != s.EvaluationCostMax || a.MaxToolCalls != s.MaxToolCalls ||
			a.CostBudgets != s.CostBudgets || a.Evaluator != s.Evaluator {
			t.Fatalf("%s: copied facts %+v differ from the settings", ctx, a)
		}
		if checkHourly(t, ctx, s, a) {
			overJudged++
		}
		floorsJudged += checkFloors(t, ctx, s, p, a)
		checkDelegation(t, ctx, s, p, a)
		delegation[0][b2i(a.Subagents)]++
		delegation[1][b2i(a.Workflows)]++
	}
	if floorsJudged < 100 || overJudged < 100 || codexOn < 50 || evaluated < 100 {
		t.Fatalf("floors judged %d, hourly bounds %d, codex on %d, evaluated %d: the pin exercised too little",
			floorsJudged, overJudged, codexOn, evaluated)
	}
	for i, n := range delegation {
		if n[0] < 50 || n[1] < 50 {
			t.Fatalf("delegation switch %d admitted %d times, refused %d: the pin exercised too little", i, n[1], n[0])
		}
	}
}

// checkDelegation holds a's subagents and workflows to what grant records
// on a build and a revise run of p opted in to both: the switch is
// admitted exactly where an opt-in takes effect.
func checkDelegation(t *testing.T, ctx string, s Settings, p *v1alpha1.Project, a *v1alpha1.ProjectAdmits) {
	t.Helper()
	optedIn := p.DeepCopy()
	optedIn.Spec.Delegation = &v1alpha1.ProjectDelegation{Subagents: true, Workflows: true}
	for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise} {
		g := s.grant(optedIn, stage)
		if a.Subagents != g.Subagents || a.Workflows != g.Workflows {
			t.Fatalf("%s: admits subagents %v, workflows %v; a %s run of a Project opted in records %v, %v", ctx,
				a.Subagents, a.Workflows, stage, g.Subagents, g.Workflows)
		}
	}
	if a.Workflows && !a.Subagents {
		t.Fatalf("%s: workflows admitted without subagents", ctx)
	}
}

// b2i is 1 for true.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The models the generated settings choose among: claude ones the registry
// prices, one it does not, and codex ones.
var (
	admitsClaudeIDs = []string{"anthropic/claude-haiku-4-5", "anthropic/claude-sonnet-5", "anthropic/claude-opus-5",
		"anthropic/claude-opus-5-5", "anthropic/claude-unpriced-9"}
	admitsCodexIDs = []string{"openai/gpt-5.3-codex", "openai/gpt-5.6-sol", "openai/gpt-6-astra"}
)

// admitsGen generates the settings and Projects the admits pin runs over.
type admitsGen struct{ rng *rand.Rand }

func (g admitsGen) pick(ids []string) string { return ids[g.rng.Intn(len(ids))] }

func (g admitsGen) ceiling() StageCeiling {
	return StageCeiling{MaxTurns: int32(1 + g.rng.Intn(300)), TokenBudget: int64(1 + g.rng.Intn(2_000_000)),
		Timeout: time.Duration(1+g.rng.Intn(120)) * time.Minute}
}

// settings are controller settings: claude always, codex or not, the
// evaluator on or off (and codex evaluated or not), the broker counting
// tokens or cost under an hourly limit or none, and the evaluation and
// tool-call ceilings set or not.
func (g admitsGen) settings() Settings {
	s := testSettings()
	s.Plan, s.Build, s.Revise = g.ceiling(), g.ceiling(), g.ceiling()
	s.Claude = RunHarness{ID: "claude", PlanModel: g.pick(admitsClaudeIDs), BuildModel: g.pick(admitsClaudeIDs),
		Allowed: NewModelList(g.pick(admitsClaudeIDs), g.pick(admitsClaudeIDs))}
	if g.rng.Intn(2) == 0 {
		s.Codex = RunHarness{ID: "codex", PlanModel: g.pick(admitsCodexIDs), BuildModel: g.pick(admitsCodexIDs),
			Allowed: NewModelList(g.pick(admitsCodexIDs))}
	}
	s.Evaluator = g.rng.Intn(2) == 0
	s.Verification, s.Previews = s.Evaluator, true
	s.EvaluateCodex = g.rng.Intn(2) == 0
	if s.Evaluator {
		s.Evaluate = g.ceiling()
		s.EvaluateModel = g.pick(admitsClaudeIDs)
	}
	s.CostBudgets = g.rng.Intn(3) > 0
	if g.rng.Intn(4) > 0 {
		s.BrokerCostPerHour = int64(1 + g.rng.Intn(60_000_000))
	}
	if g.rng.Intn(2) == 0 {
		s.EvaluationCostMax = int64(1 + g.rng.Intn(20_000_000))
	}
	if g.rng.Intn(2) == 0 {
		s.MaxToolCalls = int32(1 + g.rng.Intn(500))
	}
	// Workflows only beside subagents, as serve refuses them otherwise.
	s.BuildSubagents = g.rng.Intn(2) == 0
	s.BuildWorkflows = s.BuildSubagents && g.rng.Intn(2) == 0
	return s
}

// project is the test Project asking to be verified, on claude or codex.
func (g admitsGen) project() *v1alpha1.Project {
	p := testProject()
	p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	if g.rng.Intn(2) == 0 {
		p.Spec.Harness = v1alpha1.ProjectHarnessCodex
	}
	return p
}

// checkModels holds a's models to admittedModels and modelRefusal, and its
// default models to the stage flags'.
func checkModels(t *testing.T, ctx string, s Settings, p *v1alpha1.Project, a *v1alpha1.ProjectAdmits) {
	t.Helper()
	h := p.Spec.Harness
	if !slices.Equal(a.Models, s.admittedModels(h)) {
		t.Fatalf("%s: models = %v, admittedModels = %v", ctx, a.Models, s.admittedModels(h))
	}
	if !s.harnessOff(h) {
		for _, id := range append(slices.Clone(admitsClaudeIDs), admitsCodexIDs...) {
			q := p.DeepCopy()
			q.Spec.Models = &v1alpha1.ProjectModels{Plan: id}
			if refused := s.modelRefusal(q) != ""; refused == slices.Contains(a.Models, id) {
				t.Fatalf("%s: model %s published %v, refused %v", ctx, id, slices.Contains(a.Models, id), refused)
			}
		}
	}
	want := v1alpha1.ProjectStageModels{Plan: s.flagModel(h, v1alpha1.IntentStagePlan),
		Build: s.flagModel(h, v1alpha1.IntentStageBuild)}
	if s.Evaluator && (!h.IsCodex() || s.EvaluateCodex) {
		want.Evaluate = s.flagModel(h, v1alpha1.IntentStageEvaluate)
	}
	if a.DefaultModels != want {
		t.Fatalf("%s: defaultModels = %+v, want %+v", ctx, a.DefaultModels, want)
	}
}

// checkStages holds a's stage ceilings to grant: for a stage the
// controller runs for the harness, a run under random limits is granted
// each limit clamped to the published ceiling; for one it does not run the
// ceiling is zero.
func checkStages(t *testing.T, ctx string, rng *rand.Rand, s Settings, p *v1alpha1.Project,
	a *v1alpha1.ProjectAdmits) {
	t.Helper()
	runs := s.Evaluator && (!p.Spec.Harness.IsCodex() || s.EvaluateCodex)
	for _, stage := range []v1alpha1.IntentStage{v1alpha1.IntentStagePlan, v1alpha1.IntentStageBuild,
		v1alpha1.IntentStageRevise, v1alpha1.IntentStageEvaluate} {
		c := publishedCeiling(a, stage)
		if roleStage(stage) && !runs {
			if c != (v1alpha1.ProjectStageCeiling{}) {
				t.Fatalf("%s: %s ceiling %+v published for a stage that does not run", ctx, stage, c)
			}
			continue
		}
		q := p.DeepCopy()
		l := v1alpha1.StageLimits{MaxTurns: int32(rng.Intn(400)), TokenBudget: int64(rng.Intn(3_000_000))}
		if rng.Intn(2) == 0 {
			l.Timeout = &metav1.Duration{Duration: time.Duration(rng.Intn(180)) * time.Minute}
		}
		setStageLimits(q, stage, l)
		g := s.grant(q, stage)
		wantTimeout := clampLimit(int64(timeoutOf(l.Timeout)), int64(timeoutOf(c.Timeout)))
		if int64(g.MaxTurns) != clampLimit(int64(l.MaxTurns), int64(c.MaxTurns)) ||
			g.TokenBudget != clampLimit(l.TokenBudget, c.TokenBudget) ||
			g.TimeoutMilliseconds != time.Duration(wantTimeout).Milliseconds() {
			t.Fatalf("%s: %s granted %+v under limits %+v; the published ceiling %+v says otherwise",
				ctx, stage, g, l, c)
		}
	}
}

// checkHourly holds maxRunCostMicroUSD to budgetOverHourly: a plan budget
// is refused exactly when it is over the published bound, and with no
// hourly bound none is published. It reports whether a bound was judged.
func checkHourly(t *testing.T, ctx string, s Settings, a *v1alpha1.ProjectAdmits) bool {
	t.Helper()
	if !s.CostBudgets || s.BrokerCostPerHour <= 0 {
		if a.MaxRunCostMicroUSD != 0 {
			t.Fatalf("%s: maxRunCostMicroUSD = %d with no hourly bound", ctx, a.MaxRunCostMicroUSD)
		}
		return false
	}
	for _, b := range []int64{a.MaxRunCostMicroUSD, a.MaxRunCostMicroUSD + 1} {
		if b == 0 {
			continue // zero is no stage budget at all
		}
		q := testProject()
		q.Spec.Limits.MaxCostMicroUSD = 1_000_000_000
		q.Spec.Limits.Build.MaxCostMicroUSD, q.Spec.Limits.Revise.MaxCostMicroUSD = 1, 1
		q.Spec.Limits.Plan.MaxCostMicroUSD = b
		if stage, _ := s.budgetOverHourly(q); (stage != "") != (b > a.MaxRunCostMicroUSD) {
			t.Fatalf("%s: a plan budget of %d against maxRunCostMicroUSD %d: budgetOverHourly says %q",
				ctx, b, a.MaxRunCostMicroUSD, stage)
		}
	}
	return true
}

// checkFloors holds a's runBudgetFloors to budgetUnderReserve: one entry per
// admitted model it can price, while it judges the Project at all, and for
// each a budget of the floor passes and one under it is refused, for the
// evaluate stage. It returns the floors judged.
func checkFloors(t *testing.T, ctx string, s Settings, p *v1alpha1.Project, a *v1alpha1.ProjectAdmits) int {
	t.Helper()
	if !s.CostBudgets || !s.Evaluator || p.Spec.Harness.IsCodex() {
		if len(a.RunBudgetFloors) != 0 {
			t.Fatalf("%s: runBudgetFloors %+v published where no budget is judged", ctx, a.RunBudgetFloors)
		}
		return 0
	}
	var priced []string
	for _, id := range a.Models {
		if _, ok := outputReserve(id, 1); ok {
			priced = append(priced, id)
		}
	}
	var got []string
	for _, f := range a.RunBudgetFloors {
		got = append(got, f.Model)
	}
	if !slices.Equal(got, priced) {
		t.Fatalf("%s: runBudgetFloors are for %v, want every admitted model priced: %v", ctx, got, priced)
	}
	s.EvaluationCostMax = 0
	judged := 0
	for _, f := range a.RunBudgetFloors {
		stage, floor := v1alpha1.IntentStageEvaluate, f.EvaluateMicroUSD
		for _, b := range []int64{floor - 1, floor} {
			q := p.DeepCopy()
			q.Spec.Models = &v1alpha1.ProjectModels{Evaluate: f.Model}
			huge := int64(1_000_000_000)
			q.Spec.Limits.MaxEvaluateCostMicroUSD = &huge
			setStageLimits(q, stage, v1alpha1.StageLimits{MaxCostMicroUSD: b})
			u := s.budgetUnderReserve(q)
			if refused := u.stage == stage; refused != (b < floor) {
				t.Fatalf("%s: a %s budget of %d on %s against the floor %d: budgetUnderReserve says %+v",
					ctx, stage, b, f.Model, floor, u)
			}
		}
		judged++
	}
	return judged
}

// TestProjectAdmitsHoldsTheBounds: an allowlist past the status schema's
// bounds is published cut to them, never refused: at most 64 models, none
// over 256 characters, and a floor for each published model only.
func TestProjectAdmitsHoldsTheBounds(t *testing.T) {
	s := testSettings()
	ids := make([]string, 0, 81)
	ids = append(ids, strings.Repeat("x", v1alpha1.MaxAdmittedModelLength+1))
	for i := range 80 {
		ids = append(ids, fmt.Sprintf("anthropic/claude-made-up-%d", i))
	}
	s.Claude.Allowed = NewModelList(ids...)
	s.CostBudgets, s.Evaluator = true, true
	s.EvaluateModel = "anthropic/claude-opus-5"
	classes := resourceclass.Set{}
	for i := range v1alpha1.MaxAdmittedResourceClasses {
		classes[fmt.Sprintf("c%02d", i)] = resourceclass.Resources{}
	}
	a := s.admits(testProject(), classes)
	if len(a.Models) != v1alpha1.MaxAdmittedModels {
		t.Errorf("%d models published, want the bound %d", len(a.Models), v1alpha1.MaxAdmittedModels)
	}
	for _, id := range a.Models {
		if len(id) > v1alpha1.MaxAdmittedModelLength {
			t.Errorf("model of %d characters published", len(id))
		}
	}
	if len(a.RunBudgetFloors) > v1alpha1.MaxAdmittedModels {
		t.Errorf("%d floors published", len(a.RunBudgetFloors))
	}
	if len(a.ResourceClasses) != v1alpha1.MaxAdmittedResourceClasses {
		t.Errorf("%d classes published, want %d", len(a.ResourceClasses), v1alpha1.MaxAdmittedResourceClasses)
	}
}

func publishedCeiling(a *v1alpha1.ProjectAdmits, stage v1alpha1.IntentStage) v1alpha1.ProjectStageCeiling {
	switch stage {
	case v1alpha1.IntentStagePlan:
		return a.Stages.Plan
	case v1alpha1.IntentStageBuild:
		return a.Stages.Build
	case v1alpha1.IntentStageRevise:
		return a.Stages.Revise
	}
	return a.Stages.Evaluate
}

func setStageLimits(p *v1alpha1.Project, stage v1alpha1.IntentStage, l v1alpha1.StageLimits) {
	switch stage {
	case v1alpha1.IntentStagePlan:
		p.Spec.Limits.Plan = l
	case v1alpha1.IntentStageBuild:
		p.Spec.Limits.Build = l
	case v1alpha1.IntentStageRevise:
		p.Spec.Limits.Revise = l
	case v1alpha1.IntentStageEvaluate:
		p.Spec.Limits.Evaluate = l
	}
}

// clampLimit is a limit under a published ceiling, as the page reads it: a
// positive limit below the ceiling, else the ceiling.
func clampLimit(limit, ceiling int64) int64 {
	if limit > 0 && limit < ceiling {
		return limit
	}
	return ceiling
}

func timeoutOf(d *metav1.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return d.Duration
}
