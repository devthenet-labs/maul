// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evaluation

import (
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/usagemeter"
	"github.com/bitwise-media-group/patchy/internal/usagemeter/usagemetertest"
	"github.com/bitwise-media-group/patchy/pkg/evaluation"
)

// TestMeasureSettledUnit: a collected unit's usage is settled from the
// broker on the next pass, beside the pod's own figure; while the pod's
// requests are still in flight it is looked at again, and the wait ends.
func TestMeasureSettledUnit(t *testing.T) {
	eval := testEval(unitPlan("claude"))
	unit := runningUnit(nil)
	jobName := jobs.EvalNameFor(unit.Name)
	unit.Status.JobRef = &v1alpha1.JobReference{Name: jobName}
	unit.Status.Harness = "claude"
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
	now := clock
	r := newUnitReconciler(c, runner, fakeWorkspaces{present: map[string]bool{digestA: true}})
	broker, meter := usagemetertest.New(t, func() time.Time { return now })
	r.Meter = meter
	broker.Set(jobName, usagemetertest.Answer(jobName, jobCreated.Add(-time.Hour), jobCreated.Add(time.Minute), 1,
		usagemetertest.Sonnet(0)))

	reconcileUnit(t, r, unit.Name) // collect
	if res := reconcileUnit(t, r, unit.Name); res.RequeueAfter != usagemeter.DefaultPoll {
		t.Fatalf("in flight: requeue %v, want %v", res.RequeueAfter, usagemeter.DefaultPoll)
	}
	if u := getUnit(t, c, unit.Name).Status.Usage; u.Final || u.Measured != nil {
		t.Fatalf("written while in flight: %+v", u)
	}
	// The wait ends with requests still in flight: partial, and final.
	now = now.Add(usagemeter.DefaultWait)
	reconcileUnit(t, r, unit.Name)
	u := getUnit(t, c, unit.Name).Status.Usage
	if !u.Final || u.Source != v1alpha1.UsageSourcePartial || u.Measured == nil || u.Measured.CostUSD != "3.000000" ||
		u.CostUSD != "0.750000" || u.InputTokens != 1000 {
		t.Fatalf("usage = %+v", u)
	}
}
