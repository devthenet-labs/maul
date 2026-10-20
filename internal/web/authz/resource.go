// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package authz

import (
	"context"
	"fmt"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devthenet-labs/maul/internal/web/auth"
)

// ResourceReviewer answers "may this identity <verb> <resource>?" through
// SubjectAccessReviews, cached briefly per identity+verb. It is the
// resource-parameterized sibling of Reviewer (which is findings-specific and
// grant-shaped); the evaluation API uses it with native verbs only —
// create/get/delete on evaluations — so RBAC is the whole authorization
// surface, no admission-policy change involved.
type ResourceReviewer struct {
	client    client.Client
	namespace string
	group     string
	resource  string
	// subresource is the (virtual) subresource reviewed, "" for none.
	subresource string
	ttl         time.Duration
	now         func() time.Time

	mu    sync.Mutex
	cache map[string]resourceCached
}

type resourceCached struct {
	allowed bool
	expires time.Time
}

// NewResourceReviewer builds a ResourceReviewer for one resource in one
// namespace. ttl <= 0 uses the package default.
func NewResourceReviewer(c client.Client, namespace, group, resource string, ttl time.Duration) *ResourceReviewer {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &ResourceReviewer{
		client: c, namespace: namespace, group: group, resource: resource,
		ttl: ttl, now: time.Now, cache: make(map[string]resourceCached),
	}
}

// NewSubresourceReviewer is NewResourceReviewer for one subresource of the
// resource, such as the virtual usage/all the status page's admin usage view
// is gated on: nothing on the API server serves it, so RBAC granting it is
// read only by these reviews.
func NewSubresourceReviewer(c client.Client, namespace, group, resource, subresource string,
	ttl time.Duration) *ResourceReviewer {
	r := NewResourceReviewer(c, namespace, group, resource, ttl)
	r.subresource = subresource
	return r
}

// Allowed reports whether the identity may verb the resource.
func (r *ResourceReviewer) Allowed(ctx context.Context, id auth.Identity, verb string) (bool, error) {
	key := cacheKey(id) + "\x00" + verb
	r.mu.Lock()
	if hit, ok := r.cache[key]; ok && r.now().Before(hit.expires) {
		r.mu.Unlock()
		return hit.allowed, nil
	}
	r.mu.Unlock()

	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   id.Username,
			Groups: id.Groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   r.namespace,
				Group:       r.group,
				Resource:    r.resource,
				Subresource: r.subresource,
				Verb:        verb,
			},
		},
	}
	if err := r.client.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("access review %s %s for %s: %w", verb, r.resource, id.Username, err)
	}

	r.mu.Lock()
	if len(r.cache) >= cacheLimit {
		r.cache = make(map[string]resourceCached) // reset rather than evict piecemeal
	}
	r.cache[key] = resourceCached{allowed: sar.Status.Allowed, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return sar.Status.Allowed, nil
}
