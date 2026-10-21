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

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
)

// The status server stamps spec.backfill/replay/reset on Integrations Helm
// applies. Each stamp must take ownership of its own field only: a full
// Update re-serialised the spec ("10m" reads back as "10m0s"), took the
// operator's durations and made the next helm upgrade conflict.

const serverManager = "maul-status-server"

// startOwnershipEnv starts an API server with the CRDs and returns an admin
// client and a Server whose writes land under the status server's field
// manager.
func startOwnershipEnv(t *testing.T) (client.Client, *Server) {
	t.Helper()
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maul"}}
	if err := admin.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	srvCfg := rest.CopyConfig(cfg)
	srvCfg.UserAgent = serverManager
	srvClient, err := client.New(srvCfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build server client: %v", err)
	}
	s := NewServer(srvClient, "maul", stubAuth{id: operator}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }
	return admin, s
}

func TestIntegrationStampsLeaveHelmFieldsToHelm(t *testing.T) {
	admin, s := startOwnershipEnv(t)

	helm := func() error {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "Integration",
			"metadata":   map[string]any{"name": "gh", "namespace": "maul"},
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
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "maul", Name: "gh"}, &integ); err != nil {
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

// TestFindingActionsLeaveControllerFieldsAlone covers the action handler's
// write. The Finding is applied by a controller with an approval timestamp
// in fractional seconds, which the API server stores as written and
// metav1.Time drops on a round trip: a full Update would change that value
// and take spec.approval from the controller, whose next apply conflicts.
func TestFindingActionsLeaveControllerFieldsAlone(t *testing.T) {
	admin, s := startOwnershipEnv(t)

	const controller = "integration-controller"
	apply := func() error {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "Finding",
			"metadata":   map[string]any{"name": "fnd-1", "namespace": "maul"},
			"spec": map[string]any{
				"integrationRef": map[string]any{"name": "gh"},
				"source":         "github-code-scanning",
				"advisories":     []any{"CVE-2026-0001"},
				"approval":       map[string]any{"by": "octocat", "at": "2026-07-24T09:00:00.250Z"},
			},
		}}
		return admin.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(controller))
	}
	if err := apply(); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	key := client.ObjectKey{Namespace: "maul", Name: "fnd-1"}
	var fnd v1alpha1.Finding
	if err := admin.Get(t.Context(), key, &fnd); err != nil {
		t.Fatalf("get finding: %v", err)
	}
	fnd.Status.Phase = v1alpha1.PhaseQueued
	if err := admin.Status().Update(t.Context(), &fnd); err != nil {
		t.Fatalf("seed finding status: %v", err)
	}

	for _, verb := range []string{"suspend", "resume", "expedite"} {
		if res, body := postAction(t, s, "fnd-1", verb); res.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d (%s)", verb, res.StatusCode, body)
		}
	}

	var got v1alpha1.Finding
	if err := admin.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if got.Spec.Expedite == nil || got.Spec.Suspend {
		t.Fatalf("spec = expedite %+v suspend %v, want expedited and resumed", got.Spec.Expedite, got.Spec.Suspend)
	}
	if fields, want := ownedSpecFields(t, &got, serverManager), []string{"expedite"}; !slices.Equal(fields, want) {
		t.Errorf("status server owns spec fields %v, want only %v", fields, want)
	}
	if err := apply(); err != nil {
		t.Fatalf("controller re-apply after the status server's actions conflicted: %v", err)
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
