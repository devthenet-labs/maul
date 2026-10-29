// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// Every place that switches on a run's stage has a row for each stage the
// schema admits, and none of them reaches its default branch for one: a
// stage added to v1alpha1.IntentStages fails here until every site decides
// what it means, so no site silently treats it as a plan or a build. The
// default branch itself fails closed, which an unknown stage pins.

const (
	sPlan        = v1alpha1.IntentStagePlan
	sBuild       = v1alpha1.IntentStageBuild
	sRevise      = v1alpha1.IntentStageRevise
	sEvaluate    = v1alpha1.IntentStageEvaluate
	sCheckReview = v1alpha1.IntentStageCheckReview
	// sUnknown is a stage no site knows: every site's default branch.
	sUnknown = v1alpha1.IntentStage("unknown")
)

// stageRunOf is one run of stage, round 1, attempt 1.
func stageRunOf(stage v1alpha1.IntentStage) *v1alpha1.IntentRun {
	return &v1alpha1.IntentRun{Spec: v1alpha1.IntentRunSpec{Stage: stage, Round: 1, Attempt: 1,
		Trigger:    v1alpha1.IntentRunTriggerReview,
		Repository: v1alpha1.IntentRunRepository{URL: "https://github.com/acme/app"}}}
}

// errClass names which of the stage sentinels err is, if any.
func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errStageNotEnabled):
		return "not enabled"
	case errors.Is(err, errUnknownStage):
		return "unknown"
	}
	return "other: " + err.Error()
}

func TestStageSwitchesExhaustive(t *testing.T) {
	ceil := func(n int32) StageCeiling {
		return StageCeiling{MaxTurns: n, TokenBudget: int64(n) * 1000, Timeout: time.Duration(n) * time.Minute,
			IdleTimeout: time.Duration(n) * time.Second}
	}
	set := Settings{Plan: ceil(1), Build: ceil(2), Revise: ceil(3), Evaluate: ceil(4), CheckReview: ceil(5)}
	lim := func(n int32) v1alpha1.StageLimits { return v1alpha1.StageLimits{MaxTurns: n} }
	proj := &v1alpha1.Project{Spec: v1alpha1.ProjectSpec{
		Approvers: v1alpha1.ProjectApprovers{Logins: []string{"alice"}},
		Limits: v1alpha1.ProjectLimits{Plan: lim(1), Build: lim(2), Revise: lim(3), Evaluate: lim(4),
			CheckReview: lim(5)},
	}}
	attributing := &pass{proj: proj, in: &v1alpha1.Intent{Status: v1alpha1.IntentStatus{
		Input:    &v1alpha1.IntentInput{Trigger: &v1alpha1.IntentAction{Login: "alice", UserID: 7}},
		Approval: &v1alpha1.IntentApproval{By: "bob", ByID: 8},
	}}}
	requester := attribution.To("alice", 7, v1alpha1.AttributionRequester)
	approverOf := attribution.To("bob", 8, v1alpha1.AttributionApprover)
	runs := &RunReconciler{}
	type lookup struct {
		value any
		ok    bool
	}
	sites := []struct {
		name string
		got  func(v1alpha1.IntentStage) any
		want map[v1alpha1.IntentStage]any
		// def is what the default branch answers (sUnknown's row).
		def any
	}{
		{"run_controller stagePriority", func(s v1alpha1.IntentStage) any {
			v, ok := stagePriority[s]
			return lookup{v, ok}
		}, map[v1alpha1.IntentStage]any{sPlan: lookup{int32(1), true}, sBuild: lookup{int32(2), true},
			sRevise: lookup{int32(3), true}, sEvaluate: lookup{int32(4), true},
			sCheckReview: lookup{int32(5), true}}, lookup{int32(0), false}},
		{"run_controller jobPhase", func(s v1alpha1.IntentStage) any {
			v, ok := jobPhase[s]
			return lookup{v, ok}
		}, map[v1alpha1.IntentStage]any{sPlan: lookup{"plan", true}, sBuild: lookup{"build", true},
			sRevise: lookup{"build", true}, sEvaluate: lookup{"evaluate", true},
			sCheckReview: lookup{"checkreview", true}}, lookup{"", false}},
		{"names ceiling", func(s v1alpha1.IntentStage) any { return set.ceiling(s) },
			map[v1alpha1.IntentStage]any{sPlan: ceil(1), sBuild: ceil(2), sRevise: ceil(3), sEvaluate: ceil(4),
				sCheckReview: ceil(5)}, StageCeiling{}},
		{"names stageLimits", func(s v1alpha1.IntentStage) any { return stageLimits(proj, s) },
			map[v1alpha1.IntentStage]any{sPlan: lim(1), sBuild: lim(2), sRevise: lim(3), sEvaluate: lim(4),
				sCheckReview: lim(5)}, v1alpha1.StageLimits{}},
		{"jobs stageEnv", func(s v1alpha1.IntentStage) any {
			env, err := stageEnv(s, set, v1alpha1.IntentRunGrant{})
			if err != nil {
				return errClass(err)
			}
			return env["MAUL_INVESTIGATE_MAX_TURNS"] + "/" + env["MAUL_REMEDIATE_MANUAL_MAX_TURNS"]
		}, map[v1alpha1.IntentStage]any{sPlan: "1/0", sBuild: "/2", sRevise: "/3", sEvaluate: "not enabled",
			sCheckReview: "not enabled"}, "unknown"},
		{"jobs stageTimeoutKey", func(s v1alpha1.IntentStage) any {
			key, err := stageTimeoutKey(s)
			if err != nil {
				return errClass(err)
			}
			return key
		}, map[v1alpha1.IntentStage]any{sPlan: "MAUL_INVESTIGATE_TIMEOUT", sBuild: "MAUL_REMEDIATE_TIMEOUT",
			sRevise: "MAUL_REMEDIATE_TIMEOUT", sEvaluate: "not enabled", sCheckReview: "not enabled"}, "unknown"},
		{"run_controller launchEnabled", func(s v1alpha1.IntentStage) any { return errClass(launchEnabled(s)) },
			map[v1alpha1.IntentStage]any{sPlan: "ok", sBuild: "ok", sRevise: "ok", sEvaluate: "not enabled",
				sCheckReview: "not enabled"}, "unknown"},
		{"attribute attributeRun", func(s v1alpha1.IntentStage) any { return attributing.attributeRun(s) },
			map[v1alpha1.IntentStage]any{sPlan: requester, sBuild: approverOf, sRevise: approverOf,
				sEvaluate: approverOf, sCheckReview: approverOf}, attribution.Org()},
		{"resources stageNoun", func(s v1alpha1.IntentStage) any { return stageNoun(stageRunOf(s)) },
			map[v1alpha1.IntentStage]any{sPlan: "plan", sBuild: "build", sRevise: "revise round",
				sEvaluate: "evaluation round", sCheckReview: "check review"}, `"unknown" run`},
		{"run_controller stageSpec", func(s v1alpha1.IntentStage) any {
			_, refusal := runs.stageSpec(stageRunOf(s), &jobs.Spec{}, &corev1.ConfigMap{}, proj,
				&v1alpha1.Repository{})
			switch {
			case refusal == nil:
				return "launched"
			case strings.Contains(refusal.detail, errStageNotEnabled.Error()):
				return "not enabled: " + refusal.outcome
			case strings.Contains(refusal.detail, errUnknownStage.Error()):
				return "unknown: " + refusal.outcome
			}
			// A known stage's own checks refuse the empty fixture.
			return "checked: " + refusal.outcome
		}, map[v1alpha1.IntentStage]any{sPlan: "checked: " + OutcomeAborted, sBuild: "checked: " + OutcomeAborted,
			sRevise:      "checked: " + OutcomeAborted,
			sEvaluate:    "not enabled: " + OutcomeLaunchRefused,
			sCheckReview: "not enabled: " + OutcomeLaunchRefused}, "unknown: " + OutcomeAborted},
	}
	stages := v1alpha1.IntentStages()
	for _, site := range sites {
		if got := site.got(sUnknown); !reflect.DeepEqual(got, site.def) {
			t.Errorf("%s(%q) = %#v, want the default branch's %#v", site.name, sUnknown, got, site.def)
		}
		if len(site.want) != len(stages) {
			t.Errorf("%s: %d rows for %d stages", site.name, len(site.want), len(stages))
		}
		for _, s := range stages {
			want, ok := site.want[s]
			if !ok {
				t.Errorf("%s: no row for stage %q", site.name, s)
				continue
			}
			got := site.got(s)
			if reflect.DeepEqual(got, site.def) {
				t.Errorf("%s(%q) reached the default branch (%#v)", site.name, s, got)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s(%q) = %#v, want %#v", site.name, s, got, want)
			}
		}
	}
}

// Neither new stage's run is created before its round exists (S1-17):
// createRun refuses it before it reads or writes anything.
func TestCreateRunRefusesStagesNotEnabled(t *testing.T) {
	for _, s := range []v1alpha1.IntentStage{sEvaluate, sCheckReview, sUnknown} {
		run, err := (&pass{}).createRun(t.Context(), s, v1alpha1.ProjectRepository{Name: "app"}, 1, 1, nil)
		if run != nil || err == nil {
			t.Fatalf("createRun(%q) = %v, %v; want a refusal", s, run, err)
		}
		if want := map[v1alpha1.IntentStage]string{sEvaluate: "not enabled", sCheckReview: "not enabled",
			sUnknown: "unknown"}[s]; errClass(err) != want {
			t.Errorf("createRun(%q) error %v is %q, want %q", s, err, errClass(err), want)
		}
	}
}

// A granted run of a stage not enabled (one made by hand) launches no Job:
// it ends launch_refused, saying why.
func TestLaunchRefusesStagesNotEnabled(t *testing.T) {
	for _, s := range []v1alpha1.IntentStage{sEvaluate, sCheckReview} {
		t.Run(string(s), func(t *testing.T) {
			e := newEnv(t, testProject())
			run := stageRunOf(s)
			run.ObjectMeta = metav1.ObjectMeta{Name: "hand-made-" + string(s), Namespace: testNS}
			if err := e.c.Create(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			run.Status.Phase = v1alpha1.RunRunning
			if err := e.c.Status().Update(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			if err := e.runs.launch(t.Context(), run); err != nil {
				t.Fatalf("launch: %v", err)
			}
			var got v1alpha1.IntentRun
			if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(run), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != v1alpha1.RunFailed || got.Status.Outcome != OutcomeLaunchRefused ||
				!strings.Contains(got.Status.Detail, errStageNotEnabled.Error()) {
				t.Errorf("run = %s %s %q, want failed %s naming %q", got.Status.Phase, got.Status.Outcome,
					got.Status.Detail, OutcomeLaunchRefused, errStageNotEnabled)
			}
			if n := len(e.jobs.launchedInOrder()); n != 0 {
				t.Errorf("%d Jobs launched, want none", n)
			}
		})
	}
}
