// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// Defect gate 3 found two outcome coverage gaps, both outside what a check
// could show:
//
//   - G6 (not Found): the issue said Fill "returns a copy of p"; the planted
//     copy turned an absent ID, Votes and CreatedAt into empty values, so the
//     copy could not be encoded. The plan's outcomes named only the four
//     fields the issue listed, and the evaluator never tried a field left
//     absent. A promise that something is carried over is a rule, and the
//     zero, empty and absent values of what it carries are its main inputs.
//   - G1 (owner's rule reading): `offbeat-plain-example` quoted the issue's
//     general counting rule in its from but stated only the README's values,
//     and was passed while the evaluator's own note said the rule broke (it
//     listed the contrary seq only under the separate rule outcome). The
//     keep outcomes likewise stated one example of an "unchanged for any
//     input" line.
//
// Against a false fail: what is carried over is only what the request
// promises (the whole where it says "a copy", the named fields where it
// names them), a difference nothing a caller does can see is no failure, and
// a rule only the request line states withholds a pass (contrary) but never
// fails the outcome.

// TestPlanPromptAsksForCarriedOverOutcomes: each surface's plan prompt turns
// a promise that something is carried over into an outcome over all of what
// is promised, set, zero or absent, and no wider.
func TestPlanPromptAsksForCarriedOverOutcomes(t *testing.T) {
	for name, p := range map[string]PlanPrompt{
		"preview": {IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
			Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
			Verification: &PlanVerification{Components: []PlanVerifyComponent{{Key: "api", Path: "/api"}}}},
		"cli":     testWorkspacePlanPrompt(testPlanCLI),
		"library": testWorkspacePlanPrompt(testPlanLibrary),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderPlanPrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				// G6: a preservation promise is a rule and gets an outcome.
				"Where the request promises that something is carried over (a copy, fields kept, the rest " +
					"unchanged, the same as before, a value that round-trips through save and load), that promise " +
					"is a rule too: state it as an outcome of its own",
				"over all of what the request promises: for \"a copy of p\", every field of p other than those " +
					"the request says change, the ones it does not name included",
				"whether each is set, zero, empty or left absent",
				// Against a false fail: no wider than the promise.
				"where the request names the fields kept, only those",
				// G1: a line that states a rule gets no example-only outcome.
				"An outcome whose `from` line states a rule states that rule, whatever else it names",
				"never write an outcome of only the example with that line as its `from`",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			// G1: a keep outcome may state a rule; it is no longer pushed to
			// a single example.
			if !strings.Contains(flat(got), "where its line states a rule (unchanged for any input), it states "+
				"that rule with the") {
				t.Error("the keep bullet does not let a keep outcome state its line's rule")
			}
		})
	}
}

// TestPlanPromptCarriedOverOnAPreviewIsAboutItsOwnItem: on a preview the
// carried-over outcome is worded over an item the check creates, as every
// other preview outcome is.
func TestPlanPromptCarriedOverOnAPreviewIsAboutItsOwnItem(t *testing.T) {
	got, err := RenderPlanPrompt(PlanPrompt{IssuePath: "/workspace/input/issue.md",
		ReportPath: "/workspace/reports/plan.md", Intent: testPlanRequest, BuildMaxTurns: 150,
		BuildTokenBudget: 800000, Limits: testPlanLimits,
		Verification: &PlanVerification{Components: []PlanVerifyComponent{{Key: "api", Path: "/api"}}}})
	if err != nil {
		t.Fatal(err)
	}
	want := "State such a promise over an item the person checking creates (\"a beat you copy keeps its name, " +
		"tempo and steps, and its notes even when you left them empty\")."
	if !strings.Contains(flat(got), want) {
		t.Errorf("prompt lacks %q", want)
	}
}

// TestEvaluatePromptProbesCarriedOverValues: every evaluate prompt asks the
// evaluator to try a carried-over promise on the values a careless copy
// loses, to compare as a caller observes, and to stay within the promise.
func TestEvaluatePromptProbesCarriedOverValues(t *testing.T) {
	for name, p := range map[string]EvaluatePrompt{
		"preview": testEvaluatePrompt(),
		"cli":     testWorkspacePrompt(testCLIWorkspace),
		"library": testWorkspacePrompt(testLibraryWorkspace),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"**Judge what is carried over.**",
				"try it on the values a careless copy loses: a field left unset, zero, empty or absent (a nil " +
					"value, a missing optional key), the ends of any range the rule states, and every field the " +
					"promise covers, not only those your input sets",
				"Compare the result as a caller observes it: the whole value, and where the type is written out or " +
					"read back (encoded, saved, printed), that it is written as the original is",
				// Against a false fail.
				"A field the outcome says changes, and one the promise does not cover, are not part of it",
				"a difference nothing a caller does can observe is no failure",
				// The stay-within rule does not exclude a kept zero.
				"A zero, empty or absent value of what an outcome promises to carry over is one its words decide: " +
					"it must come through as it was.",
				// G1: the rule the from states is this outcome's, whichever
				// other outcome also states it.
				"A break you saw while judging another outcome counts here too: another outcome stating the same " +
					"rule is no reason to pass this one.",
				"Report such an outcome `pass` only once you have tried that rule beyond the example and it held, " +
					"its ledger citing those calls",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}

// TestWorkspaceEvaluateComparesUnchangedAtTheBase: on a workspace, an
// "unchanged" rule's expected value is the same call at the base, so the
// evaluator needs no value the outcome does not state; a slot has no base
// and is not told so.
func TestWorkspaceEvaluateComparesUnchangedAtTheBase(t *testing.T) {
	want := "Where an outcome's rule is that something is unchanged by the change (the same output, the same " +
		"value), the base gives what it must be for any input the rule covers: make the same call with " +
		"`--base` and compare."
	for name, p := range map[string]EvaluatePrompt{
		"cli":     testWorkspacePrompt(testCLIWorkspace),
		"library": testWorkspacePrompt(testLibraryWorkspace),
	} {
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(flat(got), want) {
			t.Errorf("%s: prompt lacks %q", name, want)
		}
	}
	got, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(flat(got), "`--base`") {
		t.Error("the slot prompt speaks of a base it does not have")
	}
}

// TestCheckReviewRedQuestionDecidesAKeptZero: the red question's "a value
// far from its example is not one the rule decides" does not refuse a red
// check of a zero or absent value an outcome promises to carry over.
func TestCheckReviewRedQuestionDecidesAKeptZero(t *testing.T) {
	want := "A zero, empty or absent value of what the outcome promises to carry over (a copy, fields kept, " +
		"unchanged) is one it decides: it must come through as it was."
	red := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
	red.ExpectFail, red.FailedStep, red.FailedAssertion = true, 2, "json /rating"
	wsRed := testWorkspaceReview(t, ExampleLibraryRunCheck, testReviewLibrary)
	wsRed.Kind = acceptance.OutcomeKindChange
	wsRed.ExpectFail, wsRed.FailedStep, wsRed.FailedAssertion = true, 1, "marker too-long"
	green := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
	for name, tt := range map[string]struct {
		p    CheckReviewPrompt
		want bool
	}{"red": {red, true}, "red workspace": {wsRed, true}, "green": {green, false}} {
		got, err := RenderCheckReviewPrompt(tt.p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(flat(got), want) != tt.want {
			t.Errorf("%s: has %q = %v, want %v", name, want, !tt.want, tt.want)
		}
	}
}
