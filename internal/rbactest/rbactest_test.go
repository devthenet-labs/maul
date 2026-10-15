// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package rbactest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// lagging is an authorizer that refuses each review until it has been asked
// about it `lag` times: the window between a RoleBinding landing in etcd and
// the RBAC informers observing it. Its scheme is empty on purpose: a review
// must not depend on the caller's client knowing the authorization types
// (the e2e suite's does not).
func lagging(lag int) (client.Client, map[string]int) {
	asked := map[string]int{}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok || u.GetKind() != "SubjectAccessReview" {
				return fmt.Errorf("only unstructured SubjectAccessReviews are expected, got %T", obj)
			}
			var sar authorizationv1.SubjectAccessReview
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &sar); err != nil {
				return err
			}
			key := describe(sar.Spec)
			asked[key]++
			return unstructured.SetNestedField(u.Object, asked[key] > lag, "status", "allowed")
		},
	}).Build()
	return c, asked
}

func reviews() []authorizationv1.SubjectAccessReviewSpec {
	return Verbs("alice@acme.test", nil, authorizationv1.ResourceAttributes{
		Namespace: "patchy", Group: "patchy.bitwisemedia.uk", Resource: "integrations",
	}, "get", "update", "backfill")
}

// TestWaitAllowedOutlastsInformerLag: every review is asked until it is
// allowed, so a caller never acts inside the window.
func TestWaitAllowedOutlastsInformerLag(t *testing.T) {
	c, asked := lagging(3)
	if err := waitAllowed(t.Context(), c, 5*time.Second, time.Millisecond, reviews()); err != nil {
		t.Fatalf("waitAllowed: %v", err)
	}
	for _, r := range reviews() {
		if got := asked[describe(r)]; got != 4 {
			t.Errorf("%s asked %d times, want 4 (three refusals, then allowed)", describe(r), got)
		}
	}
}

// TestWaitAllowedIsBounded: a grant that never holds fails, naming it,
// rather than hanging the suite.
func TestWaitAllowedIsBounded(t *testing.T) {
	c, _ := lagging(1 << 30)
	err := waitAllowed(t.Context(), c, 50*time.Millisecond, time.Millisecond, reviews())
	if err == nil {
		t.Fatal("waitAllowed returned nil for a review that is never allowed")
	}
	if !strings.Contains(err.Error(), `may get integrations.patchy.bitwisemedia.uk`) {
		t.Errorf("error does not name the refused review: %v", err)
	}
}

// TestWaitAllowedReturnsAtOnceWhenAllowed: no lag, one review each.
func TestWaitAllowedReturnsAtOnceWhenAllowed(t *testing.T) {
	c, asked := lagging(0)
	WaitAllowed(t, c, reviews()...)
	for _, r := range reviews() {
		if got := asked[describe(r)]; got != 1 {
			t.Errorf("%s asked %d times, want 1", describe(r), got)
		}
	}
}
