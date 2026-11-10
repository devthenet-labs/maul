// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// evaluatorOnlyArgs renders acceptance verification with the agent
// evaluator on and no tester image: toolsArgs less the tester.
var evaluatorOnlyArgs = append(append([]string{}, previewRuntimeArgs...),
	"--set", "intentController.verification.evaluator=true",
	"--set", "preview.verification.tools.image="+toolsImage,
	"--set", "preview.verification.tools.browserImage="+browserImage,
	"--set", "preview.verification.tools.toolboxImage="+toolboxImage)

// TestPreviewToolsRenderWithoutTheTester: the evaluator needs no tester
// image. Without one the chart renders exactly what it renders with one,
// but for the tester itself: preview-controller is given no tester image
// (and so its config checksum moves), and the slot pod and Job image rules
// admit no exact image beside the preview prefix. Every tool rule, grant,
// fence, quota and isolation policy is the same object.
func TestPreviewToolsRenderWithoutTheTester(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	with, without := renderObjects(t, toolsArgs), renderObjects(t, evaluatorOnlyArgs)
	index := func(objs []*unstructured.Unstructured) map[string]*unstructured.Unstructured {
		m := make(map[string]*unstructured.Unstructured, len(objs))
		for _, o := range objs {
			m[o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName()] = o
		}
		return m
	}
	want, got := index(with), index(without)
	if len(want) != len(got) {
		t.Errorf("%d objects without the tester, %d with", len(got), len(want))
	}
	const pcConfig = "ConfigMap/maul/maul-preview-controller-config"
	if got[pcConfig] == nil {
		t.Fatalf("no %s", pcConfig)
	}
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("%s renders only with the tester", key)
			continue
		}
		w, g = withoutTester(t, key, w), g.DeepCopy()
		if key == pcDeployment {
			unstructured.RemoveNestedField(g.Object, checksumPath...)
		}
		if !reflect.DeepEqual(g.Object, w.Object) {
			t.Errorf("%s differs without the tester beyond the tester itself", key)
		}
	}
	cm := got[pcConfig]
	if v, ok, _ := unstructured.NestedString(cm.Object, "data", "MAUL_PREVIEW_TESTER_IMAGE"); ok {
		t.Errorf("preview-controller is given a tester image %q", v)
	}
	if v, _, _ := unstructured.NestedString(cm.Object, "data", "MAUL_PREVIEW_EVALUATOR"); v != "true" {
		t.Errorf("MAUL_PREVIEW_EVALUATOR = %q, want true", v)
	}
	for _, name := range []string{"maul-preview-pods", "maul-preview-jobs", "maul-preview-deployments"} {
		if expr := imageRule(t, without, name); strings.Contains(expr, "c.image in") ||
			strings.Contains(expr, "preview-tester") {
			t.Errorf("%s admits an exact image without a tester: %s", name, expr)
		}
	}
	checkToolConfig(t, renderConfigMaps(t, evaluatorOnlyArgs))
}

// pcDeployment is preview-controller's Deployment, whose config checksum
// (checksumPath) follows its config.
const pcDeployment = "Deployment/maul/maul-preview-controller"

var checksumPath = []string{"spec", "template", "metadata", "annotations", "checksum/config"}

// withoutTester is object o of the render with a tester image as it renders
// without one: no tester key in preview-controller's config, no config
// checksum on its Deployment (the config differs), and no exact tester
// image in the slot pod and Job image rules.
func withoutTester(t *testing.T, key string, o *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	o = o.DeepCopy()
	switch key {
	case "ConfigMap/maul/maul-preview-controller-config":
		unstructured.RemoveNestedField(o.Object, "data", "MAUL_PREVIEW_TESTER_IMAGE")
	case pcDeployment:
		if _, ok, _ := unstructured.NestedString(o.Object, checksumPath...); !ok {
			t.Fatalf("%s has no config checksum", key)
		}
		unstructured.RemoveNestedField(o.Object, checksumPath...)
	case "ValidatingAdmissionPolicy//maul-preview-pods", "ValidatingAdmissionPolicy//maul-preview-jobs":
		validations, _, _ := unstructured.NestedSlice(o.Object, "spec", "validations")
		exact := `c.image in ["` + testerImage + `"] ||`
		found := false
		for _, v := range validations {
			m, _ := v.(map[string]any)
			expr, _ := m["expression"].(string)
			if strings.Contains(expr, exact) {
				found = true
				m["expression"] = strings.Replace(expr, "\n  "+exact, "", 1)
			}
		}
		if !found {
			t.Fatalf("%s does not admit the tester image", key)
		}
		if err := unstructured.SetNestedSlice(o.Object, validations, "spec", "validations"); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

// TestPreviewToolsTesterStillChecked: with the evaluator on, a tester image
// is optional, but one that is set is still held to the tester's rules, and
// the evaluator without its tool images is still refused.
func TestPreviewToolsTesterStillChecked(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	with := func(args ...string) []string { return append(append([]string{}, evaluatorOnlyArgs...), args...) }
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"tag only", with("--set", "preview.verification.testerImage=ghcr.io/devthenet-labs/maul/preview-tester:v1"),
			"must be pinned by digest"},
		{"under the preview prefix", with("--set", "preview.verification.testerImage="+previewRegistry+
			"/maul/previews/tester@sha256:"+strings.Repeat("a", 64)), "under the preview image prefix"},
		{"no tools image", with("--set", "preview.verification.tools.image="),
			"preview.verification.tools.image is required"},
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
