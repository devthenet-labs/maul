// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestRetiredCheckReviewSpecNeverBlocksReady ("evaluator observed"): no
// checkreview run is ever created, so spec.models.checkReview and
// spec.limits.checkReview are inert retired fields, kept in the schema for
// one release. Whatever they hold, a Project the evaluator verifies is
// Ready: a model no flag or allowlist admits, a budget over half the
// broker's hourly limit, and one under two of its request reservations.
func TestRetiredCheckReviewSpecNeverBlocksReady(t *testing.T) {
	project := func(set func(*v1alpha1.Project)) *v1alpha1.Project {
		p := evaluatingProject()
		set(p)
		return p
	}
	for _, tt := range []struct {
		name   string
		hourly int64
		p      *v1alpha1.Project
	}{
		{"a model nobody admits", 0, project(func(p *v1alpha1.Project) {
			p.Spec.Models = &v1alpha1.ProjectModels{CheckReview: "anthropic/claude-sonnet-4"}
		})},
		{"a budget over half the hourly limit", 16000000, project(func(p *v1alpha1.Project) {
			p.Spec.Limits.MaxCostMicroUSD = 1000000
			p.Spec.Limits.MaxEvaluateCostMicroUSD = new(int64(9000000))
			for _, st := range []*v1alpha1.StageLimits{&p.Spec.Limits.Plan, &p.Spec.Limits.Build,
				&p.Spec.Limits.Revise, &p.Spec.Limits.Evaluate} {
				st.MaxCostMicroUSD = 1000000
			}
			p.Spec.Limits.CheckReview.MaxCostMicroUSD = 9000000
		})},
		{"a budget under two request reservations", 0, project(func(p *v1alpha1.Project) {
			// A priced model the stage could run on, so the reservation
			// would be judged were the stage still read.
			p.Spec.Models = &v1alpha1.ProjectModels{CheckReview: "anthropic/claude-sonnet-5"}
			p.Spec.Limits.Evaluate.MaxCostMicroUSD = 1500000
			p.Spec.Limits.CheckReview.MaxCostMicroUSD = 1
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := roleSettings()
			s.EvaluateModel = "anthropic/claude-sonnet-5"
			s.BrokerCostPerHour = tt.hourly
			if reason, msg := s.refusal(tt.p); reason != "" {
				t.Fatalf("refusal = %q (%s), want none", reason, msg)
			}
			e := newEnv(t, tt.p.DeepCopy())
			e.project.Settings = s
			e.reconcileProject()
			c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionReady)
			if c == nil || c.Status != metav1.ConditionTrue {
				t.Fatalf("Ready = %+v, want True", c)
			}
		})
	}
}

// TestRetiredStageNeverLaunched ("evaluator observed"): a checkreview run
// an earlier release left, pending or running, paused or not, under an
// evaluator that has no check reviewer's ceiling or model, is stopped
// before anything reads its stage's settings: never granted a slot or
// launched, its Job deleted, settled Aborted as retired, and every later
// pass leaves it so. Its spend still counts toward the evaluation ceiling.
func TestRetiredStageNeverLaunched(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		paused  bool
	}{
		{"pending", false, false},
		{"pending, paused", false, true},
		{"running", true, false},
		{"running, paused", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, evaluatingProject())
			s := testSettings()
			s.Verification, s.Previews, s.Evaluator = true, true, true
			s.Evaluate = StageCeiling{MaxTurns: DefaultEvaluateMaxTurns, TokenBudget: DefaultEvaluateTokenBudget,
				Timeout: DefaultEvaluateTimeout}
			s.EvaluateModel = "anthropic/claude-sonnet-5"
			e.withSettings(s)
			e.runs.Paused = tc.paused
			run := stageRunOf(v1alpha1.IntentStageCheckReview)
			run.ObjectMeta = metav1.ObjectMeta{Name: "left-checkreview", Namespace: testNS}
			if err := e.c.Create(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			const job = "left-checkreview-job"
			if tc.running {
				run.Status.Phase = v1alpha1.RunRunning
				run.Status.JobRef = &v1alpha1.JobReference{Name: job}
				if err := e.c.Status().Update(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				for _, name := range []string{runSchedulerRequest, run.Name} {
					if _, err := e.runs.Reconcile(t.Context(), req(name)); err != nil {
						t.Fatalf("reconcile %s: %v", name, err)
					}
				}
			}
			var got v1alpha1.IntentRun
			if err := e.c.Get(t.Context(), client.ObjectKeyFromObject(run), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != v1alpha1.RunFailed || got.Status.Outcome != OutcomeAborted ||
				got.Status.Detail != retiredDetail {
				t.Errorf("run = %s %s %q, want failed %s as retired", got.Status.Phase, got.Status.Outcome,
					got.Status.Detail, OutcomeAborted)
			}
			if n := len(e.jobs.launchedInOrder()); n != 0 {
				t.Errorf("%d Jobs launched, want none", n)
			}
			if tc.running && !slices.Contains(e.jobs.deleted, job) {
				t.Errorf("deleted Jobs %v, want the run's %s", e.jobs.deleted, job)
			}
		})
	}
	spent := stageRunOf(v1alpha1.IntentStageCheckReview)
	spent.Status.Usage.CostUSD = "0.25"
	p := &pass{runs: []*v1alpha1.IntentRun{spent}}
	if got := p.evaluationSpend(); got != 250000 {
		t.Errorf("evaluationSpend = %d, want the checkreview run's 250000 micro-USD", got)
	}
}
