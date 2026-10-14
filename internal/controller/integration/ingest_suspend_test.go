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

// resume clears spec.suspend on the named finding, as a human would.
func resume(t *testing.T, c client.Client, name string) {
	t.Helper()
	f := get(t, c, name)
	f.Spec.Suspend = false
	if err := c.Update(t.Context(), f); err != nil {
		t.Fatalf("resume %s: %v", name, err)
	}
}

// alertIDs lists a finding's alert IDs in order.
func alertIDs(f *v1alpha1.Finding) []string {
	ids := make([]string, 0, len(f.Spec.Alerts))
	for _, a := range f.Spec.Alerts {
		ids = append(ids, a.ID)
	}
	return ids
}

// An elder resumed after its successor opened accepts alerts again beside
// it. The family's later alerts must land on the successor — the newest
// accepting generation — not on whichever the cache happens to list first,
// and a redelivery of an alert the successor holds must not be copied into
// the resumed elder.
func TestIngestResumedElderLeavesAlertsOnSuccessor(t *testing.T) {
	in, c := newIngestor(t)
	integ := testIntegration()
	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	first := familyName(integ, testSourceFinding(42), 1)
	suspend(t, c, first)
	if err := in.Ingest(t.Context(), integ, testSourceFinding(43)); err != nil {
		t.Fatalf("Ingest(43): %v", err)
	}
	second := familyName(integ, testSourceFinding(43), 2)
	resume(t, c, first)

	for _, alert := range []int{44, 43} {
		if err := in.Ingest(t.Context(), integ, testSourceFinding(alert)); err != nil {
			t.Fatalf("Ingest(%d): %v", alert, err)
		}
	}

	if ids := alertIDs(get(t, c, first)); len(ids) != 1 || ids[0] != "42" {
		t.Errorf("resumed elder alerts = %v, want 42 alone", ids)
	}
	if ids := alertIDs(get(t, c, second)); len(ids) != 2 || ids[0] != "43" || ids[1] != "44" {
		t.Errorf("successor alerts = %v, want 43 and 44", ids)
	}
	if n := len(listFindings(t, c)); n != 2 {
		t.Errorf("findings = %d, want 2 generations", n)
	}
}

// A raced fold whose successor generation already exists and is itself
// suspended opens the generation after it, rather than failing the delivery
// with the nested fold's race.
func TestIngestRacedFoldPastSuspendedSuccessorOpensNext(t *testing.T) {
	integ := testIntegration()
	first := familyName(integ, testSourceFinding(42), 1)
	racing := false
	cl := receiverClient(&interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if f, ok := obj.(*v1alpha1.Finding); ok && racing && f.Name == first {
				f.Spec.Suspend = true
			}
			return nil
		},
	})
	in, _, _ := edgeIngestor(nil)
	in.Client = cl
	if err := in.Ingest(t.Context(), integ, testSourceFinding(42)); err != nil {
		t.Fatalf("Ingest(42): %v", err)
	}
	suspend(t, cl, first)
	if err := in.Ingest(t.Context(), integ, testSourceFinding(43)); err != nil {
		t.Fatalf("Ingest(43): %v", err)
	}
	second := familyName(integ, testSourceFinding(43), 2)
	resume(t, cl, first)
	suspend(t, cl, second)

	// The family listing sees the elder accepting; its fold then reads it
	// suspended, and generation 2 (suspended) already exists.
	racing = true
	if err := in.Ingest(t.Context(), integ, testSourceFinding(44)); err != nil {
		t.Fatalf("Ingest(44) = %v, want a successor opened", err)
	}
	racing = false

	third := get(t, cl, familyName(integ, testSourceFinding(44), 3))
	if ids := alertIDs(third); len(ids) != 1 || ids[0] != "44" {
		t.Errorf("generation 3 alerts = %v, want 44", ids)
	}
	if len(third.Spec.Related) != 1 || third.Spec.Related[0].To != second {
		t.Errorf("generation 3 related = %+v, want successor-of %s", third.Spec.Related, second)
	}
	for _, name := range []string{first, second} {
		for _, id := range alertIDs(get(t, cl, name)) {
			if id == "44" {
				t.Errorf("%s holds alert 44, want it only on generation 3", name)
			}
		}
	}
}
