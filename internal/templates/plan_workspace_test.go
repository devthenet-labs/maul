// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"reflect"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

var (
	testPlanCLI = &PlanWorkspace{Surface: SurfaceCLI, Repository: "https://github.com/devthenet-labs/bbcount",
		Binary: "/work/bin/bbrender"}
	testPlanLibrary = &PlanWorkspace{Surface: SurfaceLibrary, Language: "go"}
)

func testWorkspacePlanPrompt(w *PlanWorkspace) PlanPrompt {
	return PlanPrompt{IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md",
		Intent: testPlanRequest, BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: testPlanLimits,
		Verification: &PlanVerification{Workspace: w}}
}

// TestPlanPromptWorkspaceGoldens: a Project with no preview asks for
// outcomes on its cli or library surface (SPEC-S2 S2-15), never a preview's.
func TestPlanPromptWorkspaceGoldens(t *testing.T) {
	for _, tt := range []struct {
		name string
		w    *PlanWorkspace
	}{
		{"prompt_plan_outcomes_cli.md", testPlanCLI},
		{"prompt_plan_outcomes_library.md", testPlanLibrary},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderPlanPrompt(testWorkspacePlanPrompt(tt.w))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestPlanPromptWorkspaceNamesItsSurface: the workspace section names the
// surface and the base rule, and none of a preview's.
func TestPlanPromptWorkspaceNamesItsSurface(t *testing.T) {
	for _, tt := range []struct {
		name      string
		w         *PlanWorkspace
		want, not []string
	}{
		{"cli", testPlanCLI, []string{"runs the command-line program the build writes, `/work/bin/bbrender`,",
			"The evaluator builds `https://github.com/devthenet-labs/bbcount`", ExamplePlanOutcomesCLI},
			[]string{ExamplePlanOutcomesLibrary, "exported function"}},
		{"library", testPlanLibrary, []string{"calls the Go library from programs of its own",
			"The evaluator calls only the library's exported API", ExamplePlanOutcomesLibrary},
			[]string{ExamplePlanOutcomesCLI, "The evaluator builds"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderPlanPrompt(testWorkspacePlanPrompt(tt.w))
			if err != nil {
				t.Fatal(err)
			}
			flat := strings.Join(strings.Fields(got), " ")
			for _, want := range append(tt.want, "counts a `change` outcome only if it does not hold there") {
				if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
					t.Errorf("the prompt does not say %q", want)
				}
			}
			for _, gone := range append(tt.not, "live preview", "browser", "previewed components",
				ExamplePlanOutcomes, "### Acceptance criteria") {
				if strings.Contains(got, gone) {
					t.Errorf("the prompt says %q", gone)
				}
			}
		})
	}
}

// TestPlanOutcomesWorkspaceExamplesHold: each surface's example is a plan
// outcome list report.ParsePlan reads, kept to its own rules.
func TestPlanOutcomesWorkspaceExamplesHold(t *testing.T) {
	for name, ex := range map[string]string{"cli": ExamplePlanOutcomesCLI, "library": ExamplePlanOutcomesLibrary} {
		plan := "---\nsummary: \"Render at any tempo\"\nrepositories:\n  - \"https://github.com/o/r\"\n" +
			"new_dependencies: []\nquestions: []\nconfidence: 0.8\nestimated_max_turns: 50\n" +
			"estimated_token_budget: 100000\n" + ex + "\n---\n\n## Approach\n\nRender.\n"
		p, err := report.ParsePlan([]byte(plan))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(p.Outcomes) < 2 || p.CheckEvaluator() != nil {
			t.Errorf("%s: %d outcomes, CheckEvaluator %v", name, len(p.Outcomes), p.CheckEvaluator())
		}
		for _, f := range acceptance.LintOutcomes(p.Outcomes) {
			t.Errorf("%s: %s flagged %s on %q", name, f.Outcome, f.Rule, f.Word)
		}
		for i, line := range strings.Split(ex, "\n") {
			if len(line) > 118 {
				t.Errorf("%s example line %d is %d columns", name, i+1, len(line))
			}
		}
	}
}

// TestPlanVerificationWorkspaceWire: a workspace travels to the plan Job
// alone and back unchanged; anything EncodePlanVerification would not have
// written is refused, and an invalid workspace is never stated.
func TestPlanVerificationWorkspaceWire(t *testing.T) {
	for _, w := range []*PlanWorkspace{testPlanCLI, testPlanLibrary} {
		in := &PlanVerification{Workspace: w,
			Components: []PlanVerifyComponent{{Key: "api", Path: "/api"}}}
		enc := EncodePlanVerification(in)
		got, err := DecodePlanVerification(enc)
		if err != nil {
			t.Fatalf("decode %s: %v", enc, err)
		}
		want := &PlanVerification{Workspace: w}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip of %+v = %+v, want %+v (components dropped)", in, got, want)
		}
	}
	for name, s := range map[string]string{
		"with components": `{"components":[{"key":"api","path":"/api"}],` +
			`"workspace":{"surface":"library","language":"go"}}`,
		"node library":    `{"components":null,"workspace":{"surface":"library","language":"node"}}`,
		"binary outside":  `{"components":null,"workspace":{"surface":"cli","binary":"/bin/sh"}}`,
		"unknown surface": `{"components":null,"workspace":{"surface":"worker"}}`,
		"unknown field":   `{"components":null,"workspace":{"surface":"library","language":"go","x":1}}`,
		"repository breaks": `{"components":null,` +
			`"workspace":{"surface":"library","language":"go","repository":"a\nb"}}`,
	} {
		if _, err := DecodePlanVerification(s); err == nil {
			t.Errorf("%s: decoded %s", name, s)
		}
	}
	// An invalid workspace is never stated: the
	// preview's components stand, as before the field existed.
	for _, in := range []*PlanVerification{
		{Workspace: &PlanWorkspace{Surface: SurfaceCLI, Binary: "/bin/sh"},
			Components: []PlanVerifyComponent{{Key: "api", Path: "/api"}}},
	} {
		if enc := EncodePlanVerification(in); strings.Contains(enc, "workspace") || enc == "" {
			t.Errorf("EncodePlanVerification(%+v) = %s", in, enc)
		}
	}
	if enc := EncodePlanVerification(&PlanVerification{
		Workspace: &PlanWorkspace{Surface: "worker"}}); enc != "" {
		t.Errorf("an invalid workspace with no component encoded: %s", enc)
	}
}
