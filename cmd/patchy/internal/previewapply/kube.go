// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// DefaultPoll is how often KubeCluster's waits read again.
const DefaultPoll = 5 * time.Second

// certificateGVK is cert-manager's Certificate.
var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// KubeCluster is the Cluster over a controller-runtime client, with the
// caller's own credentials.
type KubeCluster struct {
	Client client.Client
	// Poll is how often a wait reads again; 0 is DefaultPoll.
	Poll time.Duration
}

var _ Cluster = KubeCluster{}

// Apply server-side applies obj as previewsetup.ApplyFieldManager, forcing
// ownership of the fields it sets.
func (k KubeCluster) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	return k.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj.DeepCopy()),
		client.FieldOwner(previewsetup.ApplyFieldManager), client.ForceOwnership)
}

// Exists reads obj's metadata only.
func (k KubeCluster) Exists(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(obj.GroupVersionKind())
	return found(k.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, m))
}

// NamespaceExists reads the namespace.
func (k KubeCluster) NamespaceExists(ctx context.Context, name string) (bool, error) {
	return found(k.Client.Get(ctx, client.ObjectKey{Name: name}, &corev1.Namespace{}))
}

// EnsureNamespace creates the namespace unless it exists.
func (k KubeCluster) EnsureNamespace(ctx context.Context, name string) (bool, error) {
	err := k.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	switch {
	case apierrors.IsAlreadyExists(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("create namespace %s: %w", name, err)
	}
	return true, nil
}

// SecretExists reads the Secret's metadata only, never its data.
func (k KubeCluster) SecretExists(ctx context.Context, namespace, name string) (bool, error) {
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	return found(k.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, m))
}

// WaitCertificateReady polls the Certificate until its Ready condition is
// True for its current generation, or ctx ends.
func (k KubeCluster) WaitCertificateReady(ctx context.Context, namespace, name string) error {
	var last string
	err := wait.PollUntilContextCancel(ctx, k.poll(), true, func(ctx context.Context) (bool, error) {
		c := &unstructured.Unstructured{}
		c.SetGroupVersionKind(certificateGVK)
		err := k.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, c)
		if apierrors.IsNotFound(err) {
			last = "not found"
			return false, nil
		}
		if err != nil {
			return false, err
		}
		ready, why := certificateReady(c)
		last = why
		return ready, nil
	})
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("certificate %s/%s is not Ready (%s): see kubectl -n %s describe certificate %s, and its "+
			"challenges (kubectl -n %s get challenges)", namespace, name, last, namespace, name, namespace)
	}
	return err
}

// certificateReady reads a Certificate's Ready condition: true only for
// the generation cert-manager has observed, and why not otherwise.
func certificateReady(c *unstructured.Unstructured) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(c.Object, "status", "conditions")
	for _, raw := range conds {
		cond, ok := raw.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		if g, ok := cond["observedGeneration"].(int64); ok && g < c.GetGeneration() {
			return false, "Ready is for an earlier generation"
		}
		if cond["status"] == "True" {
			return true, ""
		}
		reason, _ := cond["reason"].(string)
		msg, _ := cond["message"].(string)
		return false, fmt.Sprintf("%s: %s", reason, msg)
	}
	return false, "no Ready condition yet"
}

// WaitDeploymentAvailable polls the Deployment until its current
// generation has rolled out, every replica updated and available, or ctx
// ends.
func (k KubeCluster) WaitDeploymentAvailable(ctx context.Context, namespace, name string) error {
	var last string
	err := wait.PollUntilContextCancel(ctx, k.poll(), true, func(ctx context.Context) (bool, error) {
		d := &appsv1.Deployment{}
		err := k.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, d)
		if apierrors.IsNotFound(err) {
			last = "not found"
			return false, nil
		}
		if err != nil {
			return false, err
		}
		ok, why := rolledOut(d)
		last = why
		return ok, nil
	})
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("deployment %s/%s has not rolled out (%s): see kubectl -n %s describe deployment %s",
			namespace, name, last, namespace, name)
	}
	return err
}

// rolledOut is `kubectl rollout status`'s test: the current generation
// observed, every replica updated and available.
func rolledOut(d *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	switch {
	case s.ObservedGeneration < d.Generation:
		return false, "the rollout has not been observed yet"
	case s.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d of %d replicas updated", s.UpdatedReplicas, want)
	case s.Replicas > s.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas pending termination", s.Replicas-s.UpdatedReplicas)
	case s.AvailableReplicas < s.UpdatedReplicas:
		return false, fmt.Sprintf("%d of %d updated replicas available", s.AvailableReplicas, s.UpdatedReplicas)
	}
	return true, ""
}

func (k KubeCluster) poll() time.Duration {
	if k.Poll > 0 {
		return k.Poll
	}
	return DefaultPoll
}

// found turns a Get's error into existence: a kind the API server does
// not serve yet (its CRD not installed) has no object either.
func found(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	}
	return false, err
}
