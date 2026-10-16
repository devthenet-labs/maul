// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// matchesArg is the argument of every CEL matches() call in an expression.
var matchesArg = regexp.MustCompile(`matches\('([^']*)'\)`)

// TestAttributionJobGrammarIsTheControllers: the annotation grammar the
// rendered Job policy holds every launch to is attribution.ValuePattern, the
// one the controllers' values are pinned to (a seeded property there), so
// neither side can change without the other: a wider Go grammar would have
// launches refused at admission, a wider chart one would admit values no
// controller writes.
func TestAttributionJobGrammarIsTheControllers(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := append([]string{"template", "patchy", ".", "--namespace", "patchy"}, attributionArgs...)
	output, err := exec.CommandContext(t.Context(), "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var grammars []string
	found := false
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() != "ValidatingAdmissionPolicy" || u.GetName() != "patchy-attribution-jobs" {
			continue
		}
		found = true
		validations, _, _ := unstructured.NestedSlice(u.Object, "spec", "validations")
		for _, v := range validations {
			expr, _ := v.(map[string]any)["expression"].(string)
			for _, m := range matchesArg.FindAllStringSubmatch(expr, -1) {
				grammars = append(grammars, m[1])
			}
		}
	}
	if !found {
		t.Fatal("the attribution render has no patchy-attribution-jobs policy")
	}
	if len(grammars) == 0 {
		t.Fatal("the Job policy holds the annotation to no grammar")
	}
	for _, g := range grammars {
		if g != attribution.ValuePattern {
			t.Errorf("the Job policy's grammar is %q, want attribution.ValuePattern %q", g, attribution.ValuePattern)
		}
	}
}
