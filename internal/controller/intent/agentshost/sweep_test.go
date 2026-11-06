// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestSweep: a tool Job whose verification is gone, is a slot one, or
// settled past the linger is deleted; one whose verification still runs,
// or settled within the linger, is kept; with the agents host off every
// tool Job goes.
func TestSweep(t *testing.T) {
	settledAt := metav1.NewTime(t0)
	pv := func(name string, uid types.UID, phase v1alpha1.PreviewVerificationPhase,
		agents bool) *v1alpha1.PreviewVerification {
		p := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid},
			Status: v1alpha1.PreviewVerificationStatus{Phase: phase}}
		if agents {
			p.Spec.Placement = v1alpha1.PlacementAgents
		}
		if phase == v1alpha1.VerificationObserved {
			p.Status.CompletedAt = &settledAt
		}
		return p
	}
	objs := []client.Object{
		pv("live", "live", v1alpha1.VerificationRunning, true),
		pv("done", "done", v1alpha1.VerificationObserved, true),
		pv("slot", "slot", v1alpha1.VerificationRunning, false),
	}
	jobsOf := func() []batchv1.Job {
		return []batchv1.Job{*toolJob("live", "live"), *toolJob("done", "done"), *toolJob("slot", "slot"),
			*toolJob("gone", "gone"), *toolJob("unlabelled", "")}
	}
	for _, tc := range []struct {
		name    string
		enabled bool
		now     time.Time
		deleted []string
	}{
		{"within the linger", true, t0.Add(time.Minute), []string{"slot", "gone", "unlabelled"}},
		{"past the linger", true, t0.Add(linger), []string{"done", "slot", "gone", "unlabelled"}},
		{"agents host off", false, t0, []string{"live", "done", "slot", "gone", "unlabelled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
			tools := newFakeTools()
			tools.listed = jobsOf()
			s := &Sweeper{Client: c, Tools: tools, Namespace: ns, Enabled: tc.enabled,
				Now: func() time.Time { return tc.now }}
			if err := s.SweepOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			slices.Sort(tools.deleted)
			want := slices.Sorted(slices.Values(tc.deleted))
			if !slices.Equal(tools.deleted, want) {
				t.Errorf("deleted %v, want %v", tools.deleted, want)
			}
		})
	}
}
