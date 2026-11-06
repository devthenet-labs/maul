// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/provider"
)

// The agents-namespace tool pod (SPEC S2-12): what the chart renders for it,
// and nothing of it with it off.

// agentsOnArgs turn the agents-namespace tool pod on over a render that
// already has the evaluator, the intent controller and the repository
// images: with previews off, so only the tools image is set.
var agentsOnArgs = []string{
	"--set", "intentController.verification.evaluator=true",
	"--set", "intentController.verification.agents.enabled=true",
	"--set", "preview.verification.tools.image=" + toolsImage,
}

// agentsArgs is the widest agents-namespace render (evaluatorEgressBase)
// with the tool pod on.
var agentsArgs = slices.Concat(evaluatorEgressBase, agentsOnArgs)

// The agents tool pod's names: its admission policy and network policy.
const (
	toolsPodShapePolicy = "maul-tools-pod-shape"
	agentsToolsPolicy   = "maul-agents-tools"
)

// The agents tool pod's controller settings; none renders with it off.
var agentsConfigKeys = []string{"MAUL_INTENT_EVALUATOR_AGENTS", "MAUL_INTENT_TOOLS_IMAGE",
	"MAUL_INTENT_MAX_TOOL_PODS", "MAUL_INTENT_TOOL_BUILD_TIMEOUT_MAX", "MAUL_INTENT_TOOL_RESOURCES"}

// Destinations of the agents-namespace roles, in egressOf's form.
var (
	destArtifact    = "ns{kubernetes.io/metadata.name=maul} pod{app.kubernetes.io/name=source-controller} TCP/9790"
	destAgentsTools = "pod{app.kubernetes.io/managed-by=maul,app.kubernetes.io/name=maul-agent," +
		"maul.run/role=tools} TCP/8090"
	srcEvaluator = "pod{app.kubernetes.io/managed-by=maul,app.kubernetes.io/name=maul-agent," +
		"maul.run/role=evaluator} TCP/8090"
)

// destProxy is a network profile's egress proxy port.
func destProxy(port int) string {
	return fmt.Sprintf("ns{kubernetes.io/metadata.name=maul} pod{app.kubernetes.io/name=egress-proxy} TCP/%d", port)
}

// toolsPodLabels is an agents-namespace tool pod's label set, carrying the
// named network profiles' labels.
func toolsPodLabels(profiles ...string) labels.Set {
	l := labels.Set{
		"app.kubernetes.io/name":          "maul-agent",
		"app.kubernetes.io/managed-by":    "maul",
		v1alpha1.LabelRole:                v1alpha1.PreviewRoleTools,
		v1alpha1.LabelPreviewVerification: evUID,
	}
	for _, p := range profiles {
		l["maul.run/network-"+p] = "true"
	}
	return l
}

// TestAgentsToolPodOffRendersNothing: with the evaluator on and the
// agents-namespace tool pod off (slice 1's configuration), and with
// everything off, none of the tool pod's pieces renders: no shape policy, no
// tools network policy, no settings, no status grant, and the profile
// policies still leave the tools role out.
func TestAgentsToolPodOffRendersNothing(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for name, args := range map[string][]string{
		"evaluator, slot only": slices.Concat(toolsArgs, []string{"-f",
			"../../hack/testdata/chart-render/repository-images.yaml"}),
		"evaluator, no previews": slices.Concat(evaluatorEgressBase,
			[]string{"--set", "intentController.verification.evaluator=true"}),
		"everything off": evaluatorEgressBase,
	} {
		t.Run(name, func(t *testing.T) {
			objs := renderObjects(t, args)
			for _, o := range objs {
				if o.GetName() == toolsPodShapePolicy || o.GetName() == agentsToolsPolicy {
					t.Errorf("%s %s renders with the agents tool pod off", o.GetKind(), o.GetName())
				}
			}
			for _, key := range agentsConfigKeys {
				if _, ok := renderConfigMaps(t, args)["maul-intent-controller-config"][key]; ok {
					t.Errorf("intent-controller's settings carry %s with the agents tool pod off", key)
				}
			}
			if rules := intentRules(t, objs); slices.ContainsFunc(rules, grantsResource("previewverifications/status")) {
				t.Errorf("intent-controller may write verification results with the agents tool pod off: %v", rules)
			}
			// With the evaluator off no role exists, so no policy names one.
			for _, p := range agentPolicies(t, objs) {
				if name != "everything off" && strings.Contains(p.GetName(), "-agents-network-") &&
					selects(t, p, toolsPodLabels("go")) {
					t.Errorf("%s selects a tool pod with the agents tool pod off", p.GetName())
				}
			}
		})
	}
}

// TestAgentsToolPodEgressMatrix: in every network mode, with the agents
// tool pod on, the union of the agents-namespace policies selecting each
// role is exactly its row: the tool pod reaches the artifact server and,
// carrying a network profile's label, that profile's proxy port (its build's
// network, never wider); the evaluator gains the agents tool pods on 8090
// beside the broker and the slot tool pods; the check reviewer is
// unchanged. The tool pod's only ingress is an evaluator on 8090, and no
// other agent pod gains any ingress.
func TestAgentsToolPodEgressMatrix(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	rows := []struct {
		name string
		pod  labels.Set
		want []string
	}{
		{"tool pod, no profile", toolsPodLabels(), []string{destArtifact}},
		{"tool pod, profile go", toolsPodLabels("go"), []string{destArtifact, destProxy(8090)}},
		{"tool pod, profile npm", toolsPodLabels("npm"), []string{destArtifact, destProxy(8091)}},
		{"evaluator", agentPod("claude", v1alpha1.RoleEvaluator), []string{destBroker, destSlotTools,
			destAgentsTools}},
		{"check reviewer", agentPod("claude", v1alpha1.RoleCheckReview), []string{destBroker}},
	}
	for _, mode := range evaluatorEgressModes {
		t.Run(mode.name, func(t *testing.T) {
			policies := agentPolicies(t, renderObjects(t, slices.Concat(agentsArgs, mode.args)))
			for _, row := range rows {
				got, isolated := egressOf(t, policies, row.pod)
				if !isolated {
					t.Errorf("%s is not egress-isolated", row.name)
				}
				if want := slices.Sorted(slices.Values(row.want)); !slices.Equal(got, want) {
					t.Errorf("%s egress:\n got %q\nwant %q", row.name, got, want)
				}
			}
			if got := ingressOf(t, policies, toolsPodLabels("go")); !slices.Equal(got, []string{srcEvaluator}) {
				t.Errorf("tool pod ingress = %q, want only %q", got, srcEvaluator)
			}
			for _, pod := range []labels.Set{agentPod("claude", v1alpha1.RoleEvaluator),
				agentPod("claude", v1alpha1.RoleCheckReview), agentPod("claude", "")} {
				if got := ingressOf(t, policies, pod); len(got) != 0 {
					t.Errorf("agent pod %v gains ingress %q", pod, got)
				}
			}
		})
	}
}

// TestAgentsToolPodLeavesSlotPoliciesAlone: turning the agents tool pod on
// over slice 1's configuration changes no agents-namespace policy but the
// evaluator's (one more rule) and the profile policies' role exclusion
// (which no longer names tools), and adds only the tools policy.
func TestAgentsToolPodLeavesSlotPoliciesAlone(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	s1 := slices.Concat(evaluatorEgressBase, []string{"--set", "intentController.verification.evaluator=true"})
	off := agentPolicies(t, renderObjects(t, s1))
	on := agentPolicies(t, renderObjects(t, agentsArgs))
	offByName := map[string]*unstructured.Unstructured{}
	for _, p := range off {
		offByName[p.GetKind()+"/"+p.GetName()] = p
	}
	for _, p := range on {
		key := p.GetKind() + "/" + p.GetName()
		was, ok := offByName[key]
		switch {
		case !ok && p.GetName() == agentsToolsPolicy:
		case !ok:
			t.Errorf("new policy %s", key)
		case p.GetName() == "maul-agents-evaluator", strings.Contains(p.GetName(), "-agents-network-"):
		default:
			if fmt.Sprint(p.Object["spec"]) != fmt.Sprint(was.Object["spec"]) {
				t.Errorf("%s changed:\n got %v\nwant %v", key, p.Object["spec"], was.Object["spec"])
			}
		}
	}
	for _, pod := range []labels.Set{agentPod("claude", ""), agentPod("codex", "")} {
		before, _ := egressOf(t, off, pod)
		after, _ := egressOf(t, on, pod)
		if !slices.Equal(before, after) {
			t.Errorf("ordinary pod %v egress changed:\n off %q\n  on %q", pod, before, after)
		}
	}
}

// TestAgentsToolPodRefusals: the agents tool pod refuses to render without
// what it stands on, naming the value to fix, and renders once it is fixed.
func TestAgentsToolPodRefusals(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no evaluator", []string{"--set", "intentController.verification.evaluator=false"},
			"requires intentController.verification.evaluator"},
		{"no intent controller", []string{"--set", "intentController.enabled=false"},
			"requires intentController.enabled"},
		{"no repository images", []string{"--set", "agent.repositoryImages.enabled=false"},
			"requires agent.repositoryImages.enabled"},
		{"no tools image", []string{"--set", "preview.verification.tools.image="},
			"preview.verification.tools.image is required"},
		{"tools image by tag", []string{"--set",
			"preview.verification.tools.image=ghcr.io/devthenet-labs/maul/preview-tools:v1"}, "pinned by digest"},
		{"tools image under an agent registry", []string{"--set",
			"preview.verification.tools.image=ghcr.io/example/agent-images/tools@sha256:" + strings.Repeat("1", 64)},
			"lies under the agent registry"},
		{"build timeout over 20m", []string{"--set", "intentController.verification.agents.buildTimeoutMax=21m"},
			"buildTimeoutMax 21m must be"},
		{"build timeout under 1m", []string{"--set", "intentController.verification.agents.buildTimeoutMax=30s"},
			"buildTimeoutMax 30s must be"},
		{"no agent policies", []string{"--set", "agent.networkPolicy.create=false"}, "agent.networkPolicy.create"},
		{"istio", []string{"--set", "agent.networkPolicy.mode=istio"}, "mode istio"},
		{"no tool pods", []string{"--set", "intentController.verification.agents.maxToolPods=0"}, "maxToolPods"},
		{"sandbox without ephemeral storage", []string{"--set",
			"intentController.verification.agents.resources.sandbox.ephemeralStorage=null"}, "ephemeralStorage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := slices.Concat([]string{"template", "maul", ".", "--namespace", "maul"}, agentsArgs, tc.args)
			out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
			if err == nil {
				t.Fatal("rendered; want a refusal")
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("refusal lacks %q:\n%s", tc.want, out)
			}
		})
	}
	all := slices.Concat([]string{"template", "maul", ".", "--namespace", "maul"}, agentsArgs)
	if out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput(); err != nil {
		t.Errorf("the agents tool pod refuses with every requirement met: %v\n%s", err, out)
	}
}

// TestAgentsToolPodConfig: with previews off, intent-controller gets
// verification, every evaluator setting and the tool pod's own: the
// preview-tools image, the tool pod bound, the build ceiling and each
// container's quantities as JSON strings.
func TestAgentsToolPodConfig(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	cm := renderConfigMaps(t, slices.Concat(agentsArgs,
		[]string{"--set", "intentController.verification.agents.resources.sandbox.cpu=2"}))["maul-intent-controller-config"]
	for key, want := range map[string]string{
		"MAUL_INTENT_PREVIEWS_ENABLED":       "false",
		"MAUL_INTENT_VERIFICATION":           "true",
		"MAUL_INTENT_EVALUATOR":              "true",
		"MAUL_INTENT_EVALUATOR_AGENTS":       "true",
		"MAUL_INTENT_TOOLS_IMAGE":            toolsImage,
		"MAUL_INTENT_MAX_TOOL_PODS":          "2",
		"MAUL_INTENT_TOOL_BUILD_TIMEOUT_MAX": "20m",
	} {
		if cm[key] != want {
			t.Errorf("%s = %q, want %q", key, cm[key], want)
		}
	}
	for _, key := range toolConfigKeys["maul-intent-controller-config"] {
		if _, ok := cm[key]; !ok {
			t.Errorf("evaluator setting %s missing with previews off", key)
		}
	}
	var res map[string]map[string]string
	if err := json.Unmarshal([]byte(cm["MAUL_INTENT_TOOL_RESOURCES"]), &res); err != nil {
		t.Fatalf("MAUL_INTENT_TOOL_RESOURCES %q: %v", cm["MAUL_INTENT_TOOL_RESOURCES"], err)
	}
	want := map[string]map[string]string{
		"tools":   {"cpu": "250m", "memory": "256Mi", "ephemeralStorage": "2Gi"},
		"sandbox": {"cpu": "2", "memory": "2Gi", "ephemeralStorage": "8Gi"},
	}
	if fmt.Sprint(res) != fmt.Sprint(want) {
		t.Errorf("MAUL_INTENT_TOOL_RESOURCES = %v, want %v", res, want)
	}
}

// TestAgentsToolPodGrants: with previews off, intent-controller may write
// verification requests and their results (the agents host's), and the
// verification writer policy renders with a writer per placement; with
// previews and slot verification on too, the grant appears once.
func TestAgentsToolPodGrants(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	both := slices.Concat(toolsArgs, []string{"-f", "../../hack/testdata/chart-render/repository-images.yaml",
		"--set", "intentController.verification.agents.enabled=true"})
	for name, args := range map[string][]string{"previews off": agentsArgs, "previews on": both} {
		t.Run(name, func(t *testing.T) {
			objs := renderObjects(t, args)
			rules := intentRules(t, objs)
			for _, want := range []string{"previewverifications:create,delete,get,list,watch",
				"previewverifications/status:update"} {
				if n := len(slices.DeleteFunc(slices.Clone(rules), func(r string) bool { return r != want })); n != 1 {
					t.Errorf("intent-controller rule %q appears %d times in %q, want once", want, n, rules)
				}
			}
			policy := findObject(objs, "ValidatingAdmissionPolicy", "maul-preview-verifications")
			if policy == nil {
				t.Fatal("maul-preview-verifications does not render")
			}
			text := policyText(policy)
			for _, want := range []string{"spec.?placement.orValue('slot') == 'agents'",
				"system:serviceaccount:maul:maul-intent-controller",
				"system:serviceaccount:maul:maul-preview-controller"} {
				if !strings.Contains(text, want) {
					t.Errorf("maul-preview-verifications lacks %q", want)
				}
			}
		})
	}
}

// TestToolsPodShapeRender: the shape policy names the stamped tools image,
// binds the agents namespace alone, and refuses every model credential and
// gateway variable an agent pod may carry (each harness's EnvKeys and
// provider.GatewayEnvNames), no more and no fewer.
func TestToolsPodShapeRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	objs := renderObjects(t, agentsArgs)
	policy := findObject(objs, "ValidatingAdmissionPolicy", toolsPodShapePolicy)
	binding := findObject(objs, "ValidatingAdmissionPolicyBinding", toolsPodShapePolicy)
	if policy == nil || binding == nil {
		t.Fatalf("%s policy %v binding %v: want both", toolsPodShapePolicy, policy != nil, binding != nil)
	}
	if got := boundNamespaces(binding); !slices.Equal(got, []string{egressAgents}) {
		t.Errorf("bound to %q, want only %s", got, egressAgents)
	}
	if text := policyText(policy); !strings.Contains(text, "c.image == '"+toolsImage+"'") {
		t.Errorf("the shape policy does not pin the tools image %s", toolsImage)
	}
	want := map[string]bool{}
	for _, keys := range [][]string{harness.NewClaude().EnvKeys(), harness.NewCodex().EnvKeys(),
		harness.NewCopilot().EnvKeys(), provider.GatewayEnvNames} {
		for _, k := range keys {
			want[k] = true
		}
	}
	vars, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
	var got []string
	for _, v := range vars {
		m := v.(map[string]any)
		if m["name"] == "credentialEnv" {
			if err := json.Unmarshal([]byte(m["expression"].(string)), &got); err != nil {
				t.Fatalf("credentialEnv %q: %v", m["expression"], err)
			}
		}
	}
	if g, w := slices.Sorted(slices.Values(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(g, w) {
		t.Errorf("credentialEnv:\n got %q\nwant %q", g, w)
	}
}

// intentRules is intent-controller's release-namespace Role as
// "resource:verbs" strings, verbs sorted.
func intentRules(t *testing.T, objs []*unstructured.Unstructured) []string {
	t.Helper()
	role := findObject(objs, "Role", "maul-intent-controller")
	if role == nil {
		t.Fatal("no maul-intent-controller Role")
	}
	raw, _, _ := unstructured.NestedSlice(role.Object, "rules")
	var out []string
	for _, r := range raw {
		m := r.(map[string]any)
		res, _, _ := unstructured.NestedStringSlice(m, "resources")
		verbs, _, _ := unstructured.NestedStringSlice(m, "verbs")
		slices.Sort(verbs)
		for _, name := range res {
			out = append(out, name+":"+strings.Join(verbs, ","))
		}
	}
	return out
}

// grantsResource matches a rule string for resource.
func grantsResource(resource string) func(string) bool {
	return func(r string) bool { return strings.HasPrefix(r, resource+":") }
}

// ingressOf is the sorted union of the ingress sources every NetworkPolicy
// selecting pod admits, in networkPolicyDests' form.
func ingressOf(t *testing.T, policies []*unstructured.Unstructured, pod labels.Set) []string {
	t.Helper()
	set := map[string]bool{}
	for _, p := range policies {
		if p.GetKind() != "NetworkPolicy" || !selects(t, p, pod) {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
		for _, r := range rules {
			m := r.(map[string]any)
			from := map[string]any{"ports": m["ports"], "to": m["from"]}
			for _, d := range networkPolicyDests(from) {
				set[d] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}
