// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
	"time"
)

// planWrapUp and buildWrapUp are the wrap-up prompts of a plan and a build
// stopped at their turn limits.
var (
	planWrapUp  = WrapUpPrompt{ReportPath: "/workspace/reports/plan.md", FirstTurns: 34, Turns: 6}
	buildWrapUp = WrapUpPrompt{
		ReportPath: "/workspace/reports/build.md", CommitScriptPath: "/workspace/commit.sh", FirstTurns: 144, Turns: 6,
	}
)

// wrapUpLimits are a run's limits with turns held back for a wrap-up.
var (
	wrapUpPlanLimits  = StageLimits{MaxTurns: 34, WrapUpTurns: 6, TokenBudget: 250000, Timeout: 20 * time.Minute}
	wrapUpBuildLimits = StageLimits{MaxTurns: 114, WrapUpTurns: 6, TokenBudget: 600000, Timeout: 45 * time.Minute}
)

func TestWrapUpPromptGoldens(t *testing.T) {
	written := buildWrapUp
	written.ReportWritten = true
	tests := []struct {
		name string
		p    WrapUpPrompt
	}{
		{"prompt_wrapup_plan.md", planWrapUp},
		{"prompt_wrapup_build.md", buildWrapUp},
		{"prompt_wrapup_build_written.md", written},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderWrapUpPrompt(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestWrapUpPromptStatesTheRules: a wrap-up states the limit the run
// reached and the turns it has left. A plan's is told to stop exploring and
// write its report alone; a build's that the tree is kept as it is, that it
// may change nothing in it, and that unfinished work is success false —
// never a claim the tree does not back.
func TestWrapUpPromptStatesTheRules(t *testing.T) {
	plan, err := RenderWrapUpPrompt(planWrapUp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"limit of 34 agent turns", "at most 6 more turns", "Stop exploring", "`/workspace/reports/plan.md`",
		"Write no file but the report.",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan wrap-up lacks %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "commit") || strings.Contains(plan, "success") {
		t.Errorf("plan wrap-up speaks of a build's outputs:\n%s", plan)
	}

	for _, p := range []WrapUpPrompt{buildWrapUp, {ReportPath: buildWrapUp.ReportPath,
		CommitScriptPath: buildWrapUp.CommitScriptPath, FirstTurns: 144, Turns: 6, ReportWritten: true}} {
		build, err := RenderWrapUpPrompt(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"limit of 144 agent turns",
			"patchy refuses a wrap-up that changes any file in it, and commits only\nwhat it already holds",
			"Do not edit code or tests, do not run tests, builds or other commands, and do not commit.",
			"set `success: false`",
			"Never claim more than the tree holds.",
			"`/workspace/commit.sh`",
		} {
			if !strings.Contains(build, want) {
				t.Errorf("build wrap-up (written %v) lacks %q:\n%s", p.ReportWritten, want, build)
			}
		}
		if checks := strings.Contains(build, "Check the report you wrote"); checks != p.ReportWritten {
			t.Errorf("build wrap-up asks to check a written report = %v, want %v", checks, p.ReportWritten)
		}
	}
}

// TestPromptsStateTheWrapUp: a run with turns held back for a wrap-up is
// told so, beside the limit it runs on, and that the token and time limits
// still end it with nothing; a run with none reads exactly as before.
func TestPromptsStateTheWrapUp(t *testing.T) {
	plan, err := RenderPlanPrompt(PlanPrompt{
		IssuePath: "/workspace/input/issue.md", ReportPath: "/workspace/reports/plan.md", Intent: testPlanRequest,
		BuildMaxTurns: 150, BuildTokenBudget: 800000, Limits: wrapUpPlanLimits,
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "prompt_plan_wrapup.md", plan)
	build, err := RenderBuildPrompt(BuildPrompt{
		PlanPath: "/workspace/input/investigation.md", ReportPath: "/workspace/reports/build.md",
		CommitScriptPath: "/workspace/commit.sh", Limits: wrapUpBuildLimits,
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "prompt_build_wrapup.md", build)

	for name, tt := range map[string]struct {
		got, own, wrap string
	}{
		"plan": {plan, "This stage may take at most 34 agent turns, 250000 output tokens and 20 minutes.",
			"If you reach the turn limit, patchy gives you at most 6 more turns to write the report"},
		"build": {build, "This run may take at most 114 agent turns, 600000 output tokens and 45 minutes.",
			"If you reach the turn limit, patchy gives you at most 6 more turns to make your report"},
	} {
		if !strings.Contains(tt.got, tt.own) || !strings.Contains(tt.got, tt.wrap) {
			t.Errorf("%s prompt lacks %q or %q", name, tt.own, tt.wrap)
		}
		if !strings.Contains(tt.got, "Reaching the token or time\n  limit ends the run with nothing built") &&
			!strings.Contains(tt.got, "reaching the token or time limit ends the run with no plan") {
			t.Errorf("%s prompt does not say the token and time limits still end the run", name)
		}
	}
	for _, render := range []func() (string, error){
		func() (string, error) { return renderTestPlanPrompt(testPlanRequest, nil) }, func() (string, error) {
			return renderTestBuildPrompt(nil)
		},
	} {
		got, err := render()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "more turns") {
			t.Errorf("a prompt with no wrap-up turns promises some:\n%s", got)
		}
	}
}
