// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// evaluatorOnArgs turns the agent evaluator on beside intent-controller.
var evaluatorOnArgs = []string{
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"--set", "intentController.verification.evaluator=true",
}

// evaluatorLabelPolicies are the fence's objects, each rendered as a policy
// and a binding of the same name.
var evaluatorLabelPolicies = []string{"maul-evaluator-labels-jobs", "maul-evaluator-labels-pods"}

// TestEvaluatorLabelsPolicyRenders: the maul-evaluator-labels fence renders
// only with the evaluator on, fails closed, denies, and binds the agents
// namespace and, with previews on, exactly the four guarded slot names; the
// Job policy names intent-controller and, with previews, preview-controller
// as the only writers, and every fenced label key.
func TestEvaluatorLabelsPolicyRenders(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	slots := []string{"maul-preview-0", "maul-preview-1", "maul-preview-2", "maul-preview-3"}
	for _, tc := range []struct {
		name       string
		args       []string
		on         bool
		namespaces []string
		writers    []string
	}{
		{"off by default", []string{"-f", "../../hack/testdata/chart-render/intent-controller.yaml"}, false,
			nil, nil},
		{"off with previews", previewRuntimeArgs, false, nil, nil},
		{"on, agents namespace only", evaluatorOnArgs, true, []string{egressAgents}, []string{saIntent}},
		{"on with previews", append(append([]string{}, previewRuntimeArgs...),
			"--set", "intentController.verification.evaluator=true"), true,
			append([]string{egressAgents}, slots...), []string{saIntent, saPreview}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := renderObjects(t, tc.args)
			for _, name := range evaluatorLabelPolicies {
				policy := findObject(objs, "ValidatingAdmissionPolicy", name)
				binding := findObject(objs, "ValidatingAdmissionPolicyBinding", name)
				if !tc.on {
					if policy != nil || binding != nil {
						t.Errorf("%s renders with the evaluator off", name)
					}
					continue
				}
				if policy == nil || binding == nil {
					t.Fatalf("%s: policy %v, binding %v; want both", name, policy != nil, binding != nil)
				}
				if fp, _, _ := unstructured.NestedString(policy.Object, "spec", "failurePolicy"); fp != "Fail" {
					t.Errorf("%s failurePolicy %q, want Fail", name, fp)
				}
				if ref, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName"); ref != name {
					t.Errorf("binding %s names policy %q", name, ref)
				}
				actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
				if !slices.Equal(actions, []string{"Deny"}) {
					t.Errorf("binding %s actions %v, want [Deny]", name, actions)
				}
				if got := boundNamespaces(binding); !slices.Equal(got, tc.namespaces) {
					t.Errorf("binding %s namespaces %v, want %v", name, got, tc.namespaces)
				}
				text := policyText(policy)
				for _, key := range []string{"maul.run/role", "maul.run/evaluation", "maul.run/preview-role"} {
					if !strings.Contains(text, key) {
						t.Errorf("%s never judges %s", name, key)
					}
				}
				if name != "maul-evaluator-labels-jobs" {
					continue
				}
				for _, sa := range []string{saIntent, saPreview} {
					want := slices.Contains(tc.writers, sa)
					if strings.Contains(text, sa) != want {
						t.Errorf("%s names writer %s: %v, want %v", name, sa, !want, want)
					}
				}
			}
		})
	}
}

func findObject(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

// boundNamespaces is the binding's kubernetes.io/metadata.name In values.
func boundNamespaces(binding *unstructured.Unstructured) []string {
	exprs, _, _ := unstructured.NestedSlice(binding.Object, "spec", "matchResources", "namespaceSelector",
		"matchExpressions")
	var names []string
	for _, e := range exprs {
		m, _ := e.(map[string]any)
		if m["key"] != "kubernetes.io/metadata.name" || m["operator"] != "In" {
			continue
		}
		vals, _ := m["values"].([]any)
		for _, v := range vals {
			s, _ := v.(string)
			names = append(names, s)
		}
	}
	return names
}

// policyText is every CEL expression of the policy, joined.
func policyText(policy *unstructured.Unstructured) string {
	var b strings.Builder
	for _, field := range []string{"variables", "validations", "matchConditions"} {
		items, _, _ := unstructured.NestedSlice(policy.Object, "spec", field)
		for _, it := range items {
			m, _ := it.(map[string]any)
			s, _ := m["expression"].(string)
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
