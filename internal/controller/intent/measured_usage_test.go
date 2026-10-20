// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/usagemeter/usagemetertest"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// TestMeasureSettledRuns: with the meter on, every intent run that settles
// (the plan, then the build) has its usage settled from the broker, measured
// and priced beside the pod's own figure, and asked for by its own Job's
// name; with it off (every other test) nothing is written.
func TestMeasureSettledRuns(t *testing.T) {
	e := newEnv(t, testProject())
	start := e.clock.Now()
	e.jobs.statusFn = func(jobs.Spec) (jobs.Status, bool) {
		return jobs.Status{Done: true, Succeeded: 1, Created: start}, true
	}
	broker, meter := usagemetertest.New(t, e.clock.Now)
	broker.SetDefault(func(job string) usagewire.Response {
		return usagemetertest.Answer(job, start.Add(-time.Hour), start.Add(time.Minute), 0,
			usagemetertest.Sonnet(250))
	})
	e.runs.Meter = meter
	name := e.newIntent(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.runRuns() // a pass after the last run settled

	runs := e.intentRuns(name)
	if len(runs) < 2 {
		t.Fatalf("runs = %d, want a plan and a build", len(runs))
	}
	asked := map[string]bool{}
	for _, j := range broker.Asked() {
		asked[j] = true
	}
	for _, run := range runs {
		u := run.Status.Usage
		if !u.Final || u.Source != v1alpha1.UsageSourceMeasured || u.Measured == nil ||
			u.Measured.CostUSD != "3.000000" || u.Measured.EstimatedShortfallTokens != 250 {
			t.Errorf("run %s (%s) usage = %+v", run.Name, run.Spec.Stage, u)
		}
		if run.Status.JobRef == nil || !asked[run.Status.JobRef.Name] {
			t.Errorf("run %s: the broker was not asked for its Job", run.Name)
		}
	}
}
