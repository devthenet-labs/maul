// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usagerollup"
)

// TestRollUpInvestigationUsage: a settled, attributed investigation is folded
// into the org's usage rollup by its own controller, once, and its usage
// finalizer released; an unattributed one (from before the feature) is left
// alone; a deleted one is folded in and let go.
func TestRollUpInvestigationUsage(t *testing.T) {
	orgScope := v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: "org"}
	for _, tc := range []struct {
		name       string
		attributed bool
		deleted    bool
		phase      v1alpha1.RunPhase
		wantRuns   int64
	}{
		{name: "settled", attributed: true, phase: v1alpha1.RunComplete, wantRuns: 1},
		{name: "not applicable", phase: v1alpha1.RunComplete},
		{name: "deleted mid-run", attributed: true, deleted: true, phase: v1alpha1.RunRunning, wantRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, fnd, inv := settledInvestigation(clock.Add(-time.Hour))
			inv.Status.Phase = tc.phase
			inv.Finalizers = []string{v1alpha1.FinalizerJobs}
			if tc.attributed {
				inv.Spec.AttributedTo = attribution.Org()
				inv.Finalizers = usagerollup.Finalizers(inv.Spec.AttributedTo, v1alpha1.FinalizerJobs)
			}
			r, c := newInvestigation(t, runner, fnd, inv)
			r.Usage = usagerollup.New(c, c, "patchy", false, nil)
			key := types.NamespacedName{Namespace: "patchy", Name: inv.Name}
			if tc.deleted {
				var cur v1alpha1.Investigation
				if err := c.Get(t.Context(), key, &cur); err != nil {
					t.Fatal(err)
				}
				if err := c.Delete(t.Context(), &cur); err != nil {
					t.Fatal(err)
				}
			}
			reconcileInvestigation(t, r)
			reconcileInvestigation(t, r) // a second pass changes nothing

			var rollup v1alpha1.UsageRollup
			err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy",
				Name: stats.UsageObjectName(orgScope)}, &rollup)
			if tc.wantRuns == 0 {
				if err == nil {
					t.Fatalf("an unattributed run was aggregated: %+v", rollup.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("org rollup: %v", err)
			}
			var runs int64
			for _, m := range rollup.Status.Months {
				runs += m.Kinds[stats.UsageKindInvestigation].Runs
			}
			if runs != tc.wantRuns {
				t.Errorf("investigation runs = %d, want %d", runs, tc.wantRuns)
			}
			var got v1alpha1.Investigation
			err = c.Get(t.Context(), key, &got)
			if tc.deleted {
				if err == nil {
					t.Errorf("the deleted run is still held: finalizers %v", got.Finalizers)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(got.Finalizers, v1alpha1.FinalizerUsageRollup) ||
				!meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionUsageRolledUp) {
				t.Errorf("finalizers %v, conditions %+v", got.Finalizers, got.Status.Conditions)
			}
		})
	}
}
