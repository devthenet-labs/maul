// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usagerollup"
)

// TestRollUpRemediationUsage: a settled remediation attributed to its
// approver lands once in that person's usage rollup (no project: a Finding
// has none), and its usage finalizer is released.
func TestRollUpRemediationUsage(t *testing.T) {
	objs := runningRemediation()
	rem := objs[1].(*v1alpha1.Remediation)
	rem.Spec.AttributedTo = &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "alice", ID: 42},
		Reason: v1alpha1.AttributionApprover}
	rem.Finalizers = usagerollup.Finalizers(rem.Spec.AttributedTo, v1alpha1.FinalizerJobs)
	rem.Status.Phase = v1alpha1.RunComplete
	rem.Status.Stage = &v1alpha1.StageResult{Outcome: "ok", Harness: "codex",
		Usage: v1alpha1.UsageSummary{OutputTokens: 9, CostUSD: "0.5", Source: v1alpha1.UsageSourceReported,
			Final: true}}
	r, c := newRemediation(t, &fakeCRRunner{status: &jobs.Status{Done: true}}, &fakeForge{}, objs...)
	r.Usage = usagerollup.New(c, c, "patchy", true, nil)
	remOnce(t, r, rem.Name)
	remOnce(t, r, rem.Name)

	var list v1alpha1.UsageRollupList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Spec.Scope.User != "github-id:42" {
		t.Fatalf("rollups = %+v", list.Items)
	}
	var reported, runs int64
	for _, m := range list.Items[0].Status.Months {
		reported += m.ReportedCostMicroUSD
		runs += m.Kinds[stats.UsageKindRemediation].Runs
	}
	if runs != 1 || reported != 500_000 {
		t.Errorf("runs %d, reported cost %d", runs, reported)
	}
	var got v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: rem.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Finalizers, v1alpha1.FinalizerUsageRollup) ||
		!meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionUsageRolledUp) {
		t.Errorf("finalizers %v, conditions %+v", got.Finalizers, got.Status.Conditions)
	}
}
