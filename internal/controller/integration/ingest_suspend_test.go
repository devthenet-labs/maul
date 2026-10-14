// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// suspend sets spec.suspend on the named finding, as a human would.
func suspend(t *testing.T, c client.Client, name string) {
	t.Helper()
	f := get(t, c, name)
	f.Spec.Suspend = true
	if err := c.Update(t.Context(), f); err != nil {
		t.Fatalf("suspend %s: %v", name, err)
	}
}

// A suspended generation accepts no new alerts: a new alert of its family
// opens a successor generation — its own tracking issue and status-page row —
// exactly as it would for an admitted, held or terminal generation, instead
// of being folded into a finding nothing will advance and nobody is told
// about.
func TestIngestSuspendedFindingOpensSuccessor(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced} {
		t.Run(string(phase), func(t *testing.T) {
			in, c := newIngestor(t)
			integ := testIntegration()
			if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			first := familyName(integ, testSourceFinding(42), 1)
			if phase != v1alpha1.PhaseOpened {
				f := get(t, c, first)
				f.Status.Phase = phase
				if err := c.Status().Update(t.Context(), f); err != nil {
					t.Fatalf("set phase: %v", err)
				}
			}
			suspend(t, c, first)

			for _, alert := range []int{43, 44} {
				if err := in.Ingest(t.Context(), integ, testSourceFinding(alert)); err != nil {
					t.Fatalf("Ingest(%d): %v", alert, err)
				}
			}

			if e := get(t, c, first); len(e.Spec.Alerts) != 1 || e.Spec.Alerts[0].ID != "42" {
				t.Errorf("suspended alerts = %+v, want alert 42 alone", e.Spec.Alerts)
			}
			second := familyName(integ, testSourceFinding(43), 2)
			succ := get(t, c, second)
			if succ.Spec.Suspend {
				t.Errorf("successor suspended, want it to proceed")
			}
			ids := make([]string, 0, len(succ.Spec.Alerts))
			for _, a := range succ.Spec.Alerts {
				ids = append(ids, a.ID)
			}
			if len(ids) != 2 || ids[0] != "43" || ids[1] != "44" {
				t.Errorf("successor alerts = %v, want 43 and 44 accumulated on it", ids)
			}
			if len(succ.Spec.Related) != 1 || succ.Spec.Related[0].To != first ||
				succ.Spec.Related[0].Relationship != v1alpha1.RelationshipSuccessorOf {
				t.Errorf("successor related = %+v, want successor-of %s", succ.Spec.Related, first)
			}
			if n := len(listFindings(t, c)); n != 2 {
				t.Errorf("findings = %d, want 2 generations", n)
			}
		})
	}
}

// An alert the suspended generation already holds is a redelivery, not new
// evidence: it stays deduplicated there and opens nothing.
func TestIngestSuspendedFindingDedupsHeldAlert(t *testing.T) {
	in, c := newIngestor(t)
	integ := testIntegration()
	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	first := familyName(integ, testSourceFinding(42), 1)
	suspend(t, c, first)
	// A successor exists too, so the held alert must not fold into it either.
	if err := in.Ingest(t.Context(), integ, testSourceFinding(43)); err != nil {
		t.Fatalf("Ingest(43): %v", err)
	}

	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("redeliver 42: %v", err)
	}
	items := listFindings(t, c)
	if len(items) != 2 {
		t.Fatalf("findings = %v, want the two generations only", names(items))
	}
	second := familyName(integ, testSourceFinding(43), 2)
	if s := get(t, c, second); len(s.Spec.Alerts) != 1 || s.Spec.Alerts[0].ID != "43" {
		t.Errorf("successor alerts = %+v, want 43 alone (42 is held by the suspended generation)", s.Spec.Alerts)
	}
}

// A generation suspended between the family listing and the fold's read is
// treated as raced: the alert opens the successor rather than landing on the
// now-suspended finding.
func TestIngestFoldRacedSuspendOpensSuccessor(t *testing.T) {
	integ := testIntegration()
	first := familyName(integ, testSourceFinding(42), 1)
	racing := receiverClient(&interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if f, ok := obj.(*v1alpha1.Finding); ok && f.Name == first {
				f.Spec.Suspend = true
			}
			return nil
		},
	})
	in, _, _ := edgeIngestor(nil)
	in.Client = racing
	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("first Ingest() = %v", err)
	}
	if err := in.Ingest(t.Context(), integ, testSourceFinding(43)); err != nil {
		t.Fatalf("second Ingest() = %v", err)
	}
	if e := get(t, racing, first); len(e.Spec.Alerts) != 1 {
		t.Errorf("suspended alerts = %+v, want alert 43 not folded in", e.Spec.Alerts)
	}
	succ := get(t, racing, familyName(integ, testSourceFinding(43), 2))
	if len(succ.Spec.Alerts) != 1 || succ.Spec.Alerts[0].ID != "43" {
		t.Errorf("successor alerts = %+v, want alert 43", succ.Spec.Alerts)
	}
}
