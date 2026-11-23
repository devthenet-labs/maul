// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
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
				"Choose those inputs yourself, and work out what the rule says each must give before you try it.",
				"the ends of every range or bound the outcome states, its far side only where the outcome says what " +
					"must happen there",
				"report it `fail`, citing the calls that show it",
				"**Stay within what the outcome promises.**",
				"are never a failure",
				"you do not fail the outcome on it",
				"never report the outcome `pass`: report it `unsure`, citing the calls where it broke",
				// No probe at a value the outcome's words do not reach.
				"where the outcome states no range, a value far from its example (zero, a negative, a size far past " +
					"any it names) is one its words do not decide",
				// G7: a rule about what a call returns holds on a second call.
				"the same input given a second time, where the outcome says what a call with it returns",
				"a line may state several rules, each answered by its own outcome: take only the one about this outcome's subject",
				"A fail and the fix it starts are only ever for what the outcome itself states.",
				"never to fail an outcome on more than the outcome states",
				// Where to look keeps to the rule.
				"the rule right only on the example",
			} {
				if !strings.Contains(flat(got), want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}
