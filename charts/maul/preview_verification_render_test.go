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

// testerImage is the digest-pinned preview-tester image the verification
// fixture admits; it lies outside the preview prefix and every agent
// registry.
const testerImage = "ghcr.io/devthenet-labs/maul/preview-tester@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// verificationArgs renders the preview runtime with acceptance verification
// on.
var verificationArgs = append(append([]string{}, previewRuntimeArgs...),
	"--set", "preview.verification.enabled=true",
	"--set", "preview.verification.testerImage="+testerImage)

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

// TestPreviewVerificationRender: verification on adds exactly the grants
// §5.8 lists, the three fences, the tester's exact image to the pod and Job
// image rules (never the Deployments'), and the slice-1 quota.
func TestPreviewVerificationRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	objs := renderObjects(t, verificationArgs)
	checkVerificationPolicies(t, objs)
	checkVerificationGrants(t, objs)
	for _, name := range []string{"maul-preview-pods", "maul-preview-jobs"} {
		if expr := imageRule(t, objs, name); !strings.Contains(expr, `"`+testerImage+`"`) {
			t.Errorf("%s image rule does not admit the tester image: %s", name, expr)
		}
	}
	if expr := imageRule(t, objs, "maul-preview-deployments"); strings.Contains(expr, "@sha256:") {
		t.Errorf("the Deployment image rule names an exact image: %s", expr)
	}
	for _, q := range kindNamed(objs, "ResourceQuota", "preview-quota") {
		hard, _, _ := unstructured.NestedStringMap(q.Object, "spec", "hard")
		want := map[string]string{"pods": "10", "services": "5", "requests.cpu": "1250m",
			"requests.memory": "1280Mi", "limits.cpu": "2500m", "limits.memory": "2560Mi",
			"services.loadbalancers": "0", "services.nodeports": "0", "count/ingresses.networking.k8s.io": "4",
			"count/jobs.batch": "1", "count/networkpolicies.networking.k8s.io": "6"}
		if !maps.Equal(hard, want) {
			t.Errorf("%s quota %v, want %v", q.GetNamespace(), hard, want)
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

// checkVerificationGrants: exactly §5.8's grants, and still no Secret or
// ConfigMap for preview-controller.
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
		for res := range got {
			if strings.Contains(res, "secrets") || strings.Contains(res, "configmaps") {
				t.Errorf("%s: preview-controller is granted %s", slot, res)
			}
		}
	}
	release := grants(onlyRole(t, objs, "maul", "maul-preview-controller"))
	if release["maul.run/previewverifications"] != "get,list,watch,update" ||
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
			"services.loadbalancers": "0", "services.nodeports": "0", "limits.cpu": "2500m"} {
			if hard[k] != v {
				t.Errorf("%s quota %s = %q, want %q", q.GetNamespace(), k, hard[k], v)
			}
		}
	}
}

// TestPreviewVerificationRenderRefusals: the chart fails to render
// verification without the controllers it needs or with a tester image that
// is not digest-pinned or lies under the preview prefix or an agent
// registry (invariant 17, made symmetric).
func TestPreviewVerificationRenderRefusals(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	on := []string{"--set", "preview.verification.enabled=true"}
	foundation := []string{"-f", "../../hack/testdata/chart-render/preview-foundation.yaml"}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no preview-controller", append(append(append([]string{}, foundation...), on...),
			"--set", "preview.verification.testerImage="+testerImage), "requires previewController.enabled"},
		{"no tester image", append(append([]string{}, previewRuntimeArgs...), on...),
			"preview.verification.testerImage is required"},
		{"tag only", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage=ghcr.io/devthenet-labs/maul/preview-tester:v1"),
			"must be pinned by digest"},
		{"bare name", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage=preview-tester@sha256:"+strings.Repeat("a", 64)),
			"must be pinned by digest"},
		{"short digest", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage=ghcr.io/maul/tester@sha256:abc"),
			"must be pinned by digest"},
		{"under the preview prefix", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage="+previewRegistry+"/maul/previews/tester@sha256:"+
				strings.Repeat("a", 64)), "under the preview image prefix"},
		{"under the preview prefix, other spelling", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage=111122223333.dkr.ecr-fips.us-east-1.amazonaws.com:443/"+
				"maul/previews/tester:v1@sha256:"+strings.Repeat("a", 64)), "under the preview image prefix"},
		{"under an agent registry", append(append(append([]string{}, previewRuntimeArgs...), on...),
			"--set", "preview.verification.testerImage="+testerImage,
			"--set-json", `agent.repositoryImages.registries=["ghcr.io/devthenet-labs/maul"]`),
			"under the agent registry"},
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

// TestPreviewVerificationControllerConfig: the verification switch reaches
// both binaries only with preview.verification.enabled. Off (the default)
// neither ConfigMap carries a verification key, so preview-controller has no
// tester image and intent-controller never writes a request. On,
// preview-controller gets the tester image and its deadline bound, and
// intent-controller --intent-verification with its timeout bound, both from
// deadlineMaxSeconds: the timeout a request carries can then never exceed
// the deadline the slot Job admission policy allows.
func TestPreviewVerificationControllerConfig(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	keys := map[string][]string{
		"maul-preview-controller-config": {"MAUL_PREVIEW_TESTER_IMAGE", "MAUL_PREVIEW_VERIFY_DEADLINE_MAX"},
		"maul-intent-controller-config":  {"MAUL_INTENT_VERIFICATION", "MAUL_INTENT_VERIFY_TIMEOUT_MAX"},
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
			"--set", "preview.verification.deadlineMaxSeconds=600"), "600s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			on := renderConfigMaps(t, tc.args)
			want := map[string]map[string]string{
				"maul-preview-controller-config": {"MAUL_PREVIEW_TESTER_IMAGE": testerImage,
					"MAUL_PREVIEW_VERIFY_DEADLINE_MAX": tc.want},
				"maul-intent-controller-config": {"MAUL_INTENT_VERIFICATION": "true",
					"MAUL_INTENT_VERIFY_TIMEOUT_MAX": tc.want},
			}
			for cm, kv := range want {
				for k, v := range kv {
					if got := on[cm][k]; got != v {
						t.Errorf("%s %s = %q, want %q", cm, k, got, v)
					}
				}
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
	render := func(v string) ([]byte, error) {
		all := append(append([]string{"template", "maul", ".", "--namespace", "maul"}, verificationArgs...),
			"--set", "preview.verification.deadlineMaxSeconds="+v)
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
