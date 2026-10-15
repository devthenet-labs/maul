// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/kube"
)

// TestEgressRegistriesAgainstAPIServer server-side applies, as a dry run,
// every object the chart renders for the registries fixture (and, for the
// Pod Identity rule, its irsa variant) to a real API server, which
// validates what a render cannot: the projected credential and CA volumes
// (defaultMode, items, no subPath), the internal registry's ipBlock and
// namespace/pod-selector peers, and the Pod Identity /32. The
// CiliumNetworkPolicy, rendered only under mode cilium, has no CRD in
// envtest; the render test's assertions hold its shape.
func TestEgressRegistriesAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
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
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	for _, variant := range []struct {
		name string
		args []string
	}{
		{"pod identity", egressRegistryArgs},
		{"irsa", append(append([]string{}, egressRegistryArgs...), "--set", "egressProxy.cloudCredentials=irsa",
			"--set-json",
			`egressProxy.serviceAccount.annotations={"eks.amazonaws.com/role-arn":"arn:aws:iam::377946145366:role/p"}`)},
	} {
		t.Run(variant.name, func(t *testing.T) {
			args := append([]string{"template", "patchy", ".", "--namespace", egressRelease}, variant.args...)
			output, err := exec.CommandContext(ctx, "helm", args...).Output()
			if err != nil {
				t.Fatalf("render chart: %v", err)
			}
			dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
			var namespaces, others []*unstructured.Unstructured
			for {
				u := &unstructured.Unstructured{}
				if err := dec.Decode(u); err != nil {
					break
				}
				switch u.GetKind() {
				case "":
				case "CiliumNetworkPolicy":
					t.Errorf("mode none rendered CiliumNetworkPolicy %s", u.GetName())
				case "Namespace":
					namespaces = append(namespaces, u)
				default:
					others = append(others, u)
				}
			}
			for _, ns := range namespaces {
				if err := admin.Create(ctx, ns); err != nil && !strings.Contains(err.Error(), "already exists") {
					t.Fatalf("namespace %s: %v", ns.GetName(), err)
				}
			}
			applied := map[string]bool{}
			for _, u := range others {
				err := admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.DryRunAll,
					client.FieldOwner("chart-test"), client.ForceOwnership)
				if err != nil {
					t.Errorf("the API server refused %s %s/%s: %v", u.GetKind(), u.GetNamespace(), u.GetName(), err)
				}
				applied[u.GetKind()+"/"+u.GetName()] = true
			}
			for _, want := range []string{
				"Deployment/patchy-egress-proxy", "NetworkPolicy/patchy-egress-proxy",
				"ConfigMap/patchy-egress-proxy-config", "NetworkPolicy/patchy-agents-network-web",
				"NetworkPolicy/patchy-agents-network-ca",
			} {
				if !applied[want] {
					t.Errorf("the registries render has no %s", want)
				}
			}
		})
	}
}
