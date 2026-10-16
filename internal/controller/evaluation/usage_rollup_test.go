// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evaluation

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usagemeter/usagemetertest"
	"github.com/bitwise-media-group/patchy/internal/usagerollup"
	"github.com/bitwise-media-group/patchy/pkg/evaluation"
)

// TestRollUpUnitUsage: with the meter on, a collected unit waits for its
// usage to be final before it is folded into its user's rollup, then lands
// there once, metered, bucketed by the unit's finish, and lets go of its
// usage finalizer.
func TestRollUpUnitUsage(t *testing.T) {
	eval := testEval(unitPlan("claude"))
	unit := runningUnit(nil)
	jobName := jobs.EvalNameFor(unit.Name)
	unit.Status.JobRef = &v1alpha1.JobReference{Name: jobName}
	unit.Status.Harness = "claude"
	unit.Spec.AttributedTo = attribution.To("alice", 42, v1alpha1.AttributionSubmitter)
	unit.Finalizers = usagerollup.Finalizers(unit.Spec.AttributedTo, v1alpha1.FinalizerJobs)
	c := newClient(t, eval, unit)
	jobCreated := clock.Add(-time.Hour)
	runner := &fakeRunner{
		status: map[string]jobs.Status{jobName: {Succeeded: 1, Done: true, Created: jobCreated}},
		lines: map[string][]string{jobName: {resultLine(t, &evaluation.UnitResult{
			Tier: 2, Model: "anthropic/claude-sonnet-5", Harness: "claude",
			Summary: evaluation.ResultSummary{CasesPassed: 1, Outcome: "ok",
				TokenUsage: evaluation.TokenUsage{InputTokens: 1000, CostUSD: 0.75}},
		})}},
	}
	r := newUnitReconciler(c, runner, fakeWorkspaces{present: map[string]bool{digestA: true}})
	broker, meter := usagemetertest.New(t, func() time.Time { return clock })
	r.Meter = meter
	r.Usage = usagerollup.New(c, c, "patchy", meter.On(), nil)
	broker.Set(jobName, usagemetertest.Answer(jobName, jobCreated.Add(-time.Hour), jobCreated.Add(time.Minute), 0,
		usagemetertest.Sonnet(0)))

	reconcileUnit(t, r, unit.Name) // collect: settled, usage not yet final
	var list v1alpha1.UsageRollupList
	if err := c.List(t.Context(), &list, client.InNamespace("patchy")); err != nil || len(list.Items) != 0 {
		t.Fatalf("aggregated before the usage was final: %d, %v", len(list.Items), err)
	}
	reconcileUnit(t, r, unit.Name) // measure, then aggregate
	reconcileUnit(t, r, unit.Name) // nothing more

	if err := c.List(t.Context(), &list, client.InNamespace("patchy")); err != nil || len(list.Items) != 1 {
		t.Fatalf("rollups = %d, %v", len(list.Items), err)
	}
	st := list.Items[0].Status
	got := getUnit(t, c, unit.Name)
	day := st.Days[got.Status.FinishedAt.UTC().Format("2006-01-02")]
	if day.Runs != 1 || day.ReportedRuns != 0 || day.CostMicroUSD != 3_000_000 ||
		day.Kinds[stats.UsageKindEvaluation].Runs != 1 || st.Login != "alice" {
		t.Errorf("day = %+v, login %q", day, st.Login)
	}
	if slices.Contains(got.Finalizers, v1alpha1.FinalizerUsageRollup) ||
		!meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionUsageRolledUp) {
		t.Errorf("finalizers %v, conditions %+v", got.Finalizers, got.Status.Conditions)
	}
}

// TestRollUpCancelledUnitMeasured: a unit deleted mid-run (its Evaluation
// cancelled) is measured against the broker before it is folded in, so the
// spend its Job already ran up reaches its submitter's trusted totals
// instead of a zero reported run.
func TestRollUpCancelledUnitMeasured(t *testing.T) {
	eval := testEval(unitPlan("claude"))
	unit := runningUnit(nil)
	jobName := jobs.EvalNameFor(unit.Name)
	unit.Status.JobRef = &v1alpha1.JobReference{Name: jobName}
	unit.Status.Harness = "claude"
	unit.Spec.AttributedTo = attribution.To("alice", 42, v1alpha1.AttributionSubmitter)
	unit.Finalizers = usagerollup.Finalizers(unit.Spec.AttributedTo, v1alpha1.FinalizerJobs)
	c := newClient(t, eval, unit)
	jobCreated := clock.Add(-time.Hour)
	runner := &fakeRunner{status: map[string]jobs.Status{jobName: {Created: jobCreated}}}
	r := newUnitReconciler(c, runner, fakeWorkspaces{present: map[string]bool{digestA: true}})
	broker, meter := usagemetertest.New(t, func() time.Time { return clock })
	r.Meter = meter
	r.Usage = usagerollup.New(c, c, "patchy", meter.On(), nil)
	broker.Set(jobName, usagemetertest.Answer(jobName, jobCreated.Add(-time.Hour), jobCreated.Add(time.Minute), 0,
		usagemetertest.Sonnet(0)))
	cur := getUnit(t, c, unit.Name)
	if err := c.Delete(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileUnit(t, r, unit.Name)

	if got := len(broker.Asked()); got != 1 {
		t.Errorf("broker asked %d times, want 1", got)
	}
	var list v1alpha1.UsageRollupList
	if err := c.List(t.Context(), &list, client.InNamespace("patchy")); err != nil || len(list.Items) != 1 {
		t.Fatalf("rollups = %d, %v", len(list.Items), err)
	}
	var runs, reported, cost int64
	for _, m := range list.Items[0].Status.Months {
		runs, reported, cost = runs+m.Runs, reported+m.ReportedRuns, cost+m.CostMicroUSD
	}
	if runs != 1 || reported != 0 || cost != 3_000_000 {
		t.Errorf("runs %d, reported %d, cost %d; want one measured run of $3", runs, reported, cost)
	}
}
