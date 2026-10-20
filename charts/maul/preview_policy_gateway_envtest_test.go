// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayArgs turns the preview gateway on over the preview fixture, with
// the legacy slot Ingress off: the shape release N moves an install to.
var gatewayArgs = []string{
	"-f", "../../hack/testdata/chart-render/preview-auth.yaml",
	"-f", "../../hack/testdata/chart-render/preview-gateway.yaml",
	"--set", "preview.legacyIngress=false",
}

// gatewayLabel is the one label the slot NetworkPolicy admits the gateway by.
const gatewayLabel = "maul.run/preview-gateway"

// TestPreviewAdmissionWithGatewayAgainstAPIServer: turning the preview
// gateway on (and the legacy slot Ingress off) leaves every slot admission
// rule as it was: the same denials, the same admitted shapes, and Secrets
// still out of reach of slot workloads.
func TestPreviewAdmissionWithGatewayAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	_, admin := startPreviewPolicyEnv(t, gatewayArgs...)
	testPreviewDenials(t, admin)
	testPreviewAllowed(t, admin)
	testMultiComponentAdmission(t, admin)
	testSecretRefs(t, admin)
}

// TestPreviewGatewayNetworkPoliciesAgainstAPIServer: the API server stores
// the rendered slot and gateway NetworkPolicies as the chart means them.
// envtest has no dataplane, so this pins the stored shape (the isolation
// probe checks packets on a real cluster): slot Pods admit only port http,
// from Pods that are BOTH in the release namespace and carry the gateway
// label (one peer, never two), and egress only DNS; the gateway reaches the
// slots on port http alone. With the legacy slot Ingress off, the ALB's
// subnets are no peer although the fixture keeps the ALB values set.
func TestPreviewGatewayNetworkPoliciesAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	_, admin := startPreviewPolicyEnv(t, gatewayArgs...)
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maul"}}); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	args := append([]string{"template", "maul", ".", "--namespace", "maul",
		"-f", "../../hack/testdata/chart-render/preview-foundation.yaml"}, gatewayArgs...)
	output, err := exec.CommandContext(ctx, "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	applied := 0
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() != "NetworkPolicy" || (u.GetName() != "preview-isolation" && u.GetName() != "maul-preview-auth") {
			continue
		}
		if err := admin.Create(ctx, u); err != nil {
			t.Fatalf("API server refuses NetworkPolicy %s/%s: %v", u.GetNamespace(), u.GetName(), err)
		}
		applied++
	}
	if applied != 3 {
		t.Fatalf("applied %d NetworkPolicies, want the two slots' and the gateway's", applied)
	}
	for _, slot := range []string{"maul-preview-0", "maul-preview-1"} {
		var np networkingv1.NetworkPolicy
		if err := admin.Get(ctx, client.ObjectKey{Namespace: slot, Name: "preview-isolation"}, &np); err != nil {
			t.Fatal(err)
		}
		checkSlotPolicy(t, &np)
	}
	var gw networkingv1.NetworkPolicy
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "maul", Name: "maul-preview-auth"}, &gw); err != nil {
		t.Fatal(err)
	}
	checkGatewayEgress(t, &gw)
}

// checkGatewayEgress: the gateway reaches the slots on port http alone.
func checkGatewayEgress(t *testing.T, gw *networkingv1.NetworkPolicy) {
	t.Helper()
	var toSlots []networkingv1.NetworkPolicyEgressRule
	for _, rule := range gw.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil && len(peer.NamespaceSelector.MatchExpressions) > 0 {
				toSlots = append(toSlots, rule)
			}
		}
	}
	if len(toSlots) != 1 || len(toSlots[0].Ports) != 1 || toSlots[0].Ports[0].Port.String() != "http" {
		t.Fatalf("gateway egress to the slots = %+v, want one rule on port http", toSlots)
	}
	if got := toSlots[0].To[0].NamespaceSelector.MatchExpressions[0].Values; !slices.Equal(got,
		[]string{"maul-preview-0", "maul-preview-1"}) {
		t.Fatalf("gateway egress namespaces = %v", got)
	}
}

func checkSlotPolicy(t *testing.T, np *networkingv1.NetworkPolicy) {
	t.Helper()
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("%s: podSelector %v, want every Pod", np.Namespace, np.Spec.PodSelector)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("%s: %d ingress rules, want 1", np.Namespace, len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.Ports) != 1 || rule.Ports[0].Port.String() != "http" {
		t.Errorf("%s: ingress ports %v, want http alone", np.Namespace, rule.Ports)
	}
	var cidrs []string
	gateways := 0
	for _, peer := range rule.From {
		switch {
		case peer.IPBlock != nil:
			cidrs = append(cidrs, peer.IPBlock.CIDR)
		case peer.PodSelector != nil:
			gateways++
			if peer.NamespaceSelector == nil ||
				peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "maul" ||
				len(peer.NamespaceSelector.MatchLabels) != 1 {
				t.Errorf("%s: gateway peer namespace %v, want the release namespace alone", np.Namespace,
					peer.NamespaceSelector)
			}
			if len(peer.PodSelector.MatchLabels) != 1 || peer.PodSelector.MatchLabels[gatewayLabel] != "true" {
				t.Errorf("%s: gateway peer pods %v, want %s=true alone", np.Namespace, peer.PodSelector, gatewayLabel)
			}
		default:
			t.Errorf("%s: unexpected peer %+v (a namespace without a pod selector admits it all)", np.Namespace, peer)
		}
	}
	if gateways != 1 || len(cidrs) != 0 {
		t.Errorf("%s: peers %d gateway, CIDRs %v; want one gateway peer and no subnet", np.Namespace, gateways, cidrs)
	}
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("%s: %d egress rules, want DNS alone", np.Namespace, len(np.Spec.Egress))
	}
	for _, p := range np.Spec.Egress[0].Ports {
		if p.Port.IntValue() != 53 {
			t.Errorf("%s: egress port %v, want 53", np.Namespace, p.Port)
		}
	}
}
