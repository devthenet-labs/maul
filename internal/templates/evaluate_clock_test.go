// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
	"time"
)

// TestEvaluatePromptStatesTheClock (gate D-B8): both evaluate prompts tell
// the evaluator that maul-tool shows the time left, when to write its
// report by it, and to report an outcome from what it saw, unsure naming
// what it could not exercise, rather than spend the run past its budget on
// it.
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
				"Past its budget, report an outcome from what you saw: unsure if you could not tell, saying in its " +
					"note what you could not exercise and why.",
				"one stopped by its wall clock only the few minutes maul kept for it, if any.",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("%s, %s: prompt lacks %q", name, tt.clock, want)
				}
			}
		}
	}
}
