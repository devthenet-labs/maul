// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usagemeter"
	"github.com/bitwise-media-group/patchy/internal/usagemeter/usagemetertest"
	"github.com/bitwise-media-group/patchy/internal/usagerollup"
)

// settledInvestigation is a finished investigation whose pod reported its
// own usage, with its Job created at jobCreated.
func settledInvestigation(jobCreated time.Time) (*fakeRunner, *v1alpha1.Finding, *v1alpha1.Investigation) {
	objs := investigationFixture()
	inv := objs[1].(*v1alpha1.Investigation)
	inv.Status.Phase = v1alpha1.RunComplete
	inv.Status.Stage = &v1alpha1.StageResult{Outcome: "ok", Harness: "claude",
		Usage: v1alpha1.UsageSummary{InputTokens: 12, CostUSD: "0.010000"}}
	return &fakeRunner{status: &jobs.Status{Done: true, Created: jobCreated}}, objs[0].(*v1alpha1.Finding), inv
}

func reconcileInvestigation(t *testing.T, r *InvestigationReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-inv-1"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// TestMeasureSettledInvestigation: under --usage-measurement a finished
// investigation's usage is settled from the broker: measured and priced
// when the broker booted before the Job, kept apart from the pod's own
// figure; looked at again while requests are in flight; reported without
// asking for a harness the broker does not carry; never written while the
// meter is off or once final.
func TestMeasureSettledInvestigation(t *testing.T) {
	jobCreated := clock.Add(-time.Hour)
	before, seen := jobCreated.Add(-time.Hour), jobCreated.Add(time.Minute)
	for _, tc := range []struct {
		name      string
		meterOff  bool
		harness   string
		final     bool
		wantSrc   v1alpha1.UsageSource
		wantCost  string
		wantAsked int
		requeue   bool
	}{
		{name: "off", meterOff: true, harness: "claude"},
		{name: "measured", harness: "claude", final: true, wantSrc: v1alpha1.UsageSourceMeasured,
			wantCost: "3.000000", wantAsked: 1},
		{name: "broker restarted mid-run", harness: "claude", final: true, wantSrc: v1alpha1.UsageSourcePartial,
			wantCost: "3.000000", wantAsked: 1},
		{name: "in flight", harness: "claude", requeue: true, wantAsked: 1},
		{name: "not brokered", harness: "copilot", final: true, wantSrc: v1alpha1.UsageSourceReported},
		{name: "codex, brokered", harness: "codex", final: true, wantSrc: v1alpha1.UsageSourceMeasured,
			wantCost: "3.000000", wantAsked: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, fnd, inv := settledInvestigation(jobCreated)
			r, c := newInvestigation(t, runner, fnd, inv)
			r.InvestigateHarness = tc.harness
			broker, meter := usagemetertest.New(t, func() time.Time { return clock })
			boot := before
			if tc.name == "broker restarted mid-run" {
				boot = jobCreated.Add(time.Second)
			}
			var inflight int64
			if tc.requeue {
				inflight = 1
			}
			broker.Set("job-1", usagemetertest.Answer("job-1", boot, seen, inflight, usagemetertest.Sonnet(400)))
			if !tc.meterOff {
				r.Meter = meter
			}
			res := reconcileInvestigation(t, r)
			if tc.requeue != (res.RequeueAfter == usagemeter.DefaultPoll) {
				t.Errorf("requeue = %v", res.RequeueAfter)
			}
			if got := len(broker.Asked()); got != tc.wantAsked {
				t.Errorf("broker asked %d times, want %d", got, tc.wantAsked)
			}
			var got v1alpha1.Investigation
			if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: inv.Name}, &got); err != nil {
				t.Fatal(err)
			}
			u := got.Status.Stage.Usage
			if u.Final != tc.final || u.Source != tc.wantSrc || u.InputTokens != 12 || u.CostUSD != "0.010000" {
				t.Fatalf("usage = %+v", u)
			}
			if tc.wantCost == "" {
				if u.Measured != nil {
					t.Errorf("measured = %+v, want none", u.Measured)
				}
				return
			}
			if u.Measured == nil || u.Measured.CostUSD != tc.wantCost || u.Measured.InputTokens != 1_000_000 ||
				u.Measured.EstimatedShortfallTokens != 400 || u.Measured.Models[0].Model != "anthropic/claude-sonnet-5" {
				t.Errorf("measured = %+v", u.Measured)
			}
			// Final is final: another pass asks nothing and writes nothing.
			asked := len(broker.Asked())
			reconcileInvestigation(t, r)
			if len(broker.Asked()) != asked {
				t.Error("a final usage was measured again")
			}
		})
	}
}

// TestMeasureDeletedInvestigation: an attributed investigation deleted
// before its usage was final (mid-run, with no stage yet, or in the settle
// window) is still measured against the broker before it is folded in, so
// its metered spend reaches the trusted totals; while requests are in
// flight it is looked at again, and it is never let go before.
func TestMeasureDeletedInvestigation(t *testing.T) {
	jobCreated := clock.Add(-time.Hour)
	before, seen := jobCreated.Add(-time.Hour), jobCreated.Add(time.Minute)
	for _, tc := range []struct {
		name     string
		midRun   bool
		inflight bool
	}{
		{name: "deleted mid-run", midRun: true},
		{name: "deleted in the settle window"},
		{name: "in flight", midRun: true, inflight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, fnd, inv := settledInvestigation(jobCreated)
			if tc.midRun {
				inv.Status.Phase, inv.Status.Stage = v1alpha1.RunRunning, nil
			}
			inv.Spec.AttributedTo = attribution.Org()
			inv.Finalizers = usagerollup.Finalizers(inv.Spec.AttributedTo, v1alpha1.FinalizerJobs)
			r, c := newInvestigation(t, runner, fnd, inv)
			r.InvestigateHarness = "claude"
			broker, meter := usagemetertest.New(t, func() time.Time { return clock })
			var inflight int64
			if tc.inflight {
				inflight = 1
			}
			broker.Set("job-1", usagemetertest.Answer("job-1", before, seen, inflight, usagemetertest.Sonnet(400)))
			r.Meter = meter
			r.Usage = usagerollup.New(c, c, "patchy", true, nil)
			key := types.NamespacedName{Namespace: "patchy", Name: inv.Name}
			var cur v1alpha1.Investigation
			if err := c.Get(t.Context(), key, &cur); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(t.Context(), &cur); err != nil {
				t.Fatal(err)
			}
			res := reconcileInvestigation(t, r)
			if len(broker.Asked()) != 1 {
				t.Errorf("broker asked %d times, want 1", len(broker.Asked()))
			}
			if tc.inflight {
				if res.RequeueAfter != usagemeter.DefaultPoll {
					t.Errorf("requeue = %v, want a poll", res.RequeueAfter)
				}
				if err := c.Get(t.Context(), key, &cur); err != nil {
					t.Errorf("a run still in flight was let go: %v", err)
				}
				return
			}
			if err := c.Get(t.Context(), key, &cur); err == nil {
				t.Errorf("the deleted run is still held: finalizers %v", cur.Finalizers)
			}
			var rollup v1alpha1.UsageRollup
			if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy",
				Name: stats.UsageObjectName(v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: "org"})},
				&rollup); err != nil {
				t.Fatalf("org rollup: %v", err)
			}
			if len(rollup.Status.Months) != 1 {
				t.Fatalf("months = %+v", rollup.Status.Months)
			}
			for _, month := range rollup.Status.Months {
				if month.Runs != 1 || month.ReportedRuns != 0 || month.InputTokens != 1_000_000 ||
					month.CostMicroUSD != 3_000_000 {
					t.Errorf("month = %+v, want one measured run of 1M input tokens and $3", month.UsageAggregate)
				}
			}
		})
	}
}
