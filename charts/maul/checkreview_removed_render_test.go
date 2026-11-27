// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestCheckReviewKeysFailTheRender: the check reviewer's settings,
// intentController.verification.checkReviewModel and checkReviewMaxTurns,
// are removed (0.14.18 refused them with a named render failure; this
// release drops them from values.schema.json). Either one set still fails
// the render, whatever its value (the old default, zero, empty) and
// whatever else is set (the evaluator on previews or on the agents tool
// pod, the evaluator off, intent-controller off): the schema admits no
// other key there, and the failure names the key.
func TestCheckReviewKeysFailTheRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	bases := map[string][]string{
		"defaults":                  nil,
		"intent-controller alone":   {"-f", "../../hack/testdata/chart-render/intent-controller.yaml"},
		"the evaluator on previews": toolsArgs,
		"the agents evaluator":      agentsArgs,
	}
	for _, key := range []struct{ name, set string }{
		{"checkReviewModel", "--set=intentController.verification.checkReviewModel=anthropic/claude-sonnet-5-5"},
		{"checkReviewModel", "--set-string=intentController.verification.checkReviewModel="},
		{"checkReviewMaxTurns", "--set=intentController.verification.checkReviewMaxTurns=6"},
		{"checkReviewMaxTurns", "--set=intentController.verification.checkReviewMaxTurns=0"},
	} {
		for base, args := range bases {
			all := slices.Concat([]string{"template", "maul", ".", "--namespace", "maul"}, args,
				[]string{key.set})
			out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
			if err == nil {
				t.Errorf("%s, %s: the render passed, want it refused", base, key.set)
				continue
			}
			if !strings.Contains(string(out), key.name) {
				t.Errorf("%s, %s: the render failed without naming %q:\n%s", base, key.set, key.name, out)
			}
		}
	}
}

// TestNoCheckReviewSettingRenders: no configuration renders a check
// reviewer's setting for intent-controller, the evaluator on or off.
func TestNoCheckReviewSettingRenders(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	for name, args := range map[string][]string{
		"defaults":                  nil,
		"intent-controller alone":   {"-f", "../../hack/testdata/chart-render/intent-controller.yaml"},
		"the evaluator on previews": toolsArgs,
		"the agents evaluator":      agentsArgs,
	} {
		for cm, data := range renderConfigMaps(t, args) {
			for k := range data {
				if strings.HasPrefix(k, "MAUL_INTENT_CHECKREVIEW_") {
					t.Errorf("%s: %s renders %s", name, cm, k)
				}
			}
		}
	}
}
