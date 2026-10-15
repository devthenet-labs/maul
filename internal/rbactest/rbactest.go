// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package rbactest

import (
	"context"
	"fmt"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// timeout bounds the wait: the authorizer's informers catch up in
	// milliseconds, so thirty seconds is only ever reached by a grant that
	// does not hold at all.
	timeout = 30 * time.Second
	// interval spaces the reviews.
	interval = 50 * time.Millisecond
)

// Review is the SubjectAccessReview spec asking whether user, in groups,
// may make the request attrs describes.
func Review(user string, groups []string,
	attrs authorizationv1.ResourceAttributes,
) authorizationv1.SubjectAccessReviewSpec {
	a := attrs
	return authorizationv1.SubjectAccessReviewSpec{User: user, Groups: groups, ResourceAttributes: &a}
}

// Verbs is one Review per verb, the other attributes shared.
func Verbs(user string, groups []string, attrs authorizationv1.ResourceAttributes,
	verbs ...string,
) []authorizationv1.SubjectAccessReviewSpec {
	out := make([]authorizationv1.SubjectAccessReviewSpec, 0, len(verbs))
	for _, v := range verbs {
		a := attrs
		a.Verb = v
		out = append(out, Review(user, groups, a))
	}
	return out
}

// WaitAllowed blocks until the API server's authorizer allows every review,
// failing t if one is still refused after the bounded timeout. c must be
// allowed to create SubjectAccessReviews (an envtest admin client is).
func WaitAllowed(t testing.TB, c client.Client, reviews ...authorizationv1.SubjectAccessReviewSpec) {
	t.Helper()
	if err := waitAllowed(t.Context(), c, timeout, interval, reviews); err != nil {
		t.Fatal(err)
	}
}

// waitAllowed is WaitAllowed with the clock exposed, so its own tests can
// prove both the wait and the timeout without a real API server.
func waitAllowed(ctx context.Context, c client.Client, timeout, interval time.Duration,
	reviews []authorizationv1.SubjectAccessReviewSpec,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, spec := range reviews {
		for {
			allowed, reason, err := review(ctx, c, spec)
			if err == nil && allowed {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("RBAC grant never took effect within %s: %s (last error %v, reason %q)",
					timeout, describe(spec), err, reason)
			}
			select {
			case <-ctx.Done():
			case <-time.After(interval):
			}
		}
	}
	return nil
}

// review asks the API server one SubjectAccessReview. It goes through
// unstructured so it works with any client, whatever its scheme holds: the
// e2e suite's client registers no authorization types.
func review(ctx context.Context, c client.Client, spec authorizationv1.SubjectAccessReviewSpec,
) (allowed bool, reason string, err error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&authorizationv1.SubjectAccessReview{Spec: spec})
	if err != nil {
		return false, "", err
	}
	sar := &unstructured.Unstructured{Object: raw}
	sar.SetGroupVersionKind(authorizationv1.SchemeGroupVersion.WithKind("SubjectAccessReview"))
	if err := c.Create(ctx, sar); err != nil {
		return false, "", err
	}
	allowed, _, _ = unstructured.NestedBool(sar.Object, "status", "allowed")
	reason, _, _ = unstructured.NestedString(sar.Object, "status", "reason")
	return allowed, reason, nil
}

// describe renders a review for a failure message.
func describe(spec authorizationv1.SubjectAccessReviewSpec) string {
	a := spec.ResourceAttributes
	if a == nil {
		return fmt.Sprintf("user %q groups %v", spec.User, spec.Groups)
	}
	res := a.Resource
	if a.Subresource != "" {
		res += "/" + a.Subresource
	}
	return fmt.Sprintf("user %q groups %v may %s %s.%s %q in namespace %q",
		spec.User, spec.Groups, a.Verb, res, a.Group, a.Name, a.Namespace)
}
