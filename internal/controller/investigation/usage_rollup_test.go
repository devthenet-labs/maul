// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"maps"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TestInvestigationBucketedByCompletion: an investigation created the day
// before it finishes (it waited for a slot across midnight) has its stage's
// finish time stamped when the controller collects it, and lands in the
// rollup's day and month of that finish, not of its creation.
func TestInvestigationBucketedByCompletion(t *testing.T) {
	objs := investigationFixture()
	inv := objs[1].(*v1alpha1.Investigation)
	inv.CreationTimestamp = metav1.NewTime(time.Date(2026, 6, 30, 23, 30, 0, 0, time.UTC))
	inv.Spec.AttributedTo = attribution.Org()
	inv.Finalizers = usagerollup.Finalizers(inv.Spec.AttributedTo, v1alpha1.FinalizerJobs)
	runner := &fakeRunner{done: true, events: investigationEvent("remediate", 0.9, false)}
	r, c := newInvestigation(t, runner, objs...)
	r.Usage = usagerollup.New(c, c, "patchy", false, nil)
	reconcileInvestigation(t, r)
	reconcileInvestigation(t, r)

	var got v1alpha1.Investigation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: inv.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Stage == nil || got.Status.Stage.FinishedAt == nil || !got.Status.Stage.FinishedAt.Time.Equal(clock) {
		t.Fatalf("stage = %+v, want finishedAt %v", got.Status.Stage, clock)
	}
	var rollup v1alpha1.UsageRollup
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy",
		Name: stats.UsageObjectName(v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: "org"})}, &rollup); err != nil {
		t.Fatalf("org rollup: %v", err)
	}
	day, month := clock.Format(time.DateOnly), clock.Format("2006-01")
	if _, ok := rollup.Status.Days[day]; !ok || len(rollup.Status.Days) != 1 {
		t.Errorf("days = %v, want only %s", slices.Collect(maps.Keys(rollup.Status.Days)), day)
	}
	if _, ok := rollup.Status.Months[month]; !ok || len(rollup.Status.Months) != 1 {
		t.Errorf("months = %v, want only %s", slices.Collect(maps.Keys(rollup.Status.Months)), month)
	}
}
