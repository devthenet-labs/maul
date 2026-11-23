// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"slices"
	"strings"
	"testing"
)

// TestEvaluatePromptLooksWhereAWrongBuildDiffers (S1 demo #57-#60, as the
// observed evaluator states it): every evaluate prompt asks the evaluator
// to try an outcome where a careless build would answer otherwise: a
// left-out item created and looked for beside a kept one, both sides of a
// rule and of a bound, inputs other than the defaults, and the wrong builds
// named.
func TestEvaluatePromptLooksWhereAWrongBuildDiffers(t *testing.T) {
	shared := []string{
		"Try the outcome where a careless build would get it wrong: inputs other than the defaults, both sides of " +
			"a rule",
		"each side of a bound",
		"The wrong builds to look for: the value fixed or left at its default",
		"the rule right only on the example, the bound off by one",
	}
	for name, tt := range map[string]struct {
		p    EvaluatePrompt
		want []string
	}{
		"slot": {testEvaluatePrompt(), []string{"create the item that must be left out and look for it beside one " +
			"that is there, so an empty answer does not pass", "for a change, the state before and after it"}},
		"cli": {testWorkspacePrompt(testCLIWorkspace), []string{"the input that must be left out beside one that " +
			"is kept"}},
		"library": {testWorkspacePrompt(testLibraryWorkspace), []string{"the input that must be left out beside " +
			"one that is kept"}},
	} {
		got, err := RenderEvaluatePrompt(tt.p)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range slices.Concat(shared, tt.want) {
			if !strings.Contains(flat(got), want) {
				t.Errorf("%s: prompt lacks %q", name, want)
			}
		}
	}
}
