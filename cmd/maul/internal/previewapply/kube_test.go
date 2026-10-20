// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fakeKube(t *testing.T, objs ...client.Object) KubeCluster {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return KubeCluster{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Poll: 10 * time.Millisecond}
}

func TestKubeNamespaces(t *testing.T) {
	k := fakeKube(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maul"}})
	ctx := t.Context()
	if ok, err := k.NamespaceExists(ctx, "maul"); !ok || err != nil {
		t.Errorf("NamespaceExists(maul) = %v, %v", ok, err)
	}
	if ok, err := k.NamespaceExists(ctx, "dex"); ok || err != nil {
		t.Errorf("NamespaceExists(dex) = %v, %v", ok, err)
	}
	if created, err := k.EnsureNamespace(ctx, "tailscale"); !created || err != nil {
		t.Errorf("EnsureNamespace = %v, %v", created, err)
	}
	if created, err := k.EnsureNamespace(ctx, "tailscale"); created || err != nil {
		t.Errorf("EnsureNamespace again = %v, %v", created, err)
	}
}

// TestKubeSecretExistsMetadataOnly reads a Secret's existence; the fake
// serves PartialObjectMetadata, which carries no data.
func TestKubeSecretExistsMetadataOnly(t *testing.T) {
	k := fakeKube(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "tailscale", Name: "operator-oauth"},
		Data: map[string][]byte{"client_secret": []byte("x")}})
	if ok, err := k.SecretExists(t.Context(), "tailscale", "operator-oauth"); !ok || err != nil {
		t.Errorf("SecretExists = %v, %v", ok, err)
	}
	if ok, err := k.SecretExists(t.Context(), "tailscale", "other"); ok || err != nil {
		t.Errorf("SecretExists(other) = %v, %v", ok, err)
	}
	if ok, err := k.Exists(t.Context(), obj("v1", "Secret", "tailscale", "operator-oauth")); !ok || err != nil {
		t.Errorf("Exists = %v, %v", ok, err)
	}
}

// TestKubeApply server-side applies a Secret, then again (idempotent).
func TestKubeApply(t *testing.T) {
	k := fakeKube(t)
	o := obj("v1", "ConfigMap", "maul", "c")
	o.Object["data"] = map[string]any{"a": "1"}
	for range 2 {
		if err := k.Apply(t.Context(), o); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	var got corev1.ConfigMap
	if err := k.Client.Get(t.Context(), client.ObjectKey{Namespace: "maul", Name: "c"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data["a"] != "1" {
		t.Errorf("data %v", got.Data)
	}
}

func certificate(gen int64, conds ...map[string]any) *unstructured.Unstructured {
	c := obj("cert-manager.io/v1", "Certificate", "maul", "preview-wildcard")
	c.SetGeneration(gen)
	list := make([]any, len(conds))
	for i, cond := range conds {
		list[i] = cond
	}
	c.Object["status"] = map[string]any{"conditions": list}
	return c
}

func TestCertificateReady(t *testing.T) {
	for name, tc := range map[string]struct {
		c    *unstructured.Unstructured
		want bool
		why  string
	}{
		"ready": {certificate(1, map[string]any{"type": "Ready", "status": "True",
			"observedGeneration": int64(1)}), true, ""},
		"no status": {certificate(1), false, "no Ready condition"},
		"pending": {certificate(1, map[string]any{"type": "Ready", "status": "False", "reason": "Issuing",
			"message": "waiting for DNS-01"}), false, "Issuing: waiting for DNS-01"},
		"stale": {certificate(2, map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}),
			false, "earlier generation"},
	} {
		ok, why := certificateReady(tc.c)
		if ok != tc.want || !strings.Contains(why, tc.why) {
			t.Errorf("%s: %v, %q", name, ok, why)
		}
	}
}

// TestKubeWaitCertificateTimeout: a Certificate never Ready ends with its
// reason and where to look.
func TestKubeWaitCertificateTimeout(t *testing.T) {
	k := fakeKube(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := k.WaitCertificateReady(ctx, "maul", "preview-wildcard")
	if err == nil || !strings.Contains(err.Error(), "describe certificate preview-wildcard") {
		t.Fatalf("WaitCertificateReady = %v", err)
	}
}

func TestRolledOut(t *testing.T) {
	d := func(gen, observed int64, replicas, updated, total, available int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec: appsv1.DeploymentSpec{Replicas: &replicas},
			Status: appsv1.DeploymentStatus{ObservedGeneration: observed, UpdatedReplicas: updated, Replicas: total,
				AvailableReplicas: available}}
	}
	for name, tc := range map[string]struct {
		d    *appsv1.Deployment
		want bool
	}{
		"done":        {d(2, 2, 2, 2, 2, 2), true},
		"unobserved":  {d(2, 1, 2, 2, 2, 2), false},
		"updating":    {d(2, 2, 2, 1, 2, 2), false},
		"old pending": {d(2, 2, 2, 2, 3, 2), false},
		"unavailable": {d(2, 2, 2, 2, 2, 1), false},
	} {
		if ok, _ := rolledOut(tc.d); ok != tc.want {
			t.Errorf("%s: %v", name, ok)
		}
	}
	k := fakeKube(t, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "cf", Name: "cloudflared",
		Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, Replicas: 1,
		AvailableReplicas: 1}})
	if err := k.WaitDeploymentAvailable(t.Context(), "cf", "cloudflared"); err != nil {
		t.Errorf("WaitDeploymentAvailable: %v", err)
	}
}
