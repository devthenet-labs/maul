// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestEvaluatePromptAsksForProofOfARedCheck (gate D-B9): a saved check saved
// to show a defect is no proof that its outcome holds once it replays
// green, so both evaluate prompts list such checks, as data in a fence,
// bounded, and lift the rule against new checks for an outcome whose saved
// check replayed green for exactly them, asking for a new check held to the
// green rubric and never a dispute or retirement of the old one. With none
// listed, neither the section nor the exception is there.
func TestEvaluatePromptAsksForProofOfARedCheck(t *testing.T) {
	for name, base := range map[string]func() EvaluatePrompt{
		"preview": testEvaluatePrompt,
		"cli":     func() EvaluatePrompt { return testWorkspacePrompt(testCLIWorkspace) },
		"library": func() EvaluatePrompt { return testWorkspacePrompt(testLibraryWorkspace) },
	} {
		t.Run(name, func(t *testing.T) {
			p := base()
			got, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, absent := range []string{"Saved checks that are not yet proof", "or it is listed under"} {
				if strings.Contains(flat(got), absent) {
					t.Errorf("a round with no red check is told %q", absent)
				}
			}
			for i := range v1alpha1.MaxSavedChecks + 3 {
				p.NotYetProof = append(p.NotYetProof, NotYetProof{Check: fmt.Sprintf("swing-red-%d", i),
					Outcome: "swing-shown"})
			}
			if got, err = RenderEvaluatePrompt(p); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"### Saved checks that are not yet proof",
				"- check swing-red-0, of outcome swing-shown",
				"When it replays green, that is not yet evidence that its outcome holds",
				"write a new check that shows the outcome holds, held to **What a check must cover** like any " +
					"other, run it green, propose it, and cite its run in the verdict.",
				"Leave the saved check as it is: do not dispute or retire it.",
				"write no new check for its outcome unless the change since touched it or it is listed under " +
					"**Saved checks that are not yet proof**.",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			if n := strings.Count(got, ", of outcome swing-shown\n"); n != NotYetProofMax {
				t.Errorf("%d checks listed, want %d", n, NotYetProofMax)
			}
			if strings.Contains(strings.Join(unfenced(got), "\n"), "swing-red-0") {
				t.Error("a listed check's id is outside its fence")
			}
		})
	}
}

// TestEvaluatePromptStatesTheClock (gate D-B8): both evaluate prompts tell
// the evaluator that maul-tool shows the time left, when to write its
// report by it, and to report an outcome unsure, naming what no check could
// express, rather than spend the run past its budget on it.
func TestEvaluatePromptStatesTheClock(t *testing.T) {
	for name, base := range map[string]func() EvaluatePrompt{
		"preview": testEvaluatePrompt,
		"cli":     func() EvaluatePrompt { return testWorkspacePrompt(testCLIWorkspace) },
	} {
		for _, tt := range []struct {
			clock time.Duration
			by    string
		}{{16 * time.Minute, "2 minutes"}, {48 * time.Minute, "6 minutes"}, {5 * time.Minute, "2 minutes"}} {
			p := base()
			p.Limits.Timeout = tt.clock
			got, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"Every `maul-tool` call prints how much of that wall clock is left on its `[maul-tool time left]` line.",
				"or once `maul-tool` shows " + tt.by + " or less left, whatever is left",
				"Past its budget, stop working on an outcome whose check still will not run as the outcome needs",
				"report it unsure, and say in its note what no check could express.",
				"one stopped by its wall clock only the few minutes maul kept for it, if any.",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("%s, %s: prompt lacks %q", name, tt.clock, want)
				}
			}
		}
	}
}
