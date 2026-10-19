// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestKubeClusterEnvtest runs KubeCluster against a real API server: a
// generated Secret applied over one a hand-run apply created with other
// data (forced ownership, as --apply's re-runs after a manual install
// need), applied again unchanged, read back as metadata only, and a kind
// whose CRD is not installed reported as absent rather than an error.
func TestKubeClusterEnvtest(t *testing.T) {
	c := startEnvtest(t)
	k := KubeCluster{Client: c}
	ctx := t.Context()
	if created, err := k.EnsureNamespace(ctx, "patchy"); !created || err != nil {
		t.Fatalf("EnsureNamespace = %v, %v", created, err)
	}
	data, err := os.ReadFile(filepath.Join("..", "previewsetup", "testdata", "golden", "existing-alb-eks",
		filepath.FromSlash(previewsetup.RelayDexSecretFile)))
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Objects(data)
	if err != nil || len(objs) != 1 {
		t.Fatalf("Objects = %v, %v", objs, err)
	}
	secret := objs[0]
	if ok, err := k.Exists(ctx, secret); ok || err != nil {
		t.Fatalf("Exists before = %v, %v", ok, err)
	}
	hand := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "patchy", Name: secret.GetName()},
		Data: map[string][]byte{previewsetup.RelayDexSecretKey: []byte("an older secret")}}
	if err := c.Create(ctx, hand, client.FieldOwner("kubectl-client-side-apply")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := k.Apply(ctx, secret); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	checkManagedBy(t, c, secret.GetName())
	if ok, err := k.SecretExists(ctx, "patchy", secret.GetName()); !ok || err != nil {
		t.Errorf("SecretExists = %v, %v", ok, err)
	}
	cert := obj("cert-manager.io/v1", "Certificate", "patchy", "preview-wildcard")
	if ok, err := k.Exists(ctx, cert); ok || err != nil {
		t.Errorf("Exists(Certificate without its CRD) = %v, %v", ok, err)
	}
}

// startEnvtest boots a kube-apiserver and returns an admin client.
func startEnvtest(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping the previewapply envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// checkManagedBy: the Secret holds the file's value, and --apply's field
// manager owns it.
func checkManagedBy(t *testing.T, c client.Client, name string) {
	t.Helper()
	var got corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "patchy", Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data[previewsetup.RelayDexSecretKey]) != "SENTINEL-not-a-secret-0123456789abcdef" {
		t.Error("the applied Secret does not hold the file's value")
	}
	managers := map[string]bool{}
	for _, m := range got.ManagedFields {
		managers[m.Manager] = true
	}
	if !managers[previewsetup.ApplyFieldManager] {
		t.Errorf("managers %v", managers)
	}
}
