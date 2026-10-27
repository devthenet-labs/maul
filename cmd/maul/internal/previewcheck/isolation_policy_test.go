// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// probeEdges is the live probe's edges fixture: the policies
// preview-controller renders for a Preview's declared edges.
const probeEdges = "../../../../hack/preview-isolation-probe/edges.yaml"

// controllerEdges is the fixture's policies as preview-controller writes
// them (its managed-by label), in slot 0.
func controllerEdges(t *testing.T) []networkingv1.NetworkPolicy {
	t.Helper()
	b, err := os.ReadFile(probeEdges)
	if err != nil {
		t.Fatal(err)
	}
	var out []networkingv1.NetworkPolicy
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	for {
		var np networkingv1.NetworkPolicy
		if err := dec.Decode(&np); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		np.Namespace = "maul-preview-0"
		np.Labels[managedByLabel] = previewController
		out = append(out, np)
	}
	return out
}

// edgeCases are the fixture's tester policy bent out of the admitted
// shape, one way each, beside the shape itself (nil).
func edgeCases() map[string]func(*networkingv1.NetworkPolicy) {
	named, udp := intstr.FromString("http"), corev1.ProtocolUDP
	end := int32(9000)
	return map[string]func(*networkingv1.NetworkPolicy){
		"admitted": nil,
		"an egress ipBlock": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0] = networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}
		},
		"an egress namespaceSelector": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0].NamespaceSelector = &metav1.LabelSelector{}
		},
		"an ingress from the agents": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{
				namespacePeer("maul-agents")}, Ports: np.Spec.Egress[0].Ports}}
		},
		"an ingress from the gateway": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{
				gatewayPeer("maul")}, Ports: np.Spec.Egress[0].Ports}}
		},
		"a named port": func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].Ports[0].Port = &named },
		"a UDP port":   func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].Ports[0].Protocol = &udp },
		"an end port":  func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].Ports[0].EndPort = &end },
		"no ports":     func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].Ports = nil },
		"no peers":     func(np *networkingv1.NetworkPolicy) { np.Spec.Egress[0].To = nil },
		"a foreign UID": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0].PodSelector.MatchLabels[previewUIDLabel] = "x"
		},
		"an extra peer label": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0].PodSelector.MatchLabels["app"] = "x"
		},
		"a peer expression": func(np *networkingv1.NetworkPolicy) {
			np.Spec.Egress[0].To[0].PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
				Key: "a", Operator: metav1.LabelSelectorOpExists}}
		},
		"one policy type": func(np *networkingv1.NetworkPolicy) {
			np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
		},
		"an empty selector":   func(np *networkingv1.NetworkPolicy) { np.Spec.PodSelector = metav1.LabelSelector{} },
		"an extra label":      func(np *networkingv1.NetworkPolicy) { np.Spec.PodSelector.MatchLabels["app"] = "x" },
		"no managed-by label": func(np *networkingv1.NetworkPolicy) { delete(np.Labels, managedByLabel) },
		"a name without -np-": func(np *networkingv1.NetworkPolicy) { np.Name = "preview-edges-tester" },
	}
}

// edgeSlot is a gateway-only slot holding the fixture's policies, the
// tester's changed by mutate.
func edgeSlot(t *testing.T, mutate func(*networkingv1.NetworkPolicy)) []networkingv1.NetworkPolicy {
	t.Helper()
	edges := controllerEdges(t)
	policies := make([]networkingv1.NetworkPolicy, 0, 1+len(edges))
	policies = append(policies, *policyWith(slotPolicyName, rule(gatewayPeer("maul"))))
	for _, np := range edges {
		if mutate != nil && np.Spec.PodSelector.MatchLabels[previewPartLabel] == "tester" {
			mutate(&np)
		}
		policies = append(policies, np)
	}
	return policies
}

// TestJudgeSlotPoliciesEdgePolicies: acceptance verification's edge
// policies, exactly as preview-controller renders them for the probe's
// fixture, are accepted beside the slot policy; an edge policy bent out of
// the admission policy's shape in any one way is refused, naming it.
func TestJudgeSlotPoliciesEdgePolicies(t *testing.T) {
	for name, mutate := range edgeCases() {
		t.Run(name, func(t *testing.T) {
			_, err := judgeSlotPolicies(edgeSlot(t, mutate), "maul-preview-0", "maul", false)
			if mutate == nil {
				if err != nil {
					t.Errorf("preview-controller's edge policies are refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), edgePolicyShapeMsg) {
				t.Errorf("err = %v, want the edge policy refused", err)
			}
		})
	}
}

// ingressStub answers ingress.sh's policy read with STUB_POLICIES.
const ingressStub = `#!/bin/sh
case "$1" in --request-timeout=*) shift ;; esac
case "$1 $2" in
"get networkpolicy") printf '%s\n' "$STUB_POLICIES" ;;
*) exit 1 ;;
esac
`

// TestEdgePolicyJudgementMatchesIngressSh: ingress.sh --check-policies
// reaches the same verdict on every edge case, so the CLI and the script
// cannot drift.
func TestEdgePolicyJudgementMatchesIngressSh(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(ingressStub), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range edgeCases() {
		t.Run(name, func(t *testing.T) {
			policies := edgeSlot(t, mutate)
			_, goErr := judgeSlotPolicies(append([]networkingv1.NetworkPolicy(nil), policies...), "maul-preview-0",
				"maul", false)
			list, err := json.Marshal(networkingv1.NetworkPolicyList{Items: policies})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", ingressSh, "--check-policies", "--image",
				"registry.example/maul/previews/netprobe:sha-"+strings.Repeat("a", 40), "--no-node-isolation")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"STUB_POLICIES="+string(list))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			shErr := cmd.Run()
			if (goErr == nil) != (shErr == nil) {
				t.Errorf("the CLI says %v, ingress.sh says %v:\n%s", goErr, shErr, stderr.String())
			}
		})
	}
}
