// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"fmt"
	"maps"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// evaluatorEgressBase is the widest agents-namespace render: repository
// images and the egress proxy (so the network-profile policies render), the
// intent controller (brokered claude, so the broker rules render) and a
// non-brokered copilot runner (so GKE's FQDNNetworkPolicy renders too).
var evaluatorEgressBase = []string{
	"-f", "../../hack/testdata/chart-render/repository-images.yaml",
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../hack/testdata/chart-render/egress-proxy.yaml",
	"--set", "agent.runners.copilot.enabled=true",
}

// evaluatorEgressModes are the network modes the evaluator's egress must be
// closed in, each as the helm arguments that select it. auto renders as
// none without capabilities, and as cilium or gke with that CRD served.
var evaluatorEgressModes = []struct {
	name string
	args []string
}{
	{"standard", []string{"--set", "agent.networkPolicy.mode=none"}},
	{"standard, dns none", []string{"--set", "agent.networkPolicy.mode=none", "--set", "agent.networkPolicy.dns=none",
		"--set", "agent.runners.copilot.enabled=false"}},
	{"cilium", []string{"--set", "agent.networkPolicy.mode=cilium"}},
	{"gke", []string{"--set", "agent.networkPolicy.mode=gke"}},
	{"auto", []string{"--set", "agent.networkPolicy.mode=auto"}},
	{"auto, cilium served", []string{"--set", "agent.networkPolicy.mode=auto",
		"--api-versions", "cilium.io/v2/CiliumNetworkPolicy"}},
	{"auto, gke served", []string{"--set", "agent.networkPolicy.mode=auto",
		"--api-versions", "networking.gke.io/v1alpha1/FQDNNetworkPolicy"}},
}

// Each role's row of DESIGN 4.2, as egress destinations in egressOf's form.
var (
	destBroker    = "ns{kubernetes.io/metadata.name=maul} pod{app.kubernetes.io/name=egress-broker} TCP/8080"
	destSlotTools = "ns{app.kubernetes.io/part-of=maul-preview} pod{maul.run/preview-role=tools} TCP/8090"
)

// agentPod is the label set of an agents-namespace agent pod: the chart's
// agent selector, a harness, a network profile and, when role is set, the
// role label. The profile label shows a role pod gains nothing from it.
func agentPod(harness, role string) labels.Set {
	l := labels.Set{
		"app.kubernetes.io/name":       "maul-agent",
		"app.kubernetes.io/managed-by": "maul",
		v1alpha1.LabelHarness:          harness,
		"maul.run/network-go":          "true",
	}
	if role != "" {
		l[v1alpha1.LabelRole] = role
		l[v1alpha1.LabelPreviewVerification] = "6f1c1f0e-0000-4000-8000-000000000001"
	}
	return l
}

// TestEvaluatorEgressMatrix: in every network mode, the union of the
// agents-namespace policies selecting each role is exactly its DESIGN 4.2
// row (the evaluator: the broker and a slot tool pod on 8090; the check
// reviewer: the broker; an agents-namespace tool pod: nothing yet), each
// role pod is egress-isolated, and no DNS reaches any of them.
func TestEvaluatorEgressMatrix(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	rows := []struct {
		role string
		want []string
	}{
		{v1alpha1.RoleEvaluator, []string{destBroker, destSlotTools}},
		{v1alpha1.RoleCheckReview, []string{destBroker}},
		{v1alpha1.PreviewRoleTools, nil},
	}
	for _, mode := range evaluatorEgressModes {
		t.Run(mode.name, func(t *testing.T) {
			args := slices.Concat(evaluatorEgressBase, mode.args,
				[]string{"--set", "intentController.verification.evaluator=true"})
			policies := agentPolicies(t, renderObjects(t, args))
			for _, row := range rows {
				for _, harness := range []string{"claude", "codex", "copilot"} {
					pod := agentPod(harness, row.role)
					got, isolated := egressOf(t, policies, pod)
					if !isolated {
						t.Errorf("%s (%s) is not egress-isolated", row.role, harness)
					}
					if want := slices.Sorted(slices.Values(row.want)); !slices.Equal(got, want) {
						t.Errorf("%s (%s) egress:\n got %q\nwant %q", row.role, harness, got, want)
					}
				}
			}
		})
	}
}

// TestEvaluatorEgressLeavesAgentPoliciesAlone: off, no agents-namespace
// policy names maul.run/role and no evaluator policy renders; on, every
// policy that already rendered differs only by the role exclusion in its
// selector, every new one selects no ordinary agent pod, and an ordinary
// pod's egress is unchanged.
func TestEvaluatorEgressLeavesAgentPoliciesAlone(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, mode := range evaluatorEgressModes {
		t.Run(mode.name, func(t *testing.T) {
			off := agentPolicies(t, renderObjects(t, slices.Concat(evaluatorEgressBase, mode.args)))
			on := agentPolicies(t, renderObjects(t, slices.Concat(evaluatorEgressBase, mode.args,
				[]string{"--set", "intentController.verification.evaluator=true"})))
			offByName := map[string]*unstructured.Unstructured{}
			for _, p := range off {
				offByName[p.GetKind()+"/"+p.GetName()] = p
				if text := fmt.Sprint(p.Object["spec"]); strings.Contains(text, v1alpha1.LabelRole) {
					t.Errorf("off: %s %s names %s", p.GetKind(), p.GetName(), v1alpha1.LabelRole)
				}
			}
			ordinary := make([]labels.Set, 0, 6)
			for _, h := range []string{"claude", "codex", "copilot"} {
				plain := agentPod(h, "")
				ordinary = append(ordinary, plain)
				bare := maps.Clone(plain)
				delete(bare, "maul.run/network-go")
				ordinary = append(ordinary, bare)
			}
			for _, p := range on {
				was, ok := offByName[p.GetKind()+"/"+p.GetName()]
				if !ok {
					for _, pod := range ordinary {
						if selects(t, p, pod) {
							t.Errorf("new %s %s selects an ordinary agent pod %v", p.GetKind(), p.GetName(), pod)
						}
					}
					continue
				}
				if got, want := withoutRoleExclusion(p), was.Object["spec"]; !reflect.DeepEqual(got, want) {
					t.Errorf("%s %s changed beyond the role exclusion:\n got %v\nwant %v",
						p.GetKind(), p.GetName(), got, want)
				}
			}
			for _, pod := range ordinary {
				before, _ := egressOf(t, off, pod)
				after, _ := egressOf(t, on, pod)
				if !slices.Equal(before, after) {
					t.Errorf("ordinary pod %v egress changed:\n off %q\n  on %q", pod, before, after)
				}
			}
		})
	}
}

// TestEvaluatorEgressRefusals: the evaluator's egress cannot be closed by
// the chart under mode istio (its agents-istio policy grants every agent pod
// istiod) or with the agent policies not rendered, so both refuse to render.
func TestEvaluatorEgressRefusals(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"istio", []string{"--set", "agent.networkPolicy.mode=istio"}, "mode istio"},
		{"no agent policies", []string{"--set", "agent.networkPolicy.create=false"}, "agent.networkPolicy.create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := slices.Concat([]string{"template", "maul", ".", "--namespace", "maul"},
				evaluatorOnArgs, tc.args)
			out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
			if err == nil {
				t.Fatal("rendered; want a refusal")
			}
			if !strings.Contains(string(out), "intentController.verification.evaluator") ||
				!strings.Contains(string(out), tc.want) {
				t.Errorf("refusal does not name the evaluator and %q:\n%s", tc.want, out)
			}
			off := slices.Concat([]string{"template", "maul", ".", "--namespace", "maul",
				"-f", "../../hack/testdata/chart-render/intent-controller.yaml"}, tc.args)
			if out, err := exec.CommandContext(t.Context(), "helm", off...).CombinedOutput(); err != nil {
				t.Errorf("off still refuses: %v\n%s", err, out)
			}
		})
	}
}

// agentPolicies are the network policies of every dialect in the agents
// namespace.
func agentPolicies(t *testing.T, objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetNamespace() != egressAgents {
			continue
		}
		switch o.GetKind() {
		case "NetworkPolicy", "CiliumNetworkPolicy", "FQDNNetworkPolicy":
			out = append(out, o)
		case "Sidecar", "AuthorizationPolicy":
			t.Fatalf("unexpected istio %s %s in a non-istio mode", o.GetKind(), o.GetName())
		}
	}
	return out
}

// selectorOf is a policy's pod selector, whatever its dialect.
func selectorOf(t *testing.T, p *unstructured.Unstructured) labels.Selector {
	t.Helper()
	field := "podSelector"
	if p.GetKind() == "CiliumNetworkPolicy" {
		field = "endpointSelector"
	}
	raw, _, _ := unstructured.NestedMap(p.Object, "spec", field)
	var ls metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &ls); err != nil {
		t.Fatalf("%s %s selector: %v", p.GetKind(), p.GetName(), err)
	}
	sel, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		t.Fatalf("%s %s selector: %v", p.GetKind(), p.GetName(), err)
	}
	return sel
}

func selects(t *testing.T, p *unstructured.Unstructured, pod labels.Set) bool {
	t.Helper()
	return selectorOf(t, p).Matches(pod)
}

// egressOf is the sorted union of the egress destinations every policy
// selecting pod grants, and whether any of them isolates its egress.
func egressOf(t *testing.T, policies []*unstructured.Unstructured, pod labels.Set) ([]string, bool) {
	t.Helper()
	set := map[string]bool{}
	isolated := false
	for _, p := range policies {
		if !selects(t, p, pod) {
			continue
		}
		switch p.GetKind() {
		case "NetworkPolicy":
			types, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "policyTypes")
			if slices.Contains(types, "Egress") {
				isolated = true
			}
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "egress")
			for _, r := range rules {
				for _, d := range networkPolicyDests(r.(map[string]any)) {
					set[d] = true
				}
			}
		case "CiliumNetworkPolicy":
			isolated = true
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "egress")
			for _, r := range rules {
				set["cilium "+canonical(r)] = true
			}
		case "FQDNNetworkPolicy":
			isolated = true
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "egress")
			for _, r := range rules {
				set["fqdn "+canonical(r)] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set)), isolated
}

// networkPolicyDests renders one NetworkPolicy egress rule as one string
// per peer: "ns{..} pod{..} PROTO/port,...", "cidr ..." or "any".
func networkPolicyDests(rule map[string]any) []string {
	ps, _ := rule["ports"].([]any)
	ports := make([]string, 0, len(ps))
	for _, p := range ps {
		m := p.(map[string]any)
		ports = append(ports, fmt.Sprintf("%v/%v", m["protocol"], m["port"]))
	}
	suffix := strings.Join(ports, ",")
	if suffix == "" {
		suffix = "all-ports"
	}
	peers, _ := rule["to"].([]any)
	if len(peers) == 0 {
		return []string{"any " + suffix}
	}
	var out []string
	for _, peer := range peers {
		m := peer.(map[string]any)
		var parts []string
		if b, ok := m["ipBlock"]; ok {
			parts = append(parts, "cidr "+canonical(b))
		}
		if ns, ok := m["namespaceSelector"]; ok {
			parts = append(parts, "ns"+selectorString(ns))
		}
		if pod, ok := m["podSelector"]; ok {
			parts = append(parts, "pod"+selectorString(pod))
		}
		out = append(out, strings.Join(parts, " ")+" "+suffix)
	}
	return out
}

// selectorString is a selector's matchLabels as {k=v,...}, or its
// canonical form when it has expressions.
func selectorString(sel any) string {
	m, _ := sel.(map[string]any)
	if _, ok := m["matchExpressions"]; ok {
		return canonical(sel)
	}
	ml, _ := m["matchLabels"].(map[string]any)
	var kv []string
	for _, k := range slices.Sorted(maps.Keys(ml)) {
		kv = append(kv, fmt.Sprintf("%s=%v", k, ml[k]))
	}
	return "{" + strings.Join(kv, ",") + "}"
}

// canonical is a stable text form of any decoded YAML value (fmt sorts map
// keys).
func canonical(v any) string { return fmt.Sprint(v) }

// withoutRoleExclusion is a policy's spec with the maul.run/role NotIn
// expression removed from its pod selector (and matchExpressions dropped
// when that leaves it empty): what the policy rendered before the evaluator.
func withoutRoleExclusion(p *unstructured.Unstructured) any {
	spec := runtime.DeepCopyJSONValue(p.Object["spec"]).(map[string]any)
	field := "podSelector"
	if p.GetKind() == "CiliumNetworkPolicy" {
		field = "endpointSelector"
	}
	sel, _ := spec[field].(map[string]any)
	exprs, _ := sel["matchExpressions"].([]any)
	var kept []any
	for _, e := range exprs {
		m := e.(map[string]any)
		if m["key"] == v1alpha1.LabelRole && m["operator"] == "NotIn" {
			continue
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		delete(sel, "matchExpressions")
	} else if sel != nil {
		sel["matchExpressions"] = kept
	}
	return spec
}
