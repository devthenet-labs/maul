// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package evaluation

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// TestGateAttribution is the design §2 row for an evaluation unit: its
// submitter, who is a verified username and never a GitHub id yet, so under
// mode attribute every unit is re-attributed to the org (reason
// submitter-unregistered); under off nothing is stamped.
func TestGateAttribution(t *testing.T) {
	for _, tc := range []struct {
		mode attribution.Mode
		want *v1alpha1.Attribution
	}{
		{attribution.ModeOff, nil},
		{attribution.ModeAttribute, &v1alpha1.Attribution{Reason: v1alpha1.AttributionSubmitterUnregistered}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			eval := testEval(unitPlan("claude"))
			eval.Spec.Submitter = "github:alice"
			c := newClient(t, eval)
			g := &GateReconciler{Client: c, Namespace: "patchy", EnabledHarnesses: []string{"claude"},
				Attribution: tc.mode, Now: func() time.Time { return clock }}
			if _, err := g.Reconcile(t.Context(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "eval-1"},
			}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			var units v1alpha1.EvaluationUnitList
			if err := c.List(t.Context(), &units, client.InNamespace("patchy")); err != nil || len(units.Items) != 1 {
				t.Fatalf("units = %d, %v; want 1", len(units.Items), err)
			}
			if got := units.Items[0].Spec.AttributedTo; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("attributedTo = %+v, want %+v", got, tc.want)
			}
			// The usage finalizer rides with the stamp.
			fins := units.Items[0].Finalizers
			if got := slices.Contains(fins, v1alpha1.FinalizerUsageRollup); got != (tc.want != nil) {
				t.Errorf("usage finalizer = %v, want %v (finalizers %v)", got, tc.want != nil, fins)
			}
		})
	}
}

// TestLaunchAttribution: the unit's Job carries its stamp's annotation
// under attribute ("org" for an unstamped unit), none under off.
func TestLaunchAttribution(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  attribution.Mode
		stamp *v1alpha1.Attribution
		want  string
	}{
		{"off", attribution.ModeOff, attribution.Org(), ""},
		{"unregistered", attribution.ModeAttribute,
			&v1alpha1.Attribution{Reason: v1alpha1.AttributionSubmitterUnregistered}, "org"},
		{"unstamped", attribution.ModeAttribute, nil, "org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unit := runningUnit(nil)
			unit.Spec.AttributedTo = tc.stamp
			c := newClient(t, testEval(unitPlan("claude")), unit)
			runner := &fakeRunner{}
			r := newUnitReconciler(c, runner, fakeWorkspaces{present: map[string]bool{digestA: true}})
			r.Attribution = tc.mode
			reconcileUnit(t, r, unit.Name)
			if len(runner.created) != 1 {
				t.Fatalf("created jobs = %d, want 1", len(runner.created))
			}
			if got := runner.created[0].AttributedTo; got != tc.want {
				t.Errorf("Job attributed to %q, want %q", got, tc.want)
			}
		})
	}
}
