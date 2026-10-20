// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/internal/kube"
)

// TestPreviewAPIServerCIDRAutoAgainstAPIServer renders the preview-controller
// with previewController.config.apiServerCIDR auto against a real API server
// (helm's lookups need one) and holds its NetworkPolicy's API egress to
// exactly the default/kubernetes Service's cluster IP and the API server's
// own endpoint addresses, each a /32: the Service address alone matches
// nothing where the CNI judges the policy after translation (kube-router).
func TestPreviewAPIServerCIDRAutoAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{ingressClassParamsCRD()},
	}}
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
		t.Fatalf("client: %v", err)
	}
	h := newHelm(t, env)

	// The API server publishes its own Service and endpoints shortly after
	// it starts.
	svc := &corev1.Service{}
	slice := &discoveryv1.EndpointSlice{}
	key := client.ObjectKey{Namespace: "default", Name: "kubernetes"}
	deadline := time.Now().Add(time.Minute)
	for {
		errSvc := admin.Get(t.Context(), key, svc)
		errSlice := admin.Get(t.Context(), key, slice)
		if errSvc == nil && errSlice == nil && len(slice.Endpoints) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("default/kubernetes Service and EndpointSlice: %v, %v", errSvc, errSlice)
		}
		time.Sleep(200 * time.Millisecond)
	}
	want := []string{svc.Spec.ClusterIP + "/32"}
	for _, ep := range slice.Endpoints {
		for _, a := range ep.Addresses {
			if !slices.Contains(want, a+"/32") {
				want = append(want, a+"/32")
			}
		}
	}
	if len(want) < 2 {
		t.Fatalf("envtest's API server publishes no endpoint address beside %v", want)
	}
	slices.Sort(want)

	base := []string{"template", "maul", ".", "--namespace", "maul",
		"-f", fixtures + "preview-foundation.yaml", "-f", fixtures + "intent-controller.yaml",
		"-f", fixtures + "preview-controller.yaml", "--show-only", "templates/preview-controller.yaml"}
	if got := apiEgress(t, h, append(slices.Clone(base), "--dry-run=server",
		"--set", "previewController.config.apiServerCIDR=auto")); !slices.Equal(got, want) {
		t.Errorf("apiServerCIDR auto: API egress = %v, want %v", got, want)
	}
	// A named /32 is exactly that address, cluster or not.
	named := apiEgress(t, h, append(slices.Clone(base), "--dry-run=server"))
	if got := named; !slices.Equal(got, []string{"172.20.0.1/32"}) {
		t.Errorf("apiServerCIDR 172.20.0.1/32: API egress = %v, want [172.20.0.1/32]", got)
	}
}

// apiEgress renders with helm and returns the preview-controller
// NetworkPolicy's ipBlock CIDRs, sorted.
func apiEgress(t *testing.T, h helmRunner, args []string) []string {
	t.Helper()
	out, err := h.run(args...)
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args[:2], " "), err, out)
	}
	var cidrs []string
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(out), 4096)
	for {
		o := &unstructured.Unstructured{}
		if err := dec.Decode(o); err != nil {
			break
		}
		if o.GetKind() != "NetworkPolicy" || o.GetName() != "maul-preview-controller" {
			continue
		}
		egress, _, _ := unstructured.NestedSlice(o.Object, "spec", "egress")
		for _, rule := range egress {
			to, _, _ := unstructured.NestedSlice(rule.(map[string]any), "to")
			for _, peer := range to {
				if cidr, ok, _ := unstructured.NestedString(peer.(map[string]any), "ipBlock", "cidr"); ok {
					cidrs = append(cidrs, cidr)
				}
			}
		}
	}
	slices.Sort(cidrs)
	return cidrs
}
