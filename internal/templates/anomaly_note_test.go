// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// A look-around anomaly is a note for the human reviewer (owner decision of
// 2026-11-14): the evaluator is told so, and that a defect in what an
// outcome states is that outcome's fail, never an anomaly; the comment shows
// the note and says it decides nothing.

// TestEvaluatePromptAnomaliesAreNotes: both evaluate prompts say anomalies
// are notes that decide nothing, that a defect in what an outcome states is
// that outcome's fail, and a break of the rule its line of the request
// states its contrary: only what neither covers is an anomaly.
func TestEvaluatePromptAnomaliesAreNotes(t *testing.T) {
	for name, p := range map[string]EvaluatePrompt{
		"slot": testEvaluatePrompt(), "cli": testWorkspacePrompt(testCLIWorkspace),
		"library": testWorkspacePrompt(testLibraryWorkspace),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			flat := strings.Join(strings.Fields(got), " ")
			for _, want := range []string{
				"maul judges the change on the outcomes alone, so an anomaly never fails, holds or confirms anything",
				"So a defect in what an outcome states, on any input, is never an anomaly: report it as that " +
					"outcome's `fail`",
				"Nor is a break of the rule an outcome's line of the request states beyond its example: list it " +
					"in that outcome's `contrary`",
				"An anomaly is what neither covers",
			} {
				if !strings.Contains(flat, want) {
					t.Errorf("prompt does not say %q", want)
				}
			}
			for _, gone := range []string{"pre_existing", "What an earlier round found", "holds the change"} {
				if strings.Contains(flat, gone) {
					t.Errorf("prompt still says %q", gone)
				}
			}
		})
	}
}

// TestAnomalyNoteWords: the comment's anomaly line names the round that
// found a carried note and says that it decides nothing.
func TestAnomalyNoteWords(t *testing.T) {
	const note = " A note for the reviewers: maul judges the change on its outcomes alone."
	carried := anomalyLine(VerifyAnomaly{Summary: "the hat rotates the wrong way", Route: "synth.Shift", FoundIn: 1})
	if want := "Look-around: round 1's evaluator, at these same commits, noticed (its words, not checked by " +
		"maul): the hat rotates the wrong way, on `synth.Shift`." + note; carried != want {
		t.Errorf("carried anomaly line = %q, want %q", carried, want)
	}
	if own, want := anomalyLine(VerifyAnomaly{Summary: "odd"}),
		"Look-around: maul's evaluator noticed (its words, not checked by maul): odd."+note; own != want {
		t.Errorf("this round's anomaly line = %q, want %q", own, want)
	}
}
