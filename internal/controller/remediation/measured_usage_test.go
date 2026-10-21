// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/usagemeter/usagemetertest"
	"github.com/devthenet-labs/maul/internal/usagerollup"
)

// TestMeasureSettledRemediation: a finished remediation's usage is settled
// from the broker on the harness the spawner resolved, whatever the pod
// claims it ran: a pod reporting "copilot" on a claude run is still
// measured, a copilot run (not brokered) is reported without asking the
// broker even when its pod claims claude, and a codex run, brokered since
// codex runs proxy-only, is measured.
func TestMeasureSettledRemediation(t *testing.T) {
	jobCreated := crdClock.Add(-time.Hour)
	for _, tc := range []struct {
		name, specHarness, podHarness string
		want                          v1alpha1.UsageSource
		asked                         int
	}{
		{"claude run, pod claims copilot", "claude", "copilot", v1alpha1.UsageSourceMeasured, 1},
		{"copilot run, pod claims claude", "copilot", "claude", v1alpha1.UsageSourceReported, 0},
		{"codex run", "codex", "codex", v1alpha1.UsageSourceMeasured, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := runningRemediation()
			rem := objs[1].(*v1alpha1.Remediation)
			rem.Spec.Parameters.Harness = tc.specHarness
			rem.Status.Phase = v1alpha1.RunComplete
			rem.Status.Stage = &v1alpha1.StageResult{Outcome: "ok", Harness: tc.podHarness,
				Usage: v1alpha1.UsageSummary{OutputTokens: 9}}
			runner := &fakeCRRunner{status: &jobs.Status{Done: true, Created: jobCreated}}
			r, c := newRemediation(t, runner, &fakeForge{}, objs...)
			broker, meter := usagemetertest.New(t, func() time.Time { return crdClock })
			broker.Set("job-rem-1", usagemetertest.Answer("job-rem-1", jobCreated.Add(-time.Hour),
				jobCreated.Add(time.Minute), 0, usagemetertest.Sonnet(0)))
			r.Meter = meter
			remOnce(t, r, rem.Name)

			if got := len(broker.Asked()); got != tc.asked {
				t.Errorf("broker asked %d times, want %d", got, tc.asked)
			}
			var got v1alpha1.Remediation
			if err := c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: rem.Name}, &got); err != nil {
				t.Fatal(err)
			}
			u := got.Status.Stage.Usage
			if !u.Final || u.Source != tc.want || u.OutputTokens != 9 {
				t.Fatalf("usage = %+v", u)
			}
			if (u.Measured != nil) != (tc.want == v1alpha1.UsageSourceMeasured) {
				t.Errorf("measured = %+v", u.Measured)
			}
		})
	}
}

// TestMeasureDeletedRemediation: an attributed remediation deleted mid-run
// (its Finding deleted under it) is measured against the broker before it is
// folded in, an aborted stage holding the measurement, so its metered spend
// reaches its person's trusted totals instead of a zero reported run.
func TestMeasureDeletedRemediation(t *testing.T) {
	jobCreated := crdClock.Add(-time.Hour)
	objs := runningRemediation()
	rem := objs[1].(*v1alpha1.Remediation)
	rem.Spec.AttributedTo = &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "alice", ID: 42},
		Reason: v1alpha1.AttributionApprover}
	rem.Finalizers = usagerollup.Finalizers(rem.Spec.AttributedTo, v1alpha1.FinalizerJobs)
	runner := &fakeCRRunner{status: &jobs.Status{Created: jobCreated}}
	r, c := newRemediation(t, runner, &fakeForge{}, objs...)
	broker, meter := usagemetertest.New(t, func() time.Time { return crdClock })
	broker.Set("job-rem-1", usagemetertest.Answer("job-rem-1", jobCreated.Add(-time.Hour),
		jobCreated.Add(time.Minute), 0, usagemetertest.Sonnet(0)))
	r.Meter = meter
	r.Usage = usagerollup.New(c, c, "maul", true, nil)
	key := types.NamespacedName{Namespace: "maul", Name: rem.Name}
	var cur v1alpha1.Remediation
	if err := c.Get(t.Context(), key, &cur); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), &cur); err != nil {
		t.Fatal(err)
	}
	remOnce(t, r, rem.Name)

	if got := len(broker.Asked()); got != 1 {
		t.Errorf("broker asked %d times, want 1", got)
	}
	if err := c.Get(t.Context(), key, &cur); err == nil {
		t.Errorf("the deleted run is still held: finalizers %v", cur.Finalizers)
	}
	var list v1alpha1.UsageRollupList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("rollups = %+v", list.Items)
	}
	month, ok := list.Items[0].Status.Months[crdClock.Format("2006-01")]
	if !ok || month.Runs != 1 || month.ReportedRuns != 0 || month.InputTokens != 1_000_000 ||
		month.CostMicroUSD != 3_000_000 {
		t.Errorf("months = %+v, want one measured run of 1M input tokens and $3 in %s",
			list.Items[0].Status.Months, crdClock.Format("2006-01"))
	}
}
