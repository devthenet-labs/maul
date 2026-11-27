// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"math/rand"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/projectsettings"
)

// TestProjectSettingsRunBudgetIsStageBudget pins the one rule the project
// settings page restates. The page refuses a save that would take a Project
// not Ready under the broker's cost unit (a run budget over half the hourly
// limit, or under what its model's requests reserve), and to do that it
// reckons each stage's run budget from the spec and what the controller
// publishes. projectsettings.RunBudget must therefore be this controller's
// own Settings.stageBudget for every stage, whatever the Project's limits
// and whatever evaluation ceiling the controller runs with; the published
// ceiling is Settings.EvaluationCostMax (status.admits.maxEvaluateCostMicroUSD).
// Seeded, so the gate is deterministic.
func TestProjectSettingsRunBudgetIsStageBudget(t *testing.T) {
	stages := map[string]v1alpha1.IntentStage{
		"plan": v1alpha1.IntentStagePlan, "build": v1alpha1.IntentStageBuild, "revise": v1alpha1.IntentStageRevise,
		"evaluate": v1alpha1.IntentStageEvaluate,
	}
	if got := projectsettings.StageNames(); len(got) != len(stages) {
		t.Fatalf("projectsettings has stages %v, this pin knows %d", got, len(stages))
	}
	rng := rand.New(rand.NewSource(0x5e771465))
	money := func() int64 {
		if rng.Intn(3) == 0 {
			return 0
		}
		return int64(1 + rng.Intn(30_000_000))
	}
	var own, ceiling int
	for range 5000 {
		p := &v1alpha1.Project{}
		l := &p.Spec.Limits
		l.MaxCostMicroUSD = money()
		if n := money(); n > 0 {
			l.MaxEvaluateCostMicroUSD = &n
		}
		for _, st := range []*v1alpha1.StageLimits{&l.Plan, &l.Build, &l.Revise, &l.Evaluate} {
			st.MaxCostMicroUSD = money()
		}
		s := Settings{EvaluationCostMax: money()}
		admits := &v1alpha1.ProjectAdmits{MaxEvaluateCostMicroUSD: s.EvaluationCostMax}
		for _, name := range projectsettings.StageNames() {
			stage, ok := stages[name]
			if !ok {
				t.Fatalf("projectsettings stage %q is none this controller runs", name)
			}
			want := s.stageBudget(p, stage)
			if got := projectsettings.RunBudget(&p.Spec, admits, name); got != want {
				t.Fatalf("%s: RunBudget = %d, stageBudget = %d (limits %+v, evaluation ceiling %d)", name, got, want,
					*l, s.EvaluationCostMax)
			}
			if want == stageLimits(p, stage).MaxCostMicroUSD {
				own++
			} else {
				ceiling++
			}
		}
	}
	if own < 1000 || ceiling < 1000 {
		t.Fatalf("%d budgets were the stage's own and %d its ceiling: the pin exercised too little", own, ceiling)
	}
}
