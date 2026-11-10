// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// modelSettings are testSettings with the evaluator's models and an
// allowlist on each harness, codex on.
func modelSettings() Settings {
	s := testSettings()
	s.Evaluator = true
	s.EvaluateModel, s.CheckReviewModel = "anthropic/claude-sonnet-5", "anthropic/claude-haiku-4-5"
	s.Claude.Allowed = NewModelList("anthropic/claude-fable-5")
	s.Codex = RunHarness{ID: "codex", PlanModel: "openai/gpt-5.3-codex", BuildModel: "openai/gpt-5.6-sol",
		Allowed: NewModelList("openai/gpt-5.6-luna")}
	return s
}

// evaluatorOff turns the agent evaluator off as serve does without
// --intent-evaluator: no evaluator models either.
func evaluatorOff(s *Settings) {
	s.Evaluator, s.EvaluateModel, s.CheckReviewModel = false, "", ""
}

// TestStageModel: a stage the Project chooses no model for runs the
// install's flag, exactly as before; a chosen one runs the choice. A revise
// run takes the build choice; a codex Project's role runs take its build
// model (chosen or the flag) unless it chooses theirs.
func TestStageModel(t *testing.T) {
	s := modelSettings()
	claude := func(m *v1alpha1.ProjectModels) *v1alpha1.Project {
		p := testProject()
		p.Spec.Models = m
		return p
	}
	codex := func(m *v1alpha1.ProjectModels) *v1alpha1.Project {
		p := claude(m)
		p.Spec.Harness = v1alpha1.ProjectHarnessCodex
		return p
	}
	plan, build, revise := v1alpha1.IntentStagePlan, v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise
	evaluate, review := v1alpha1.IntentStageEvaluate, v1alpha1.IntentStageCheckReview
	for _, tt := range []struct {
		name         string
		p            *v1alpha1.Project
		stage        v1alpha1.IntentStage
		want, chosen string
	}{
		{"claude plan, none chosen", claude(nil), plan, "anthropic/claude-sonnet-5", ""},
		{"claude build, none chosen", claude(nil), build, "anthropic/claude-opus-5", ""},
		{"claude revise, none chosen", claude(nil), revise, "anthropic/claude-opus-5", ""},
		{"claude evaluate, none chosen", claude(nil), evaluate, "anthropic/claude-sonnet-5", ""},
		{"claude checkreview, none chosen", claude(nil), review, "anthropic/claude-haiku-4-5", ""},
		{"an empty choice", claude(&v1alpha1.ProjectModels{}), build, "anthropic/claude-opus-5", ""},
		{"plan chosen", claude(&v1alpha1.ProjectModels{Plan: "anthropic/claude-fable-5"}), plan,
			"anthropic/claude-fable-5", "anthropic/claude-fable-5"},
		{"plan chosen leaves build", claude(&v1alpha1.ProjectModels{Plan: "anthropic/claude-fable-5"}), build,
			"anthropic/claude-opus-5", ""},
		{"build chosen runs revise", claude(&v1alpha1.ProjectModels{Build: "anthropic/claude-fable-5"}), revise,
			"anthropic/claude-fable-5", "anthropic/claude-fable-5"},
		{"claude build chosen leaves evaluate", claude(&v1alpha1.ProjectModels{Build: "anthropic/claude-fable-5"}),
			evaluate, "anthropic/claude-sonnet-5", ""},
		{"checkreview chosen", claude(&v1alpha1.ProjectModels{CheckReview: "anthropic/claude-fable-5"}), review,
			"anthropic/claude-fable-5", "anthropic/claude-fable-5"},
		{"codex plan, none chosen", codex(nil), plan, "openai/gpt-5.3-codex", ""},
		{"codex evaluate, none chosen", codex(nil), evaluate, "openai/gpt-5.6-sol", ""},
		{"codex evaluate takes the build choice", codex(&v1alpha1.ProjectModels{Build: "openai/gpt-5.6-luna"}),
			evaluate, "openai/gpt-5.6-luna", "openai/gpt-5.6-luna"},
		{"codex checkreview chosen", codex(&v1alpha1.ProjectModels{Build: "openai/gpt-5.6-luna",
			CheckReview: "openai/gpt-5.3-codex"}), review, "openai/gpt-5.3-codex", "openai/gpt-5.3-codex"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.stageModel(tt.p, tt.stage); got != tt.want {
				t.Errorf("stageModel = %q, want %q", got, tt.want)
			}
			if got := chosenModel(tt.p, tt.stage); got != tt.chosen {
				t.Errorf("chosenModel = %q, want %q", got, tt.chosen)
			}
		})
	}
}

// TestModelRefusal: a Project's chosen models must each be one this
// controller admits for its harness: a stage flag's model (the evaluator's
// only under it) or an allowlisted one that resolved to that harness.
func TestModelRefusal(t *testing.T) {
	for _, tt := range []struct {
		name     string
		set      func(*Settings)
		harness  v1alpha1.ProjectHarness
		models   *v1alpha1.ProjectModels
		wantPart string
	}{
		{name: "no models", models: nil},
		{name: "a stage flag's model", models: &v1alpha1.ProjectModels{Plan: "anthropic/claude-opus-5"}},
		{name: "an allowlisted model", models: &v1alpha1.ProjectModels{Build: "anthropic/claude-fable-5"}},
		{name: "the evaluator's model", models: &v1alpha1.ProjectModels{Plan: "anthropic/claude-haiku-4-5"}},
		{name: "the evaluator's model with the evaluator off", set: func(s *Settings) { s.Evaluator = false },
			models:   &v1alpha1.ProjectModels{Plan: "anthropic/claude-haiku-4-5"},
			wantPart: "spec.models.plan is anthropic/claude-haiku-4-5"},
		// With the evaluator off no evaluate or checkreview run is ever
		// created, so those two choices are not judged: turning the
		// evaluator off (or never on, the default) stops no Project.
		{name: "the evaluator's choices with the evaluator off", set: evaluatorOff,
			models: &v1alpha1.ProjectModels{Evaluate: "anthropic/claude-sonnet-4",
				CheckReview: "anthropic/claude-haiku-4-5"}},
		{name: "a plan choice with the evaluator off is still judged", set: evaluatorOff,
			models:   &v1alpha1.ProjectModels{Plan: "anthropic/claude-sonnet-4", CheckReview: "anthropic/claude-haiku-4-5"},
			wantPart: "spec.models.plan is anthropic/claude-sonnet-4"},
		{name: "a model nobody allowlisted", models: &v1alpha1.ProjectModels{Evaluate: "anthropic/claude-haiku-4-5",
			CheckReview: "anthropic/claude-sonnet-4"}, wantPart: "spec.models.checkReview is anthropic/claude-sonnet-4"},
		{name: "codex's model on claude", models: &v1alpha1.ProjectModels{Build: "openai/gpt-5.6-luna"},
			wantPart: "does not run a claude Project's intents on"},
		{name: "claude's model on codex", harness: v1alpha1.ProjectHarnessCodex,
			models: &v1alpha1.ProjectModels{Plan: "anthropic/claude-opus-5"}, wantPart: "a codex Project's"},
		{name: "codex's allowlisted model on codex", harness: v1alpha1.ProjectHarnessCodex,
			models: &v1alpha1.ProjectModels{Plan: "openai/gpt-5.6-luna", Build: "openai/gpt-5.3-codex"}},
		// HarnessNotEnabled refuses it: its runs wait Pending, unheld.
		{name: "codex models while --intent-codex is off", set: func(s *Settings) { s.Codex = RunHarness{} },
			harness: v1alpha1.ProjectHarnessCodex, models: &v1alpha1.ProjectModels{Plan: "openai/gpt-5.6-luna"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := modelSettings()
			if tt.set != nil {
				tt.set(&s)
			}
			p := testProject()
			p.Spec.Harness, p.Spec.Models = tt.harness, tt.models
			got := s.modelRefusal(p)
			if tt.wantPart == "" {
				if got != "" {
					t.Fatalf("modelRefusal = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantPart) || !strings.Contains(got, "--intent-model-allowlist") {
				t.Fatalf("modelRefusal = %q, want it to contain %q and name the allowlist", got, tt.wantPart)
			}
			if reason, _ := s.refusal(p); reason != ReasonModelNotAllowed {
				t.Errorf("refusal reason = %q, want %s", reason, ReasonModelNotAllowed)
			}
		})
	}
}

// withSettings gives all three reconcilers s, as the binary sets one
// Settings for them.
func (e *env) withSettings(s Settings) {
	e.project.Settings, e.intent.Settings, e.runs.Settings = s, s, s
}

// setModels writes the test Project's spec.models.
func (e *env) setModels(t *testing.T, m *v1alpha1.ProjectModels) {
	t.Helper()
	p := e.getProject()
	p.Spec.Models = m
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// TestProjectModelsRecordedOnRuns: a Project choosing its plan model plans
// on it and the plan run records it; its build, choosing none, runs the
// install's build flag and records none, exactly as a run did before
// Projects chose. A later choice applies to new runs only: the plan run's
// record is unchanged.
func TestProjectModelsRecordedOnRuns(t *testing.T) {
	p := testProject()
	p.Spec.Models = &v1alpha1.ProjectModels{Plan: "anthropic/claude-fable-5"}
	e := newEnv(t, p)
	e.withSettings(modelSettings())
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if plan := e.onlyLaunch(t, "plan"); plan.Model != "anthropic/claude-fable-5" {
		t.Errorf("plan launch model = %q, want the Project's anthropic/claude-fable-5", plan.Model)
	}

	e.setModels(t, &v1alpha1.ProjectModels{Plan: "anthropic/claude-opus-5"})
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if build := e.onlyLaunch(t, "build"); build.Model != "anthropic/claude-opus-5" {
		t.Errorf("build launch model = %q, want the build flag's anthropic/claude-opus-5", build.Model)
	}
	for _, run := range e.intentRuns(name) {
		want := ""
		if run.Spec.Stage == v1alpha1.IntentStagePlan {
			want = "anthropic/claude-fable-5"
		}
		if run.Spec.Model != want {
			t.Errorf("run %s records model %q, want %q", run.Name, run.Spec.Model, want)
		}
	}
}

// TestProjectModelNotAllowed: a Project naming a model this controller does
// not admit is not Ready (ModelNotAllowed), and an intent of it already
// open is held Blocked with no run created; naming an admitted one lifts the
// block, and the plan then runs on it.
func TestProjectModelNotAllowed(t *testing.T) {
	p := testProject()
	p.Spec.Models = &v1alpha1.ProjectModels{Plan: "anthropic/claude-sonnet-4"}
	e := newEnv(t, p)
	e.withSettings(modelSettings())
	e.reconcileProject()
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
	if c == nil || c.Status != "False" || c.Reason != ReasonModelNotAllowed ||
		!strings.Contains(c.Message, "anthropic/claude-sonnet-4") {
		t.Fatalf("Ready = %+v, want False with %s naming the model", c, ReasonModelNotAllowed)
	}

	name := e.newIntent(approver)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionModelNotAllowed) {
		t.Fatalf("conditions = %+v, want ModelNotAllowed True", in.Status.Conditions)
	}
	for range 3 {
		e.mustIntent(name)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	if runs := e.intentRuns(name); len(runs) != 0 || len(e.jobs.launched()) != 0 {
		t.Fatalf("%d runs, %d launches while the model is not admitted; want none", len(runs), len(e.jobs.launched()))
	}

	e.setModels(t, &v1alpha1.ProjectModels{Plan: "anthropic/claude-fable-5"})
	in = e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionModelNotAllowed); c == nil ||
		c.Status != "False" {
		t.Errorf("ModelNotAllowed = %+v once the model is admitted, want False", c)
	}
	if plan := e.onlyLaunch(t, "plan"); plan.Model != "anthropic/claude-fable-5" {
		t.Errorf("plan launch model = %q, want anthropic/claude-fable-5", plan.Model)
	}
}

// TestRecordedModelNoLongerAdmitted: a run recording a model this
// controller no longer admits (the allowlist changed since it was created)
// is not launched: it ends launch_refused, naming the model.
func TestRecordedModelNoLongerAdmitted(t *testing.T) {
	r := &RunReconciler{Settings: testSettings()}
	run := &v1alpha1.IntentRun{}
	run.Spec.Stage, run.Spec.Model = v1alpha1.IntentStagePlan, "anthropic/claude-fable-5"
	var spec jobs.Spec
	_, refusal := r.stageSpec(run, &spec, &corev1.ConfigMap{}, testProject(), nil)
	if refusal == nil || refusal.outcome != OutcomeLaunchRefused || !strings.Contains(refusal.detail,
		"anthropic/claude-fable-5") || spec.Model != "" {
		t.Fatalf("refusal = %+v, spec model %q; want launch_refused naming the model, none set", refusal, spec.Model)
	}

	r.Settings.Claude.Allowed = NewModelList("anthropic/claude-fable-5")
	if got, refusal := r.runModel(run); refusal != nil || got != "anthropic/claude-fable-5" {
		t.Fatalf("runModel = %q, %+v once admitted; want the recorded model", got, refusal)
	}
	run.Spec.Model = ""
	if got, _ := r.runModel(run); got != "anthropic/claude-sonnet-5" {
		t.Errorf("runModel of a run recording none = %q, want the plan flag's", got)
	}
}

// TestProjectModelRefusedAtApproval: a Project whose models stop being
// admitted while its intent awaits approval holds the intent at the
// approval: no build run is created on a model that could never launch.
// Naming an admitted model lifts the hold, and the build runs on it.
func TestProjectModelRefusedAtApproval(t *testing.T) {
	e := newEnv(t, testProject())
	e.withSettings(modelSettings())
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	launched := len(e.jobs.launched())

	e.setModels(t, &v1alpha1.ProjectModels{Build: "anthropic/claude-sonnet-4"})
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionModelNotAllowed) {
		t.Fatalf("conditions = %+v, want ModelNotAllowed True", in.Status.Conditions)
	}
	for range 3 {
		e.mustIntent(name)
		e.runRuns()
		e.clock.Advance(time.Minute)
	}
	for _, run := range e.intentRuns(name) {
		if run.Spec.Stage != v1alpha1.IntentStagePlan {
			t.Errorf("run %s (%s) created while the build model is not admitted", run.Name, run.Spec.Stage)
		}
	}
	if n := len(e.jobs.launched()); n != launched {
		t.Errorf("%d Jobs launched, want the plan's %d alone", n, launched)
	}

	e.setModels(t, &v1alpha1.ProjectModels{Build: "anthropic/claude-fable-5"})
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if build := e.onlyLaunch(t, "build"); build.Model != "anthropic/claude-fable-5" {
		t.Errorf("build launch model = %q, want anthropic/claude-fable-5", build.Model)
	}
}
