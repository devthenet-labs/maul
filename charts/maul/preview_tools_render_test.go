// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// The slot tool pod's three images, digest-pinned, outside the preview
// prefix and every agent registry.
var (
	toolsImage   = "ghcr.io/devthenet-labs/maul/preview-tools@sha256:" + strings.Repeat("1", 64)
	browserImage = "ghcr.io/devthenet-labs/maul/preview-browser@sha256:" + strings.Repeat("2", 64)
	toolboxImage = "ghcr.io/devthenet-labs/maul/preview-toolbox@sha256:" + strings.Repeat("3", 64)
)

// toolsArgs renders acceptance verification with the agent evaluator on: the
// slot tool pod's admission, quota, LimitRange, isolation and grants.
var toolsArgs = append(append([]string{}, verificationArgs...),
	"--set", "intentController.verification.evaluator=true",
	"--set", "preview.verification.tools.image="+toolsImage,
	"--set", "preview.verification.tools.browserImage="+browserImage,
	"--set", "preview.verification.tools.toolboxImage="+toolboxImage)

// toolPolicies are the admission policies the tool pod adds: preview-
// controller's slot Secrets and ConfigMaps, and its release-namespace ledger
// and evidence ConfigMaps.
var toolPolicies = []string{"maul-preview-tool-objects", "maul-preview-ledgers"}

// The label sets a slot's pods carry: a component's, and the tool pod's.
var (
	componentPodLabels = labels.Set{"maul.run/preview": "battles", "maul.run/preview-uid": verifyUID,
		"maul.run/preview-part": "api"}
	toolPodLabels = labels.Set{"maul.run/preview": "battles", "maul.run/preview-uid": verifyUID,
		"maul.run/preview-part": "tools", "maul.run/preview-role": "tools",
		"maul.run/preview-verification": evUID}
)

// toolConfigKeys are the controller settings the evaluator renders; none
// renders without it.
var toolConfigKeys = map[string][]string{
	"maul-preview-controller-config": {"MAUL_PREVIEW_EVALUATOR", "MAUL_PREVIEW_TOOLS_IMAGE",
		"MAUL_PREVIEW_BROWSER_IMAGE", "MAUL_PREVIEW_TOOLBOX_IMAGE", "MAUL_PREVIEW_AGENTS_NAMESPACE"},
	"maul-intent-controller-config": {"MAUL_INTENT_EVALUATOR", "MAUL_INTENT_EVALUATE_MODEL",
		"MAUL_INTENT_CHECKREVIEW_MODEL", "MAUL_INTENT_EVALUATE_TIMEOUT", "MAUL_INTENT_DEFAULT_HANDOFF",
		"MAUL_INTENT_HANDOFF_MAX", "MAUL_INTENT_EVALUATE_CODEX", "MAUL_INTENT_EVALUATION_COST_MAX",
		"MAUL_INTENT_MAX_TOOL_CALLS", "MAUL_INTENT_EVALUATE_MAX_TURNS", "MAUL_INTENT_CHECKREVIEW_MAX_TURNS"},
}

// TestPreviewToolsOffRendersNothing: verification without the evaluator, and
// the evaluator without verification, render none of the tool pod's slot
// pieces: today's isolation selector, LimitRange, grants and Job roles, and
// no evaluator setting for either controller.
func TestPreviewToolsOffRendersNothing(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for name, args := range map[string][]string{
		"verification only": verificationArgs,
		"evaluator only": append(append([]string{}, previewRuntimeArgs...),
			"--set", "intentController.verification.evaluator=true"),
	} {
		t.Run(name, func(t *testing.T) {
			objs := renderObjects(t, args)
			for _, u := range objs {
				if slices.Contains(toolPolicies, u.GetName()) || u.GetName() == "preview-tools-isolation" {
					t.Errorf("renders %s %s", u.GetKind(), u.GetName())
				}
			}
			for _, np := range kindNamed(objs, "NetworkPolicy", "preview-isolation") {
				if sel, _, _ := unstructured.NestedMap(np.Object, "spec", "podSelector"); len(sel) != 0 {
					t.Errorf("%s preview-isolation selects %v, want every pod", np.GetNamespace(), sel)
				}
			}
			for _, lr := range kindNamed(objs, "LimitRange", "preview-limits") {
				if got := limitRangeMax(t, lr); got != "1/512Mi" {
					t.Errorf("%s LimitRange max %s, want 1/512Mi", lr.GetNamespace(), got)
				}
			}
			for _, role := range rolesNamed(objs, "maul-preview-controller") {
				g := grants(role)
				for _, res := range []string{"/secrets", "/configmaps"} {
					if v, ok := g[res]; ok {
						t.Errorf("%s/%s grants %s %q", role.GetNamespace(), role.GetName(), res, v)
					}
				}
				if role.GetNamespace() != egressRelease && g["/events"] != "" {
					t.Errorf("%s: preview-controller reads events: %q", role.GetNamespace(), g["/events"])
				}
			}
			if strings.Contains(validationsText(findObject(objs, "ValidatingAdmissionPolicy", "maul-preview-jobs")),
				"'tools'") {
				t.Error("maul-preview-jobs admits the tools role")
			}
			cms := renderConfigMaps(t, args)
			for cm, keys := range toolConfigKeys {
				for _, k := range keys {
					if v, ok := cms[cm][k]; ok {
						t.Errorf("%s carries %s=%q", cm, k, v)
					}
				}
			}
		})
	}
}

// TestPreviewToolsRender: verification with the evaluator on renders the
// tool pod's slot pieces: preview-controller's added grants (slot events
// read, the key Secret and tool ConfigMap, the release ledger and evidence
// ConfigMaps) and their fences; the three images in the pod and Job rules
// (never the Deployments'); the tools Job role; the quota and LimitRange
// SP-2 measured; preview-isolation leaving the tool pod to its own
// default-deny; and both controllers' evaluator settings.
func TestPreviewToolsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	objs := renderObjects(t, toolsArgs)
	checkVerificationPolicies(t, objs)
	for _, name := range toolPolicies {
		if findObject(objs, "ValidatingAdmissionPolicy", name) == nil ||
			findObject(objs, "ValidatingAdmissionPolicyBinding", name) == nil {
			t.Errorf("no ValidatingAdmissionPolicy and binding %s", name)
		}
	}
	for _, slot := range []string{"maul-preview-0", "maul-preview-1"} {
		got := grants(onlyRole(t, objs, slot, "maul-preview-controller"))
		for res, verbs := range map[string]string{
			"/events": "get,list,watch", "/secrets": "create,delete", "/configmaps": "create,update,delete",
		} {
			if got[res] != verbs {
				t.Errorf("%s: preview-controller %s = %q, want %q", slot, res, got[res], verbs)
			}
		}
	}
	release := grants(onlyRole(t, objs, egressRelease, "maul-preview-controller"))
	if got := release["/configmaps"]; got != "create,delete" {
		t.Errorf("preview-controller release configmaps = %q, want create,delete", got)
	}
	for _, name := range []string{"maul-preview-pods", "maul-preview-jobs"} {
		text := validationsText(findObject(objs, "ValidatingAdmissionPolicy", name))
		for _, img := range []string{toolsImage, browserImage, toolboxImage} {
			if !strings.Contains(text, "'"+img+"'") {
				t.Errorf("%s does not admit %s", name, img)
			}
		}
	}
	deployments := validationsText(findObject(objs, "ValidatingAdmissionPolicy", "maul-preview-deployments"))
	for _, img := range []string{toolsImage, browserImage, toolboxImage} {
		if strings.Contains(deployments, img) {
			t.Errorf("the Deployment rules name %s", img)
		}
	}
	if !strings.Contains(validationsText(findObject(objs, "ValidatingAdmissionPolicy", "maul-preview-jobs")),
		"['tester', 'command', 'seed', 'tools']") {
		t.Error("maul-preview-jobs does not admit the tools role")
	}
	for _, q := range kindNamed(objs, "ResourceQuota", "preview-quota") {
		hard, _, _ := unstructured.NestedStringMap(q.Object, "spec", "hard")
		want := map[string]string{"pods": "11", "services": "5", "requests.cpu": "3750m",
			"requests.memory": "3328Mi", "limits.cpu": "5", "limits.memory": "4608Mi",
			"services.loadbalancers": "0", "services.nodeports": "0", "count/ingresses.networking.k8s.io": "4",
			"count/jobs.batch": "2", "count/networkpolicies.networking.k8s.io": "9"}
		if !maps.Equal(hard, want) {
			t.Errorf("%s quota %v, want %v", q.GetNamespace(), hard, want)
		}
	}
	for _, lr := range kindNamed(objs, "LimitRange", "preview-limits") {
		if got := limitRangeMax(t, lr); got != "1500m/1536Mi" {
			t.Errorf("%s LimitRange max %s, want 1500m/1536Mi", lr.GetNamespace(), got)
		}
	}
	checkToolIsolation(t, objs)
	checkToolConfig(t, renderConfigMaps(t, toolsArgs))
}

// checkToolIsolation: in every slot preview-isolation selects a component
// but not the tool pod, and preview-tools-isolation selects the tool pod
// alone, both directions, with no rule: the tool pod reaches and is reached
// by nothing but what preview-controller's per-verification policies add.
func checkToolIsolation(t *testing.T, objs []*unstructured.Unstructured) {
	t.Helper()
	for _, slot := range []string{"maul-preview-0", "maul-preview-1"} {
		var iso, tools *unstructured.Unstructured
		for _, np := range kindNamed(objs, "NetworkPolicy", "preview-isolation") {
			if np.GetNamespace() == slot {
				iso = np
			}
		}
		for _, np := range kindNamed(objs, "NetworkPolicy", "preview-tools-isolation") {
			if np.GetNamespace() == slot {
				tools = np
			}
		}
		if iso == nil || tools == nil {
			t.Fatalf("%s: preview-isolation %v, preview-tools-isolation %v", slot, iso != nil, tools != nil)
		}
		if !selects(t, iso, componentPodLabels) || selects(t, iso, toolPodLabels) {
			t.Errorf("%s: preview-isolation must select components and leave the tool pod out", slot)
		}
		if selects(t, tools, componentPodLabels) || !selects(t, tools, toolPodLabels) {
			t.Errorf("%s: preview-tools-isolation must select the tool pod alone", slot)
		}
		types, _, _ := unstructured.NestedStringSlice(tools.Object, "spec", "policyTypes")
		if !slices.Equal(types, []string{"Ingress", "Egress"}) {
			t.Errorf("%s: preview-tools-isolation policyTypes %v", slot, types)
		}
		for _, dir := range []string{"ingress", "egress"} {
			if rules, _, _ := unstructured.NestedSlice(tools.Object, "spec", dir); len(rules) != 0 {
				t.Errorf("%s: preview-tools-isolation has %s rules %v", slot, dir, rules)
			}
		}
	}
}

// checkToolConfig: each controller gets the evaluator's settings from the
// values, the cost ceiling in micro-USD as every cost flag takes it.
func checkToolConfig(t *testing.T, cms map[string]map[string]string) {
	t.Helper()
	want := map[string]map[string]string{
		"maul-preview-controller-config": {"MAUL_PREVIEW_EVALUATOR": "true", "MAUL_PREVIEW_TOOLS_IMAGE": toolsImage,
			"MAUL_PREVIEW_BROWSER_IMAGE": browserImage, "MAUL_PREVIEW_TOOLBOX_IMAGE": toolboxImage,
			"MAUL_PREVIEW_AGENTS_NAMESPACE": "maul-agents"},
		"maul-intent-controller-config": {"MAUL_INTENT_EVALUATOR": "true",
			"MAUL_INTENT_EVALUATE_MODEL":    "anthropic/claude-sonnet-5",
			"MAUL_INTENT_CHECKREVIEW_MODEL": "anthropic/claude-sonnet-5",
			"MAUL_INTENT_EVALUATE_TIMEOUT":  "20m", "MAUL_INTENT_DEFAULT_HANDOFF": "gated",
			"MAUL_INTENT_HANDOFF_MAX": "60m", "MAUL_INTENT_EVALUATE_CODEX": "false",
			"MAUL_INTENT_EVALUATION_COST_MAX": "6000000", "MAUL_INTENT_MAX_TOOL_CALLS": "300",
			"MAUL_INTENT_EVALUATE_MAX_TURNS": "100", "MAUL_INTENT_CHECKREVIEW_MAX_TURNS": "6"},
	}
	for cm, kv := range want {
		for k, v := range kv {
			if got := cms[cm][k]; got != v {
				t.Errorf("%s %s = %q, want %q", cm, k, got, v)
			}
		}
	}
}

// TestPreviewToolsRenderRefusals: with the evaluator on, the chart fails to
// render verification without each tool image digest-pinned and clear of the
// preview prefix and every agent registry, with an evaluate timeout the slot
// Job deadline cannot hold, or with out-of-range evaluator settings.
func TestPreviewToolsRenderRefusals(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	with := func(args ...string) []string { return append(append([]string{}, toolsArgs...), args...) }
	type refusal struct {
		name string
		args []string
		want string
	}
	cases := make([]refusal, 0, 20)
	cases = append(cases,
		refusal{"evaluate timeout past the slot deadline", with("--set",
			"intentController.verification.evaluateTimeout=30m"), "intentController.verification.evaluateTimeout"},
		refusal{"too few tool calls", with("--set", "intentController.verification.maxToolCalls=49"),
			"maxToolCalls"},
		refusal{"too many tool calls", with("--set", "intentController.verification.maxToolCalls=301"),
			"maxToolCalls"},
		refusal{"no evaluate turns", with("--set", "intentController.verification.evaluateMaxTurns=0"),
			"evaluateMaxTurns"},
		refusal{"too many evaluate turns", with("--set", "intentController.verification.evaluateMaxTurns=1001"),
			"evaluateMaxTurns"},
		refusal{"no check-review turns", with("--set", "intentController.verification.checkReviewMaxTurns=0"),
			"checkReviewMaxTurns"},
		refusal{"an unknown hand-off", with("--set", "intentController.verification.defaultHandoff=never"),
			"defaultHandoff"},
		refusal{"no evaluation budget", with("--set", "intentController.verification.maxEvaluateCostUSD=0"),
			"maxEvaluateCostUSD"})
	for _, key := range []string{"image", "browserImage", "toolboxImage"} {
		set := "preview.verification.tools." + key + "="
		cases = append(cases,
			refusal{key + " missing", with("--set", set), "preview.verification.tools." + key + " is required"},
			refusal{key + " by tag", with("--set", set+"ghcr.io/devthenet-labs/maul/preview-x:v1"),
				"must be pinned by digest"},
			refusal{key + " under the preview prefix", with("--set", set+previewRegistry+
				"/maul/previews/tools@sha256:"+strings.Repeat("a", 64)), "under the preview image prefix"},
			refusal{key + " under an agent registry", with("--set", set+"registry.example.com/agents/x@sha256:"+
				strings.Repeat("a", 64), "--set-json", `agent.repositoryImages.registries=["registry.example.com/agents"]`),
				"under the agent registry"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all := append([]string{"template", "maul", ".", "--namespace", "maul"}, tc.args...)
			out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("render: %v, want a failure containing %q:\n%s", err, tc.want, out)
			}
		})
	}
}

// limitRangeMax is a LimitRange's Container max as cpu/memory.
func limitRangeMax(t *testing.T, lr *unstructured.Unstructured) string {
	t.Helper()
	limits, _, _ := unstructured.NestedSlice(lr.Object, "spec", "limits")
	for _, l := range limits {
		m, _ := l.(map[string]any)
		if m["type"] == "Container" {
			most, _, _ := unstructured.NestedStringMap(m, "max")
			return most["cpu"] + "/" + most["memory"]
		}
	}
	t.Fatalf("%s has no Container limits", lr.GetNamespace())
	return ""
}

// validationsText is every validation expression of a policy, joined.
func validationsText(p *unstructured.Unstructured) string {
	if p == nil {
		return ""
	}
	validations, _, _ := unstructured.NestedSlice(p.Object, "spec", "validations")
	var b strings.Builder
	for _, v := range validations {
		m, _ := v.(map[string]any)
		expr, _ := m["expression"].(string)
		b.WriteString(expr)
		b.WriteString("\n")
	}
	return b.String()
}
