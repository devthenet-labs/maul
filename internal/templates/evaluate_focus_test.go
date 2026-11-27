// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

const (
	focusHeading   = "### What this round covers"
	focusTurns     = "Then use the turns left on what the diff can affect"
	focusLookAbout = "From the diff since round 1, work out"
)

// focusedPrompt is a round after a fix, which the controller handed its
// diff and the previous round's results: a slot round or, with ws, a
// workspace round.
func focusedPrompt(ws bool, focused bool) EvaluatePrompt {
	p := testEvaluatePrompt()
	if ws {
		p = testWorkspacePrompt(testLibraryWorkspace)
	}
	p.Round, p.PriorRound = 2, 1
	p.FixDiff = "--- a/api/ratings.go\n+++ b/api/ratings.go\n@@ -1 +1 @@\n-k := 16\n+k := 32\n"
	p.PriorVerdicts = []PriorVerdict{{Outcome: p.Outcomes[0].ID, Result: "failed", Reason: "the winner did not rise"},
		{Outcome: p.Outcomes[1].ID, Result: "passed"}}
	p.Focused = focused
	return p
}

// TestFocusedRoundSection holds the change-focused wording to a focused
// round alone, in both prompts: without Focused the prompt is what a round
// after a fix always was, byte for byte (the goldens hold the rest).
func TestFocusedRoundSection(t *testing.T) {
	for _, ws := range []bool{false, true} {
		plain, err := RenderEvaluatePrompt(focusedPrompt(ws, false))
		if err != nil {
			t.Fatal(err)
		}
		focused, err := RenderEvaluatePrompt(focusedPrompt(ws, true))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{focusHeading, focusTurns} {
			if strings.Contains(plain, s) {
				t.Errorf("workspace %v: an unfocused round says %q", ws, s)
			}
			if !strings.Contains(focused, s) {
				t.Errorf("workspace %v: a focused round lacks %q", ws, s)
			}
		}
		if !strings.Contains(plain, "use the turns left") {
			t.Errorf("workspace %v: an unfocused round lost its turns sentence", ws)
		}
		if strings.Contains(focused, "to try what you have not tried yet") {
			t.Errorf("workspace %v: a focused round still tells the evaluator to try what it has not tried", ws)
		}
	}
}

// TestFocusedRoundSafetyWording holds the safety rules the section states:
// the single look is only for an outcome judged `passed` before, everything
// else (any other result, accepted among them, an outcome
// the diff can affect, a shared-code diff) is judged in full, and no
// verdict rests on the previous round.
func TestFocusedRoundSafetyWording(t *testing.T) {
	for _, ws := range []bool{false, true} {
		got, err := RenderEvaluatePrompt(focusedPrompt(ws, true))
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(got), " ")
		for _, want := range []string{
			"not judged `passed` there", "judged anything else, `accepted` included, or not listed",
			"An outcome an approver accepted was never seen to hold.",
			"as fully as in a first round", "judged `passed` there and that the diff cannot affect",
			"one direct look", "the diff is cut", "a router, a schema, a shared component, a dependency",
			"treat every outcome as affected", "is never evidence here", "an outcome you did not look at in this run is unsure",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("workspace %v: the focused section lacks %q", ws, want)
			}
		}
	}
}

// TestFocusedLookAroundIsSlotOnly: only the slot prompt has a look-around
// to narrow; the workspace prompt has none and gains none.
func TestFocusedLookAroundIsSlotOnly(t *testing.T) {
	slot, err := RenderEvaluatePrompt(focusedPrompt(false, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(slot), " "), focusLookAbout) {
		t.Errorf("a focused slot round's look-around is not narrowed to the diff:\n%s", slot)
	}
	plain, err := RenderEvaluatePrompt(focusedPrompt(false, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(strings.Fields(plain), " "), focusLookAbout) {
		t.Error("an unfocused round's look-around is narrowed")
	}
	ws, err := RenderEvaluatePrompt(focusedPrompt(true, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(ws), "look around") {
		t.Error("the workspace prompt mentions a look-around")
	}
}
