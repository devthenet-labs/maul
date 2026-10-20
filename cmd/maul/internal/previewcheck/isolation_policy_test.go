// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os/exec"
	"reflect"
	"testing"
	"testing/quick"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// policyCase is a generated slot namespace: its policies, and whether the
// legacy load balancer path serves.
type policyCase struct {
	policies []networkingv1.NetworkPolicy
	legacy   bool
}

// TestJudgeSlotPoliciesAdmitsOnlyTheGateway (seeded): the static check
// accepts a slot namespace exactly when preview-isolation exists, governs
// ingress for every Pod and admits the gateway's peer, and no policy there
// admits anything but that peer (and, while the legacy path serves, an
// ipBlock): no rule from anywhere, no other selector, no expression.
func TestJudgeSlotPoliciesAdmitsOnlyTheGateway(t *testing.T) {
	gw := gatewayPeer("maul")
	exprGW := gatewayPeer("maul")
	exprGW.NamespaceSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: metadataNameLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"maul"}}},
		MatchLabels: map[string]string{metadataNameLabel: "maul"}}
	extraLabel := gatewayPeer("maul")
	extraLabel.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{gatewayLabel: "true", "a": "b"}}
	block := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.40.128.0/24"}}
	blockAndPods := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"},
		PodSelector: &metav1.LabelSelector{}}
	// good[i] says whether pool[i] is an allowed peer (blocks: only with
	// legacy).
	pool := []networkingv1.NetworkPolicyPeer{gw, block, namespacePeer("maul-agents"), namespacePeer("maul"),
		{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{gatewayLabel: "true"}}},
		{PodSelector: &metav1.LabelSelector{}}, {NamespaceSelector: &metav1.LabelSelector{}},
		gatewayPeer("maul-agents"), exprGW, extraLabel, blockAndPods}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		var c policyCase
		c.legacy = r.Intn(2) == 0
		names := []string{slotPolicyName, "allow-x"}
		for range 1 + r.Intn(2) {
			name := names[r.Intn(len(names))]
			np := policyWith(name)
			if r.Intn(10) == 0 {
				np.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
			}
			if r.Intn(10) == 0 {
				np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
			}
			for range r.Intn(3) {
				var rl networkingv1.NetworkPolicyIngressRule
				if r.Intn(8) > 0 {
					for range 1 + r.Intn(2) {
						// Bias towards the gateway's peer so accepted
						// namespaces are common.
						if r.Intn(2) == 0 {
							rl.From = append(rl.From, gw)
						} else {
							rl.From = append(rl.From, pool[r.Intn(len(pool))])
						}
					}
				}
				np.Spec.Ingress = append(np.Spec.Ingress, rl)
			}
			c.policies = append(c.policies, *np)
		}
		args[0] = reflect.ValueOf(c)
	}
	accepted := 0
	prop := func(c policyCase) bool {
		want := oracleAccepts(c, gw, block)
		_, err := judgeSlotPolicies(append([]networkingv1.NetworkPolicy(nil), c.policies...), "maul-preview-0",
			"maul", c.legacy)
		if err == nil {
			accepted++
		}
		if (err == nil) != want {
			b, _ := yaml.Marshal(c.policies)
			t.Logf("legacy %v, accepted %v (%v), want %v:\n%s", c.legacy, err == nil, err, want, b)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	if accepted == 0 {
		t.Error("no generated slot namespace was accepted: the property only ever saw refusals")
	}
}

// oracleAccepts is the property's own statement of an acceptable slot
// namespace, over the generator's peers: gw the gateway's exact peer,
// block the one ipBlock (allowed only with the legacy path).
func oracleAccepts(c policyCase, gw, block networkingv1.NetworkPolicyPeer) bool {
	ok, found, admits := true, false, false
	for _, np := range c.policies {
		if np.Name == slotPolicyName {
			found = true
			egressOnly := len(np.Spec.PolicyTypes) == 1 && np.Spec.PolicyTypes[0] == networkingv1.PolicyTypeEgress
			ok = ok && len(np.Spec.PodSelector.MatchLabels) == 0 && !egressOnly
		}
		for _, rl := range np.Spec.Ingress {
			ok = ok && len(rl.From) > 0
			for _, p := range rl.From {
				switch {
				case reflect.DeepEqual(p, gw):
					admits = admits || np.Name == slotPolicyName
				case reflect.DeepEqual(p, block) && c.legacy:
				default:
					ok = false
				}
			}
		}
	}
	return ok && found && admits
}

// TestJudgeSlotPoliciesAcceptsTheChart: the slot policy the chart renders
// for the setup command's gateway-only values is accepted, and the static
// check is no stricter than the chart itself.
func TestJudgeSlotPoliciesAcceptsTheChart(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	out, err := exec.CommandContext(t.Context(), "helm", "template", "maul", "../../../../charts/maul",
		"-n", "maul", "-f", "../../../../hack/testdata/chart-render/intent-controller.yaml",
		"-f", "../previewsetup/testdata/golden/cloudflare-tunnel-k3s/maul-previews.values.yaml",
		"--set", "previewAuth.keys.renderOffline=true",
		"--set", "previewController.config.apiServerCIDR=10.43.0.1/32").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("helm template: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	slots := map[string][]networkingv1.NetworkPolicy{}
	for {
		var np networkingv1.NetworkPolicy
		if err := dec.Decode(&np); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if np.Kind == "NetworkPolicy" && len(np.Namespace) > len("maul-preview-") &&
			np.Namespace[:len("maul-preview-")] == "maul-preview-" {
			slots[np.Namespace] = append(slots[np.Namespace], np)
		}
	}
	if len(slots) == 0 {
		t.Fatal("the render has no slot NetworkPolicy")
	}
	for ns, policies := range slots {
		if _, err := judgeSlotPolicies(policies, ns, "maul", false); err != nil {
			t.Errorf("%s: the chart's own slot policies are refused: %v", ns, err)
		}
	}
}
