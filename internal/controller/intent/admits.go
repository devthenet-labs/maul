// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/resourceclass"
)

// admits is what s admits for Project p, published as status.admits so the
// status page's project settings, and the admission policy beside them,
// judge a change by the controller's own rules rather than a copy of them.
// Every figure comes from the method that judges runs (a seeded test pins
// each): the models modelRefusal admits, the stage flags' models, the
// ceilings grant clamps to, the hourly bound budgetOverHourly holds a run
// budget under, the floors budgetUnderReserve holds an evaluator stage's
// budget over, the delegation grant records for a Project that opts in,
// and the operator's resource classes. It depends on the spec
// and the settings only, so an unchanged Project is never written again,
// and what does not fit the status schema's bounds is dropped, never
// failed: a refused status write would lose Ready with it.
func (s Settings) admits(p *v1alpha1.Project, classes resourceclass.Set) *v1alpha1.ProjectAdmits {
	h := p.Spec.Harness
	evaluator := s.evaluatorRuns(h)
	a := &v1alpha1.ProjectAdmits{
		Models: boundedModels(s.admittedModels(h)),
		DefaultModels: v1alpha1.ProjectStageModels{
			Plan:  boundedModel(s.flagModel(h, v1alpha1.IntentStagePlan)),
			Build: boundedModel(s.flagModel(h, v1alpha1.IntentStageBuild)),
		},
		Stages: v1alpha1.ProjectStageCeilings{
			Plan:   publishCeiling(s.Plan),
			Build:  publishCeiling(s.Build),
			Revise: publishCeiling(s.Revise),
		},
		MaxEvaluateCostMicroUSD: max(s.EvaluationCostMax, 0),
		MaxToolCalls:            max(s.MaxToolCalls, 0),
		CostBudgets:             s.CostBudgets,
		Evaluator:               s.Evaluator,
		Subagents:               s.subagentsAllowed(h),
		Workflows:               s.workflowsAllowed(h),
		ResourceClasses:         publishClasses(classes),
	}
	if evaluator {
		a.DefaultModels.Evaluate = boundedModel(s.flagModel(h, v1alpha1.IntentStageEvaluate))
		a.DefaultModels.CheckReview = boundedModel(s.flagModel(h, v1alpha1.IntentStageCheckReview))
		a.Stages.Evaluate, a.Stages.CheckReview = publishCeiling(s.Evaluate), publishCeiling(s.CheckReview)
	}
	// budgetOverHourly refuses 2*b > hourly, so the largest budget it lets
	// through is exactly half the limit, rounded down.
	if s.CostBudgets && s.BrokerCostPerHour > 0 {
		a.MaxRunCostMicroUSD = s.BrokerCostPerHour / 2
	}
	// budgetUnderReserve judges only a claude Project's evaluator stages,
	// and only under the cost unit.
	if s.CostBudgets && s.Evaluator && !h.IsCodex() {
		for _, id := range a.Models {
			eval, okE := outputReserve(id, roleOutputBounds[v1alpha1.IntentStageEvaluate])
			review, okR := outputReserve(id, roleOutputBounds[v1alpha1.IntentStageCheckReview])
			if !okE || !okR {
				continue
			}
			a.RunBudgetFloors = append(a.RunBudgetFloors, v1alpha1.ProjectRunBudgetFloor{Model: id,
				EvaluateMicroUSD: max(2*eval, 0), CheckReviewMicroUSD: max(2*review, 0)})
		}
	}
	return a
}

// evaluatorRuns reports whether s creates evaluate and checkreview runs for
// a Project on harness h: the agent evaluator is on, and for codex
// --intent-evaluate-codex is too (evaluatorUnverified otherwise).
func (s Settings) evaluatorRuns(h v1alpha1.ProjectHarness) bool {
	return s.Evaluator && (!h.IsCodex() || s.EvaluateCodex)
}

// publishCeiling is a stage ceiling in the status's spelling; a zero
// timeout is left unset.
func publishCeiling(c StageCeiling) v1alpha1.ProjectStageCeiling {
	out := v1alpha1.ProjectStageCeiling{MaxTurns: max(c.MaxTurns, 0), TokenBudget: max(c.TokenBudget, 0)}
	if c.Timeout > 0 {
		out.Timeout = &metav1.Duration{Duration: c.Timeout}
	}
	return out
}

// boundedModels are ids held to the status schema: at most
// v1alpha1.MaxAdmittedModels, each at most v1alpha1.MaxAdmittedModelLength
// characters (a longer one could never be chosen by a spec anyway).
func boundedModels(ids []string) []string {
	var out []string
	for _, id := range ids {
		if id == "" || len(id) > v1alpha1.MaxAdmittedModelLength || len(out) == v1alpha1.MaxAdmittedModels {
			continue
		}
		out = append(out, id)
	}
	return out
}

func boundedModel(id string) string {
	if len(id) > v1alpha1.MaxAdmittedModelLength {
		return ""
	}
	return id
}

// publishClasses are the operator's classes by name, sorted, with their
// sizes as Kubernetes writes them, at most v1alpha1.MaxAdmittedResourceClasses.
func publishClasses(classes resourceclass.Set) []v1alpha1.ProjectResourceClass {
	var out []v1alpha1.ProjectResourceClass
	for _, name := range classes.Names() {
		if name == "" || len(name) > v1alpha1.MaxResourceClassNameLength ||
			len(out) == v1alpha1.MaxAdmittedResourceClasses {
			continue
		}
		r, _ := classes.Lookup(name)
		cpuReq, memReq, cpuLim, memLim := r.Strings()
		out = append(out, v1alpha1.ProjectResourceClass{Name: name,
			Requests: v1alpha1.ProjectResourceQuantities{CPU: cpuReq, Memory: memReq},
			Limits:   v1alpha1.ProjectResourceQuantities{CPU: cpuLim, Memory: memLim}})
	}
	return out
}
