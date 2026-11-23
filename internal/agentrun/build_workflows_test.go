// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
)

// buildWorkflowsGranted is a build granted subagents and workflows, and no
// turn grant of its own: a workflow's agents are not counted against one.
func buildWorkflowsGranted(t *testing.T, out *bytes.Buffer) (Config, string, repairCase) {
	t.Helper()
	cfg, ws, c := buildGranted(t, out, 0)
	cfg.BuildWorkflows = true
	return cfg, ws, c
}

// TestFromEnvBuildWorkflows: MAUL_BUILD_WORKFLOWS is "true" or unset, any
// other value refused by name; it needs MAUL_BUILD_SUBAGENTS beside it,
// which already holds it to an intent build on claude; and it is a
// configuration key, so a repository-image Job that does not set it blanks
// it.
func TestFromEnvBuildWorkflows(t *testing.T) {
	for value, want := range map[string]bool{"": false, "true": true} {
		env := buildSubagentsEnv()
		env["MAUL_BUILD_WORKFLOWS"] = value
		cfg, err := FromEnv(func(k string) string { return env[k] })
		if err != nil || cfg.BuildWorkflows != want {
			t.Errorf("FromEnv(%q) = %v, %v; want %v", value, cfg.BuildWorkflows, err, want)
		}
	}
	env := buildSubagentsEnv()
	env["MAUL_BUILD_WORKFLOWS"] = "yes"
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), `MAUL_BUILD_WORKFLOWS="yes"`) {
		t.Errorf("FromEnv(yes) error = %v, want it refused by name", err)
	}
	env = buildSubagentsEnv()
	delete(env, "MAUL_BUILD_SUBAGENTS")
	env["MAUL_BUILD_WORKFLOWS"] = "true"
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), "MAUL_BUILD_WORKFLOWS") || !strings.Contains(err.Error(), "MAUL_BUILD_SUBAGENTS") {
		t.Errorf("FromEnv without subagents error = %v, want workflows refused without them", err)
	}
	env = buildSubagentsEnv()
	env["MAUL_BUILD_WORKFLOWS"] = "true"
	env["MAUL_PHASE"] = string(PhasePlan)
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), "MAUL_BUILD_WORKFLOWS") {
		t.Errorf("FromEnv on a plan error = %v, want workflows refused off the build", err)
	}
	if !slices.Contains(ConfigEnvKeys(), "MAUL_BUILD_WORKFLOWS") {
		t.Errorf("ConfigEnvKeys() = %v, want MAUL_BUILD_WORKFLOWS among them", ConfigEnvKeys())
	}
}

// TestBuildWorkflowsInvocation pins a build granted workflows: Workflow
// allowed and not denied, the workflow levers in --settings, the prompt's
// workflows section; and its report repair, granted neither subagents nor
// workflows, runs exactly the command an ungranted build's repair runs.
func TestBuildWorkflowsInvocation(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildWorkflowsGranted(t, &out)
	fx := &fakeExec{steps: []step{c.first(ws, c.bad), {
		ws: ws, stdout: streamResumed, writes: map[string]string{c.report: c.good},
	}}}
	a := New(cfg, fx)
	a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fx.specs) != 2 {
		t.Fatalf("commands = %d, want the first run and one repair", len(fx.specs))
	}
	first, repair := fx.specs[0], fx.specs[1]
	if deny := strings.Fields(argAfter(first.Argv, "--disallowedTools")); slices.Contains(deny, "Workflow") {
		t.Errorf("first run denies %v, want Workflow allowed", deny)
	}
	if allow := strings.Fields(argAfter(first.Argv, "--allowedTools")); !slices.Contains(allow, "Workflow") {
		t.Errorf("first run allows %v, want Workflow among them", allow)
	}
	if !strings.Contains(first.Stdin, "## Workflows") || !strings.Contains(first.Stdin, "## Subagents") {
		t.Error("the granted build's prompt lacks its subagents or workflows section")
	}
	if deny := strings.Fields(argAfter(repair.Argv, "--disallowedTools")); !slices.Contains(deny, "Workflow") ||
		!slices.Contains(deny, "Agent") || slices.Contains(repair.Argv, "--settings") {
		t.Errorf("repair argv %v, want Workflow and Agent denied and no --settings", repair.Argv)
	}
	goldenFile(t, "invocation_build_workflows.txt", renderInvocation(first, ws))
	// The repair is the ungranted build's repair, byte for byte.
	goldenFile(t, "invocation_build_repair.txt", renderInvocation(repair, ws))
}

// TestWorkflowGrantIsTheDelegationSwitchs: the grant is the one the
// delegation switch reads, so a granted build's workflow start passes it,
// and a call for a workflow by its path ends the run, as a build granted
// subagents alone ends on a workflow's start.
func TestWorkflowGrantIsTheDelegationSwitchs(t *testing.T) {
	const byPath = `{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"p_wf",` +
		`"name":"Workflow","input":{"scriptPath":"/workspace/repo/.claude/workflows/x.js"}}]},` +
		`"parent_tool_use_id":null}`
	tests := []struct {
		name      string
		workflows bool
		lines     []string
		reason    string // "" when the run must go on
	}{
		{"a workflow's start, granted", true, []string{lineWorkflowStarted}, ""},
		{"a workflow's start, subagents alone", false, []string{lineWorkflowStarted}, "not granted"},
		{"a workflow by its path, granted", true, []string{byPath}, "inline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws, c := buildWorkflowsGranted(t, &out)
			cfg.BuildWorkflows = tt.workflows
			first := c.first(ws, c.good)
			first.budgetLines = tt.lines
			stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
			if tt.reason == "" {
				if stage.Outcome == envelope.OutcomeBudgetExceeded {
					t.Fatalf("stage = %q (detail %q), want the run to go on", stage.Outcome, stage.Detail)
				}
				return
			}
			if stage.Outcome != envelope.OutcomeBudgetExceeded || !strings.Contains(stage.Detail, tt.reason) {
				t.Fatalf("stage = %q (detail %q), want budget_exceeded mentioning %q", stage.Outcome,
					stage.Detail, tt.reason)
			}
		})
	}
}

// TestRepairIsNotGrantedWorkflows: a granted build's report repair runs
// without workflows, so a repair that starts one is refused as on any
// other stage.
func TestRepairIsNotGrantedWorkflows(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildWorkflowsGranted(t, &out)
	fx := &fakeExec{steps: []step{
		c.first(ws, c.bad),
		{ws: ws, stdout: streamResumed, budgetLines: []string{lineWorkflowStarted},
			writes: map[string]string{c.report: c.good}},
	}}
	stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
	if stage.Outcome != envelope.OutcomeReportInvalid || !strings.Contains(stage.Detail, "workflow") {
		t.Fatalf("stage = %q (detail %q), want report_invalid on the repair's workflow", stage.Outcome,
			stage.Detail)
	}
}

// TestIdleWatchdogCountsWorkflowProgress: a workflow's agents print no
// turns, only the workflow's progress events, so a build granted workflows
// that waits on one long past the idle limit is not ended while the
// workflow progresses; a build granted subagents alone, whose stream holds
// no workflow, counts those lines as nothing, as before.
func TestIdleWatchdogCountsWorkflowProgress(t *testing.T) {
	for _, workflows := range []bool{true, false} {
		t.Run(map[bool]string{true: "granted", false: "subagents alone"}[workflows], func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws, _ := buildWorkflowsGranted(t, &out)
			cfg = watched(cfg, testIdle)
			cfg.BuildWorkflows = workflows
			fx := &streamExec{
				ws: ws, tick: testTick, lines: []string{lineHungCall}, every: lineWorkflowProgress,
				finishAfter: 5 * testIdle, result: strings.Split(streamSuccess, "\n")[2],
			}
			ev := runOne(t, cfg, fx, &out)
			stage := eventStage(t, ev)
			idled := stage.Outcome == envelope.OutcomeTimeout && strings.HasPrefix(stage.Detail, "no progress")
			if idled == workflows {
				t.Errorf("outcome = %q (detail %q): the watchdog ended the run %v, want %v", stage.Outcome,
					stage.Detail, idled, !workflows)
			}
		})
	}
}

// lineWorkflowProgress is a workflow's progress event, as claude 2.1.322
// prints it (shortened).
const lineWorkflowProgress = `{"type":"system","subtype":"task_progress","task_id":"wm40trwqm",` +
	`"tool_use_id":"p_wf","description":"One: a0","usage":{"total_tokens":21,"tool_uses":1,"duration_ms":70},` +
	`"workflow_progress":[{"type":"workflow_agent","index":1,"label":"a0","state":"start"}]}`
