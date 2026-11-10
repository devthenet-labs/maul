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
)

// testerImage is a digest-pinned image of the retired preview tester: no
// render admits it, and setting preview.verification.testerImage to it (or
// anything) fails the render.
const testerImage = "ghcr.io/devthenet-labs/maul/preview-tester@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// evaluatorSwitchArgs turn the agent evaluator, verification's one switch
// (S4), on with its three tool images.
var evaluatorSwitchArgs = []string{
	"--set", "intentController.verification.evaluator=true",
	"--set", "preview.verification.tools.image=" + toolsImage,
	"--set", "preview.verification.tools.browserImage=" + browserImage,
	"--set", "preview.verification.tools.toolboxImage=" + toolboxImage,
}

// verificationArgs renders the preview runtime with acceptance verification
// on: the agent evaluator.
var verificationArgs = append(append([]string{}, previewRuntimeArgs...), evaluatorSwitchArgs...)

// verificationPolicies are the admission policies verification adds; none
// renders without it.
var verificationPolicies = []string{"maul-preview-jobs", "maul-preview-networkpolicies",
	"maul-preview-verifications"}

// TestPreviewVerificationOffRendersNothing: with verification off (the
// default), the preview runtime carries none of its policies, grants or
// quota keys, and the slot image rule names no exact image.
func TestPreviewVerificationOffRendersNothing(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	objs := renderObjects(t, previewRuntimeArgs)
	for _, u := range objs {
		if slices.Contains(verificationPolicies, u.GetName()) {
			t.Errorf("verification off renders %s %s", u.GetKind(), u.GetName())
		}
	}
	for _, role := range rolesNamed(objs, "maul-preview-controller") {
		if p, ok := grants(role)["/pods"]; ok && p != "list" {
			t.Errorf("verification off: %s/%s grants pods %q, want list", role.GetNamespace(), role.GetName(), p)
		}
		for _, r := range roleRules(role) {
			for _, res := range r.resources {
				if slices.Contains([]string{"jobs", "networkpolicies", "pods/log", "previewverifications",
					"previewverifications/status"}, res) {
					t.Errorf("verification off: %s/%s grants %s", role.GetNamespace(), role.GetName(), res)
				}
			}
		}
	}
	for _, r := range roleRules(onlyRole(t, objs, "maul", "maul-intent-controller")) {
		if slices.Contains(r.resources, "previewverifications") {
			t.Error("verification off: intent-controller is granted previewverifications")
		}
	}
	for _, q := range kindNamed(objs, "ResourceQuota", "preview-quota") {
		hard, _, _ := unstructured.NestedStringMap(q.Object, "spec", "hard")
		want := map[string]string{"pods": "8", "services": "5", "requests.cpu": "1", "requests.memory": "1Gi",
			"limits.cpu": "2", "limits.memory": "2Gi", "services.loadbalancers": "0", "services.nodeports": "0",
			"count/ingresses.networking.k8s.io": "4"}
		if !maps.Equal(hard, want) {
			t.Errorf("%s quota %v, want today's %v", q.GetNamespace(), hard, want)
		}
	}
	if expr := imageRule(t, objs, "maul-preview-pods"); strings.Contains(expr, "@sha256:") {
		t.Errorf("verification off: the pod image rule names an exact image: %s", expr)
	}
}

// TestPreviewVerificationRender: verification on (the evaluator) adds the
// grants §5.8 lists and the three fences, and no slot image rule admits an
// exact image beside the preview prefix: the tool images have rules of
// their own, and the retired tester's image is admitted nowhere. The tool
// pod's own grants, quota and LimitRange are TestPreviewToolsRender's.
func TestPreviewVerificationRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	objs := renderObjects(t, verificationArgs)
	checkVerificationPolicies(t, objs)
	checkVerificationGrants(t, objs)
	for _, name := range []string{"maul-preview-pods", "maul-preview-jobs", "maul-preview-deployments"} {
		if expr := imageRule(t, objs, name); strings.Contains(expr, "c.image in") ||
			strings.Contains(expr, "@sha256:") || strings.Contains(expr, "preview-tester") {
			t.Errorf("%s image rule names an exact image: %s", name, expr)
		}
	}
}

// checkVerificationPolicies: each of the three fences renders once, with one
// binding.
func checkVerificationPolicies(t *testing.T, objs []*unstructured.Unstructured) {
	t.Helper()
	for _, name := range verificationPolicies {
		if len(kindNamed(objs, "ValidatingAdmissionPolicy", name)) != 1 {
			t.Errorf("no ValidatingAdmissionPolicy %s", name)
		}
		bindings := 0
		for _, u := range objs {
			if u.GetKind() == "ValidatingAdmissionPolicyBinding" {
				if p, _, _ := unstructured.NestedString(u.Object, "spec", "policyName"); p == name {
					bindings++
				}
			}
		}
		if bindings != 1 {
			t.Errorf("%s has %d bindings, want 1", name, bindings)
		}
	}
}

// checkVerificationGrants: §5.8's grants (the tool pod's Secret and
// ConfigMap grants beside them are TestPreviewToolsRender's).
func checkVerificationGrants(t *testing.T, objs []*unstructured.Unstructured) {
	t.Helper()
	for _, slot := range []string{"maul-preview-0", "maul-preview-1"} {
		got := grants(onlyRole(t, objs, slot, "maul-preview-controller"))
		for res, verbs := range map[string]string{
			"batch/jobs":                        "create,get,list,delete",
			"networking.k8s.io/networkpolicies": "create,get,list,update,delete",
			"/pods/log":                         "get",
			"/pods":                             "list,delete",
		} {
			if got[res] != verbs {
				t.Errorf("%s: preview-controller %s = %q, want %q", slot, res, got[res], verbs)
			}
		}
	}
	release := grants(onlyRole(t, objs, "maul", "maul-preview-controller"))
	if release["maul.run/previewverifications"] != "get,list,watch" ||
		release["maul.run/previewverifications/status"] != "update" {
		t.Errorf("preview-controller release grants %v", release)
	}
	ic := grants(onlyRole(t, objs, "maul", "maul-intent-controller"))
	if ic["maul.run/previewverifications"] != "create,get,list,watch,delete" {
		t.Errorf("intent-controller previewverifications = %q", ic["maul.run/previewverifications"])
	}
}

// TestPreviewQuotaIsValuesDriven: each quota value can be set, and load
// balancers and node ports stay zero whatever is set.
func TestPreviewQuotaIsValuesDriven(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	objs := renderObjects(t, append(append([]string{}, verificationArgs...),
		"--set", "preview.quota.pods=16", "--set", "preview.quota.services=8",
		"--set", "preview.quota.requests.cpu=2", "--set", "preview.quota.limits.memory=4Gi",
		"--set", "preview.quota.jobs=3", "--set", "preview.quota.networkPolicies=12"))
	for _, q := range kindNamed(objs, "ResourceQuota", "preview-quota") {
		hard, _, _ := unstructured.NestedStringMap(q.Object, "spec", "hard")
		for k, v := range map[string]string{"pods": "16", "services": "8", "requests.cpu": "2",
			"limits.memory": "4Gi", "count/jobs.batch": "3", "count/networkpolicies.networking.k8s.io": "12",
			"services.loadbalancers": "0", "services.nodeports": "0", "limits.cpu": "5"} {
			if hard[k] != v {
				t.Errorf("%s quota %s = %q, want %q", q.GetNamespace(), k, hard[k], v)
			}
		}
	}
}

// TestPreviewVerificationRenderRefusals: the chart fails to render
// verification with the removed preview.verification.enabled set, whatever
// its value and whatever else is set (S4: the evaluator is verification's
// one switch, and the message names it), or with the removed
// preview.verification.testerImage set, whatever its value, evaluator on or
// off, previews on or off (S4: the preview tester is deleted).
func TestPreviewVerificationRenderRefusals(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	on := evaluatorSwitchArgs
	foundation := []string{"-f", "../../hack/testdata/chart-render/preview-foundation.yaml"}
	const removed = "preview.verification.enabled is removed"
	const testerRemoved = "preview.verification.testerImage is removed"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"the removed switch, slice 1 alone", append(append([]string{}, previewRuntimeArgs...),
			"--set", "preview.verification.enabled=true",
			"--set", "preview.verification.testerImage="+testerImage), removed},
		{"the removed switch beside the evaluator", append(append([]string{}, verificationArgs...),
			"--set", "preview.verification.enabled=true"), "intentController.verification.evaluator"},
		{"the removed switch, false", append(append([]string{}, verificationArgs...),
			"--set", "preview.verification.enabled=false"), "docs/intents/evaluator.md#migrating-from-slice-1"},
		{"the removed switch without previews", append(append([]string{}, foundation...),
			"--set", "preview.verification.enabled=true"), removed},
		{"a stamped tester beside the evaluator", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage="+testerImage), testerRemoved},
		{"a stamped tester names the migration", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage="+testerImage),
			"docs/intents/evaluator.md#migrating-from-slice-1"},
		{"a stamped tester alone", append(append([]string{}, previewRuntimeArgs...),
			"--set", "preview.verification.testerImage="+testerImage), testerRemoved},
		{"an empty tester image", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage="), testerRemoved},
		{"a tester image by tag", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage=ghcr.io/devthenet-labs/maul/preview-tester:v1"), testerRemoved},
		{"a tester image without previews", append(append([]string{}, foundation...),
			"--set", "preview.verification.testerImage="+testerImage), testerRemoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := append([]string{"template", "maul", ".", "--namespace", "maul"}, tc.args...)
			out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("render: %v, want a failure containing %q:\n%s", err, tc.want, out)
			}
		})
	}
}

// rule is one Role rule, flattened for comparison.
type rule struct{ groups, resources, verbs []string }

func roleRules(u *unstructured.Unstructured) []rule {
	if u == nil {
		return nil
	}
	raw, _, _ := unstructured.NestedSlice(u.Object, "rules")
	var out []rule
	for _, r := range raw {
		m, _ := r.(map[string]any)
		strs := func(k string) []string {
			s, _, _ := unstructured.NestedStringSlice(m, k)
			return s
		}
		out = append(out, rule{strs("apiGroups"), strs("resources"), strs("verbs")})
	}
	return out
}

// grants maps group/resource to the comma-joined verbs of the rule naming it.
func grants(u *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	for _, r := range roleRules(u) {
		for _, g := range r.groups {
			for _, res := range r.resources {
				out[g+"/"+res] = strings.Join(r.verbs, ",")
			}
		}
	}
	return out
}

func kindNamed(objs []*unstructured.Unstructured, kind, name string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, u := range objs {
		if u.GetKind() == kind && u.GetName() == name {
			out = append(out, u)
		}
	}
	return out
}

func rolesNamed(objs []*unstructured.Unstructured, name string) []*unstructured.Unstructured {
	return kindNamed(objs, "Role", name)
}

func onlyRole(t *testing.T, objs []*unstructured.Unstructured, ns, name string) *unstructured.Unstructured {
	t.Helper()
	for _, u := range rolesNamed(objs, name) {
		if u.GetNamespace() == ns {
			return u
		}
	}
	t.Fatalf("no Role %s/%s", ns, name)
	return nil
}

// imageRule is the expression of a slot policy's image validation.
func imageRule(t *testing.T, objs []*unstructured.Unstructured, policy string) string {
	t.Helper()
	for _, u := range kindNamed(objs, "ValidatingAdmissionPolicy", policy) {
		validations, _, _ := unstructured.NestedSlice(u.Object, "spec", "validations")
		for _, v := range validations {
			m, _ := v.(map[string]any)
			if msg, _ := m["message"].(string); strings.HasPrefix(msg, "only immutable") {
				expr, _ := m["expression"].(string)
				return expr
			}
		}
	}
	t.Fatalf("%s has no image rule", policy)
	return ""
}

// TestPreviewVerificationControllerConfig: the verification settings reach
// both binaries only with the agent evaluator, verification's one switch.
// Off (the default) neither ConfigMap carries a verification key, so
// intent-controller never writes a request. On, preview-controller gets its
// deadline bound, and intent-controller the evaluator with its timeout
// bound, both from deadlineMaxSeconds: the timeout a request carries can
// then never exceed the deadline the slot Job admission policy allows. The
// removed MAUL_INTENT_VERIFICATION and MAUL_PREVIEW_TESTER_IMAGE, which the
// controllers refuse, are never rendered.
func TestPreviewVerificationControllerConfig(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	keys := map[string][]string{
		"maul-preview-controller-config": {"MAUL_PREVIEW_TESTER_IMAGE", "MAUL_PREVIEW_VERIFY_DEADLINE_MAX"},
		"maul-intent-controller-config": {"MAUL_INTENT_VERIFICATION", "MAUL_INTENT_VERIFY_TIMEOUT_MAX",
			"MAUL_INTENT_EVALUATOR"},
	}
	off := renderConfigMaps(t, previewRuntimeArgs)
	for cm, ks := range keys {
		if _, ok := off[cm]; !ok {
			t.Fatalf("no ConfigMap %s rendered", cm)
		}
		for _, k := range ks {
			if v, ok := off[cm][k]; ok {
				t.Errorf("verification off: %s carries %s=%q", cm, k, v)
			}
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default bound", verificationArgs, "1800s"},
		{"own bound", append(append([]string{}, verificationArgs...),
			"--set", "preview.verification.deadlineMaxSeconds=600",
			"--set", "intentController.verification.evaluateTimeout=5m"), "600s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			on := renderConfigMaps(t, tc.args)
			want := map[string]map[string]string{
				"maul-preview-controller-config": {"MAUL_PREVIEW_VERIFY_DEADLINE_MAX": tc.want},
				"maul-intent-controller-config": {"MAUL_INTENT_EVALUATOR": "true",
					"MAUL_INTENT_VERIFY_TIMEOUT_MAX": tc.want},
			}
			for cm, kv := range want {
				for k, v := range kv {
					if got := on[cm][k]; got != v {
						t.Errorf("%s %s = %q, want %q", cm, k, got, v)
					}
				}
			}
			if v, ok := on["maul-intent-controller-config"]["MAUL_INTENT_VERIFICATION"]; ok {
				t.Errorf("the removed MAUL_INTENT_VERIFICATION rendered (%q): intent-controller refuses it", v)
			}
			if v, ok := on["maul-preview-controller-config"]["MAUL_PREVIEW_TESTER_IMAGE"]; ok {
				t.Errorf("the removed MAUL_PREVIEW_TESTER_IMAGE rendered (%q): preview-controller refuses it", v)
			}
		})
	}
}

// TestPreviewVerificationDeadlineBounds: deadlineMaxSeconds is held by the
// schema to the range both controllers accept for the flags it renders
// (1m to 1h), so an out-of-range value fails the render instead of a
// controller's startup.
func TestPreviewVerificationDeadlineBounds(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	// An evaluation round must fit under the deadline, so the round is held
	// under the least the schema admits: only the schema bound is tested.
	render := func(v string) ([]byte, error) {
		all := append(append([]string{"template", "maul", ".", "--namespace", "maul"}, verificationArgs...),
			"--set", "preview.verification.deadlineMaxSeconds="+v,
			"--set", "intentController.verification.evaluateTimeout=30s")
		return exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
	}
	for _, v := range []string{"59", "3601"} {
		if out, err := render(v); err == nil || !strings.Contains(string(out), "deadlineMaxSeconds") {
			t.Errorf("deadlineMaxSeconds=%s: %v, want a schema failure naming it:\n%s", v, err, out)
		}
	}
	for _, v := range []string{"60", "3600"} {
		if out, err := render(v); err != nil {
			t.Errorf("deadlineMaxSeconds=%s: %v\n%s", v, err, out)
		}
	}
}
