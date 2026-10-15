// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// The status server stamps spec.backfill/replay/reset on Integrations Helm
// applies. Each stamp must take ownership of its own field only: a full
// Update re-serialised the spec ("10m" reads back as "10m0s"), took the
// operator's durations and made the next helm upgrade conflict.

const serverManager = "patchy-status-server"

func TestIntegrationStampsLeaveHelmFieldsToHelm(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping ownership envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build admin client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "patchy"}}
	if err := admin.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	srvCfg := rest.CopyConfig(cfg)
	srvCfg.UserAgent = serverManager
	srvClient, err := client.New(srvCfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build server client: %v", err)
	}
	s := NewServer(srvClient, "patchy", stubAuth{id: operator}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }

	helm := func() error {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "Integration",
			"metadata":   map[string]any{"name": "gh", "namespace": "patchy"},
			"spec": map[string]any{
				"provider":  "github",
				"interval":  "10m",
				"secretRef": map[string]any{"name": "github-app"},
				"github": map[string]any{
					"codeScanningAlerts": map[string]any{"enabled": true},
					"redelivery":         map[string]any{"enabled": true, "lookback": "24h"},
				},
			},
		}}
		return admin.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("helm"))
	}
	if err := helm(); err != nil {
		t.Fatalf("first helm apply: %v", err)
	}

	res, body := postBackfill(t, s, "gh", `{"repositories":["acme/"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("backfill status = %d (%s)", res.StatusCode, body)
	}
	if err := s.requestReplay(t.Context(), *operator); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := s.resetAll(t.Context(), *operator); err != nil {
		t.Fatalf("reset: %v", err)
	}

	var integ v1alpha1.Integration
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "patchy", Name: "gh"}, &integ); err != nil {
		t.Fatalf("get integration: %v", err)
	}
	if integ.Spec.Backfill == nil || integ.Spec.Replay == nil || integ.Spec.Reset == nil {
		t.Fatalf("stamps missing: backfill %v replay %v reset %v",
			integ.Spec.Backfill, integ.Spec.Replay, integ.Spec.Reset)
	}
	want := []string{"backfill", "replay", "reset"}
	if got := ownedSpecFields(t, &integ, serverManager); !slices.Equal(got, want) {
		t.Errorf("status server owns spec fields %v, want only %v", got, want)
	}
	if err := helm(); err != nil {
		t.Fatalf("helm re-apply after the status server's stamps conflicted: %v", err)
	}
}

// ownedSpecFields reports the top-level spec fields manager owns on obj.
func ownedSpecFields(t *testing.T, obj metav1.Object, manager string) []string {
	t.Helper()
	var out []string
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != manager || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]map[string]any
		if err := json.Unmarshal(mf.FieldsV1.GetRawBytes(), &fields); err != nil {
			t.Fatalf("decode managed fields of %s: %v", manager, err)
		}
		for k := range fields["f:spec"] {
			if name, ok := strings.CutPrefix(k, "f:"); ok {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}
