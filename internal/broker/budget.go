// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/provider"
)

// podReader reads what the broker needs to know of a caller pod: one get of
// the pod TokenReview bound the token to, in the agent namespace only (the
// chart's Roles grant get on pods there and nothing else: no list, no
// watch). It reads the provider.BudgetAnnotation under the cost limits and
// the attributed-to annotation under per-user credentials. The result rides
// on the cached verdict, so it costs one read per token per verdict TTL.
type podReader struct {
	cs        kubernetes.Interface
	namespace string
	// budget and attribution say which annotations to read.
	budget, attribution bool
}

// podFacts is what one read found.
type podFacts struct {
	// verified is whether the token's pod was read and is the pod the token
	// is bound to: a name and UID were reported and the pod by that name
	// has that UID.
	verified bool
	// budget is the pod's budget in micro-USD; 0 when there is none to
	// honour (or it was not read).
	budget int64
	// attributedTo is the pod's attributed-to annotation as written, ""
	// when absent (or not read). Only a verified pod's is ever set.
	attributedTo string
}

// read returns the facts of the pod named name with UID uid. A token bound
// to no pod UID, a pod that is gone, or a pod whose UID is not the token's
// (the name now belongs to another pod, whose annotations are not this
// caller's) is unverified: no budget (0 falls back to --cost-per-pod, never
// to unlimited) and no attribution (which per-user credentials under
// enforce refuse). An absent or malformed budget is 0. A read the API
// server did not answer is an error, so the verdict is not cached and the
// request is refused as an unavailable review rather than served under
// facts the broker did not see.
func (p *podReader) read(ctx context.Context, name, uid string) (podFacts, error) {
	if name == "" || uid == "" {
		return podFacts{}, nil
	}
	pod, err := p.cs.CoreV1().Pods(p.namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return podFacts{}, nil
	case err != nil:
		return podFacts{}, err
	case string(pod.UID) != uid:
		return podFacts{}, nil
	}
	facts := podFacts{verified: true}
	if p.budget {
		if budget, ok := provider.ParseBudget(pod.Annotations[provider.BudgetAnnotation]); ok {
			facts.budget = budget
		}
	}
	if p.attribution {
		facts.attributedTo = pod.Annotations[v1alpha1.AnnotationAttributedTo]
	}
	return facts, nil
}
