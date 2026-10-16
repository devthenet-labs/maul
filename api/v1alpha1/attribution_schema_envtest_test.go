// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchyv1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// testAttributionSchema: spec.attributedTo carries a user exactly when its
// reason names a person, the user is a GitHub account by numeric id, and the
// stamp is as immutable as the rest of each run's spec.
func testAttributionSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	n := 0
	runs := func(a *patchyv1.Attribution) []client.Object {
		n++
		meta := func(kind string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: fmt.Sprintf("attr-%d-%s", n, kind), Namespace: "default"}
		}
		return []client.Object{
			&patchyv1.Investigation{ObjectMeta: meta("inv"), Spec: patchyv1.InvestigationSpec{
				FindingRef: patchyv1.ObjectReference{Name: "attr"}, Attempt: 1, AttributedTo: a.DeepCopy(),
			}},
			&patchyv1.Remediation{ObjectMeta: meta("rem"), Spec: patchyv1.RemediationSpec{
				FindingRef:       patchyv1.ObjectReference{Name: "attr"},
				InvestigationRef: patchyv1.ObjectReference{Name: "attr-inv-1"},
				RepositoryRef:    patchyv1.LocalObjectReference{Name: "attr-src"},
				Attempt:          1, AttributedTo: a.DeepCopy(),
			}},
			&patchyv1.EvaluationUnit{ObjectMeta: meta("unit"), Spec: patchyv1.EvaluationUnitSpec{
				EvaluationRef: patchyv1.ObjectReference{Name: "attr"}, Unit: evalUnitPlan(), AttributedTo: a.DeepCopy(),
			}},
		}
	}
	person := &patchyv1.Attribution{
		User: &patchyv1.UserRef{Login: "alice", ID: 42}, Reason: patchyv1.AttributionApprover,
	}
	for _, a := range []*patchyv1.Attribution{
		person,
		{Reason: patchyv1.AttributionOrg},
		{Reason: patchyv1.AttributionApproverUnregistered},
		{Reason: patchyv1.AttributionSubmitterUnregistered},
		{User: &patchyv1.UserRef{Login: "b0b", ID: 1}, Reason: patchyv1.AttributionReviewer},
	} {
		for _, obj := range runs(a) {
			if err := c.Create(ctx, obj); err != nil {
				t.Errorf("Create(%T attributed %+v) = %v, want nil", obj, *a, err)
				continue
			}
			got := obj.DeepCopyObject().(client.Object)
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
				t.Fatalf("Get(%T) = %v", obj, err)
			}
			if stamp := attributedTo(got); !reflect.DeepEqual(stamp, a) {
				t.Errorf("%T attributedTo round-tripped as %+v, want %+v", obj, stamp, a)
			}
		}
	}
	for _, a := range []*patchyv1.Attribution{
		{Reason: patchyv1.AttributionApprover},
		{User: &patchyv1.UserRef{Login: "alice", ID: 42}, Reason: patchyv1.AttributionOrg},
		{User: &patchyv1.UserRef{Login: "alice", ID: 42}, Reason: patchyv1.AttributionApproverUnregistered},
		{User: &patchyv1.UserRef{Login: "alice"}, Reason: patchyv1.AttributionApprover},
		{User: &patchyv1.UserRef{Login: "patchy[bot]", ID: 3}, Reason: patchyv1.AttributionCommenter},
		{User: &patchyv1.UserRef{Login: "github:alice", ID: 3}, Reason: patchyv1.AttributionApprover},
		{User: &patchyv1.UserRef{ID: 3}, Reason: patchyv1.AttributionApprover},
		{Reason: "enforce"},
	} {
		for _, obj := range runs(a) {
			if err := c.Create(ctx, obj); err == nil {
				t.Errorf("Create(%T attributed %+v) = nil, want rejection", obj, *a)
			}
		}
	}

	// Immutable: neither changing nor adding nor clearing the stamp passes.
	inv := &patchyv1.Investigation{}
	if err := c.Get(ctx, client.ObjectKey{Name: "attr-1-inv", Namespace: "default"}, inv); err != nil {
		t.Fatalf("Get(investigation) = %v", err)
	}
	unstamped := &patchyv1.Investigation{
		ObjectMeta: metav1.ObjectMeta{Name: "attr-unstamped-inv", Namespace: "default"},
		Spec:       patchyv1.InvestigationSpec{FindingRef: patchyv1.ObjectReference{Name: "attr"}, Attempt: 1},
	}
	if err := c.Create(ctx, unstamped); err != nil {
		t.Fatalf("Create(unstamped investigation) = %v", err)
	}
	for name, change := range map[string]func(){
		"another person": func() {
			inv.Spec.AttributedTo = &patchyv1.Attribution{
				User: &patchyv1.UserRef{Login: "mallory", ID: 666}, Reason: patchyv1.AttributionApprover,
			}
		},
		"cleared": func() { inv.Spec.AttributedTo = nil },
		"added":   func() { unstamped.Spec.AttributedTo = person.DeepCopy() },
	} {
		target := client.Object(inv)
		if name == "added" {
			target = unstamped
		}
		before := target.DeepCopyObject().(client.Object)
		change()
		if err := c.Update(ctx, target); err == nil {
			t.Errorf("Update(attributedTo %s) = nil, want the immutability rejection", name)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(before), target); err != nil {
			t.Fatalf("re-Get = %v", err)
		}
	}
}

func attributedTo(obj client.Object) *patchyv1.Attribution {
	switch o := obj.(type) {
	case *patchyv1.Investigation:
		return o.Spec.AttributedTo
	case *patchyv1.Remediation:
		return o.Spec.AttributedTo
	case *patchyv1.EvaluationUnit:
		return o.Spec.AttributedTo
	}
	return nil
}
