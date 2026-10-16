// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/bitwise-media-group/patchy/internal/provider"
)

// budgetReader reads a caller pod's provider.BudgetAnnotation under the cost
// limits: one get of the pod TokenReview bound the token to, in the agent
// namespace only (the chart's Role grants get on pods there and nothing
// else: no list, no watch). The result rides on the cached verdict, so it
// costs one read per token per verdict TTL.
type budgetReader struct {
	cs        kubernetes.Interface
	namespace string
}

// read returns the pod's budget in micro-USD, or 0 when there is none to
// honour: a token bound to no pod UID, a pod that is gone, a pod whose UID is
// not the token's (the name now belongs to another pod, whose budget is not
// this caller's), or an absent or malformed annotation. 0 falls back to
// --cost-per-pod, never to unlimited. A read the API server did not answer is
// an error, so the verdict is not cached and the request is refused as an
// unavailable review rather than served under a budget the broker did not
// see.
func (b *budgetReader) read(ctx context.Context, name, uid string) (int64, error) {
	if name == "" || uid == "" {
		return 0, nil
	}
	pod, err := b.cs.CoreV1().Pods(b.namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return 0, nil
	case err != nil:
		return 0, err
	case string(pod.UID) != uid:
		return 0, nil
	}
	budget, ok := provider.ParseBudget(pod.Annotations[provider.BudgetAnnotation])
	if !ok {
		return 0, nil
	}
	return budget, nil
}
