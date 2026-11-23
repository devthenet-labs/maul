// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestBuildPromptSubagents: a build granted subagents is told what the
// posture holds it to, each a measured fact of the pinned CLI: subagents run
// in the foreground only (and so does every command), at most four at once
// with a refused fifth not to be retried, one level deep, on this run's
// model, in this same tree, never in a worktree of their own. It is told
// that their model calls count against this run's turn grant beside its
// own, that the cost the broker charges for every call is what really
// bounds the run, and that the report and commit.sh are its own to write.
// Without the grant the prompt says none of it.
func TestBuildPromptSubagents(t *testing.T) {
	render := func(atOnce int) string {
		t.Helper()
		got, err := RenderBuildPrompt(BuildPrompt{
			PlanPath: "/workspace/input/investigation.md", ReportPath: "/workspace/reports/build.md",
			CommitScriptPath: "/workspace/commit.sh", Limits: testBuildLimits, SubagentsAtOnce: atOnce,
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	on := foldSpace(render(4))
	for _, want := range []string{
		"## Subagents",
		"Agent tool",
		"in the foreground",
		"run_in_background",
		"At most 4 subagents run at once",
		"do not retry",
		"cannot start subagents of its own",
		"this run's model",
		"same working tree",
		"`isolation`",
		"counts against this run's 120 turns beside your own",
		"cost",
		"broker",
		"the report and `/workspace/commit.sh` yourself",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("granted build prompt lacks %q:\n%s", want, on)
		}
	}
	off := render(0)
	for _, unwanted := range []string{"Subagent", "subagent", "Agent tool", "isolation"} {
		if strings.Contains(off, unwanted) {
			t.Errorf("ungranted build prompt mentions %q", unwanted)
		}
	}
	if base, err := renderTestBuildPrompt(nil); err != nil || base != off {
		t.Errorf("the ungranted prompt is not the plain build prompt (err %v)", err)
	}
}

// foldSpace folds a rendered prompt's line breaks to spaces, so a phrase
// wrapped at the template's column limit still matches.
func foldSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestBuildPromptWorkflows: a build granted workflows is given the opt-in
// the Workflow tool's own description asks for, and told what the posture
// holds a workflow to, each a measured fact of the pinned CLI: a script
// inline or a built-in workflow by name, and nothing else, on pain of the
// run ending; a workflow in the background, ending in a notification to wait
// for; at most four agents at once, the rest waiting; agents with its tools,
// on its model, starting neither subagents nor workflows. It is told the
// degraded limits: a workflow's agents count against neither its turns nor
// its live token limit, only the cost budget and the time limit bind them,
// and their conversations are not in its transcript. Without the grant the
// prompt is exactly the subagents prompt.
func TestBuildPromptWorkflows(t *testing.T) {
	render := func(atOnce int) string {
		t.Helper()
		got, err := RenderBuildPrompt(BuildPrompt{
			PlanPath: "/workspace/input/investigation.md", ReportPath: "/workspace/reports/build.md",
			CommitScriptPath: "/workspace/commit.sh", Limits: testBuildLimits, SubagentsAtOnce: 4,
			WorkflowsAtOnce: atOnce, BuiltinWorkflows: []string{"deep-research"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	on := foldSpace(render(4))
	for _, want := range []string{
		"## Workflows",
		"Workflow tool",
		"`script`",
		"`deep-research`",
		"`scriptPath`",
		"`resumeFromRunId`",
		"the run ends",
		"notification",
		"At most 4 of a workflow's agents run at once",
		"wait",
		"this run's model",
		"neither subagents nor workflows",
		"neither this run's 120 turns nor its token limit",
		"cost",
		"time limit",
		"transcript",
		"the one thing that runs in the background in this run",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("granted build prompt lacks %q:\n%s", want, on)
		}
	}
	off := render(0)
	for _, unwanted := range []string{"## Workflows", "Workflow tool", "scriptPath"} {
		if strings.Contains(off, unwanted) {
			t.Errorf("a build granted no workflows is told %q", unwanted)
		}
	}
	plain, err := RenderBuildPrompt(BuildPrompt{
		PlanPath: "/workspace/input/investigation.md", ReportPath: "/workspace/reports/build.md",
		CommitScriptPath: "/workspace/commit.sh", Limits: testBuildLimits, SubagentsAtOnce: 4,
	})
	if err != nil || plain != off {
		t.Errorf("a build granted no workflows is not the subagents prompt (err %v)", err)
	}
}
