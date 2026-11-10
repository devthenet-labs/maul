// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"slices"
	"strings"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// ReasonModelNotAllowed is the Project Ready reason, and the Intent
// ModelNotAllowed block's, for a Project whose spec.models names a model
// this controller does not admit for its harness (Settings.modelRefusal).
const ReasonModelNotAllowed = "ModelNotAllowed"

// ModelList is a comma-separated list of canonical model ids. It is a
// string, not a slice, so Settings stays comparable.
type ModelList string

// NewModelList is the list of ids, empty ones dropped.
func NewModelList(ids ...string) ModelList {
	return ModelList(strings.Join(slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == "" }), ","))
}

// IDs are the list's model ids, in order.
func (l ModelList) IDs() []string {
	if l == "" {
		return nil
	}
	return strings.Split(string(l), ",")
}

// Has reports whether id is on the list.
func (l ModelList) Has(id string) bool { return id != "" && slices.Contains(l.IDs(), id) }

// harness is the run harness a Project, or a run recording h, launches on:
// Codex for codex, Claude otherwise (also when unset).
func (s Settings) harness(h v1alpha1.ProjectHarness) RunHarness {
	if h.IsCodex() {
		return s.Codex
	}
	return s.Claude
}

// admittedModels are the models a Project on harness h may choose in
// spec.models: its stage flags' own (and, under the agent evaluator, the
// claude evaluator's and check reviewer's), then the --intent-model-allowlist
// entries that resolved to h at startup. None while h is off.
func (s Settings) admittedModels(h v1alpha1.ProjectHarness) []string {
	if s.harnessOff(h) {
		return nil
	}
	rh := s.harness(h)
	ids := []string{rh.PlanModel, rh.BuildModel}
	if !h.IsCodex() && s.Evaluator {
		ids = append(ids, s.EvaluateModel, s.CheckReviewModel)
	}
	var out []string
	for _, id := range append(ids, rh.Allowed.IDs()...) {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// modelAdmitted reports whether a run on harness h may launch on id.
func (s Settings) modelAdmitted(h v1alpha1.ProjectHarness, id string) bool {
	return slices.Contains(s.admittedModels(h), id)
}

// projectModelFields are spec.models' fields by name, in the Project's
// order.
func projectModelFields(m *v1alpha1.ProjectModels) [][2]string {
	if m == nil {
		return nil
	}
	return [][2]string{{"plan", m.Plan}, {"build", m.Build}, {"evaluate", m.Evaluate}, {"checkReview", m.CheckReview}}
}

// modelRefusal is why p's spec.models keeps it from being Ready, "" when
// every model it names (it may name none) is one this controller admits
// for its harness. Every field it sets is judged, whether or not its stage
// runs now (an evaluate model with the evaluator off), so a choice that
// could never run is reported when it is made, not when it is first used.
// A Project on a harness this controller runs no intent on is refused for
// that alone (HarnessNotEnabled), its runs waiting Pending, never held for
// its models.
func (s Settings) modelRefusal(p *v1alpha1.Project) string {
	if s.harnessOff(p.Spec.Harness) {
		return ""
	}
	harness := p.Spec.Harness
	if harness == "" {
		harness = v1alpha1.ProjectHarnessClaude
	}
	admitted := s.admittedModels(p.Spec.Harness)
	for _, f := range projectModelFields(p.Spec.Models) {
		if f[1] == "" || slices.Contains(admitted, f[1]) {
			continue
		}
		return fmt.Sprintf("spec.models.%s is %s, which intent-controller does not run a %s Project's intents on: "+
			"it admits %s (the stage flags' models, and those --intent-model-allowlist adds for this harness, chart "+
			"intentController.config.modelAllowlist); until spec.models names one of them, or none, no intent of "+
			"this project is discovered and no run of one already open is created", f[0], f[1], harness,
			strings.Join(admitted, ", "))
	}
	return ""
}

// chosenModel is the model p's spec.models chooses for a run of stage, ""
// when it chooses none, so the run records none and runs the stage flag as
// before (runModel). A revise run takes the build choice. An evaluate or
// checkreview run takes its own; a codex Project's, unset, takes the build
// choice, as a codex role run otherwise runs the codex build model.
func chosenModel(p *v1alpha1.Project, stage v1alpha1.IntentStage) string {
	m := p.Spec.Models
	if m == nil {
		return ""
	}
	var id string
	switch stage {
	case v1alpha1.IntentStagePlan:
		return m.Plan
	case v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise:
		return m.Build
	case v1alpha1.IntentStageEvaluate:
		id = m.Evaluate
	case v1alpha1.IntentStageCheckReview:
		id = m.CheckReview
	}
	if id == "" && p.Spec.Harness.IsCodex() {
		return m.Build
	}
	return id
}

// stageModel is the model a run of stage of p's intents runs: its Project's
// choice (chosenModel), else this controller's flag for the stage on p's
// harness (flagModel).
func (s Settings) stageModel(p *v1alpha1.Project, stage v1alpha1.IntentStage) string {
	if id := chosenModel(p, stage); id != "" {
		return id
	}
	return s.flagModel(p.Spec.Harness, stage)
}

// flagModel is the stage flag's model for a run of stage on harness h: the
// plan or build model of h (a revise run's is the build's); an evaluate or
// checkreview run's, the stage's own on claude and the build model on codex,
// which has no evaluator models of its own.
func (s Settings) flagModel(h v1alpha1.ProjectHarness, stage v1alpha1.IntentStage) string {
	rh := s.harness(h)
	switch {
	case stage == v1alpha1.IntentStagePlan:
		return rh.PlanModel
	case !roleStage(stage), h.IsCodex():
		return rh.BuildModel
	case stage == v1alpha1.IntentStageCheckReview:
		return s.CheckReviewModel
	}
	return s.EvaluateModel
}
