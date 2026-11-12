// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The defect gate 2 finding G-B-LIB-2: planners turned an issue's rule into
// its worked example values, and the evaluator tested only that example,
// where a right and a wrong build agree (G1: a rest that wraps from step 16
// to step 1 gives the same answer for hits on steps 1 and 16; G5: a hat that
// rotates the wrong way looks right on the README's symmetric hat). An
// outcome is judged by the rule it states: the plan states the rule with the
// example as one instance, the evaluator probes the rule beyond the example
// and shows a failure on data it chose, and the reviewer refuses a green
// check that tries only the example. Against a false fail: a probe stays
// within what the outcome promises, and a rule only the request line states
// withholds a pass but never fails the outcome.

// TestPlanPromptAsksForTheRule: each surface's plan prompt asks for the
// rule the request states, the example as an instance of it, and no wider.
func TestPlanPromptAsksForTheRule(t *testing.T) {
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
				"Where the request states a rule",
				"the outcome states the rule, with the example as one instance of it",
				"its `from` is the line that states the rule",
				"Never reduce a rule to its examples",
				"State the rule as the request does and no wider: where the request gives only an example, state " +
					"the example, and add no input, bound or behaviour the request does not promise.",
				"those values are an example of it, never the whole outcome",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}

// TestPlanOutcomesExamplesStateARule: each surface's example list shows an
// outcome that states a rule with its example as an instance.
func TestPlanOutcomesExamplesStateARule(t *testing.T) {
	for name, ex := range map[string]string{"preview": ExamplePlanOutcomes, "cli": ExamplePlanOutcomesCLI,
		"library": ExamplePlanOutcomesLibrary} {
		if !strings.Contains(ex, "for example") {
			t.Errorf("%s: no outcome states a rule with its example", name)
		}
	}
}

// TestEvaluatePromptProbesTheRule: every evaluate prompt asks the evaluator
// to try a rule beyond its example, on inputs it chooses, to fail it only
// within what the outcome promises, and to withhold a pass on a rule only
// the request line states without failing the outcome on it.
func TestEvaluatePromptProbesTheRule(t *testing.T) {
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
				"**Judge the rule, not only its example.**",
				"the outcome holds only if the rule holds, and a build can be right on the example and wrong elsewhere",
				"try the rule on inputs it covers beyond the example",
				"Choose those inputs yourself, and work out what the rule says each must give before you run it.",
				"the ends of every range or bound the outcome states, its far side only where the outcome says what " +
					"must happen there",
				"assert both, the far side only where the outcome says what must happen there",
				"report it `fail` with a check of your own data that shows it",
				"**Stay within what the outcome promises.**",
				"are never a failure",
				"you do not fail the outcome on it",
				"never report the outcome `pass`: list the seqs where it broke in its `contrary`",
				"A fail and the fix it starts are only ever for what the outcome itself states.",
				"never to fail an outcome on more than the outcome states",
				// The cover list and the disputes keep to the rule.
				"Where the outcome states a rule, assert it on an input beyond its example as well as on the example",
				"right only on the example",
				"never dispute it for trying inputs the outcome does not name",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}

// TestGreenRubricRefusesAnExampleOnlyCheck: the reviewer's green rule, and
// so the evaluator's, refuses a check that tries a rule only on its example.
func TestGreenRubricRefusesAnExampleOnlyCheck(t *testing.T) {
	want := "one that tries a rule the outcome states only on the example the outcome or the request names"
	if !strings.Contains(GreenRubric, want) {
		t.Errorf("the green rule lacks %q", want)
	}
}

// TestCheckReviewPromptJudgesTheRule: the green question refuses a check of
// an example-only outcome whose line states the rule and that tries nothing
// beyond it; the red question admits an input beyond the example only where
// the rule, as the outcome words it, covers it and decides its value, and
// still never shows the request.
func TestCheckReviewPromptJudgesTheRule(t *testing.T) {
	green := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
	green.From, green.Request = "ratings should update after every battle", "# Elo\n"
	wsGreen := testWorkspaceReview(t, ExampleCLIRunCheck, testReviewCLI)
	red := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
	red.From, red.Request = sentinel+" every battle", "# "+sentinel+"\n"
	red.ExpectFail, red.FailedStep, red.FailedAssertion = true, 2, "json /rating"
	wsRed := testWorkspaceReview(t, ExampleLibraryRunCheck, testReviewLibrary)
	wsRed.Kind = acceptance.OutcomeKindChange
	wsRed.ExpectFail, wsRed.FailedStep, wsRed.FailedAssertion = true, 1, "marker too-long"
	for _, tt := range []struct {
		name string
		p    CheckReviewPrompt
		want []string
	}{
		{"green", green, []string{"names only an example of a rule its line states",
			"one that tries a rule the outcome states only on the example the outcome or the request names"}},
		{"green workspace", wsGreen, []string{"names only an example of a rule its line states"}},
		{"red", red, []string{"an input beyond that example is still within the outcome",
			"Answer `does_not_prove` if the rule as the outcome words it does not cover that input"}},
		{"red workspace", wsRed, []string{"an input beyond that example is still within the outcome"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderCheckReviewPrompt(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			if tt.p.ExpectFail && strings.Contains(got, sentinel) {
				t.Error("the red question shows the request or its line")
			}
		})
	}
}
