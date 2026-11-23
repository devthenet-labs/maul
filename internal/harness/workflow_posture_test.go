// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestClaudeWorkflowsPosture pins the writable posture with Subagents and
// Workflows on (TestClaudePostureProbe holds the pinned CLI to it): Workflow
// leaves the deny list and joins the allow list, which dontAsk needs before
// it runs one; the subagents levers stay, but for the one that removes
// workflows, the child model's holding the workflow agents too; and the
// workflow agents' bound joins them in the same --settings JSON. Every other
// flag is as with Subagents alone.
func TestClaudeWorkflowsPosture(t *testing.T) {
	req := PromptRequest{
		Prompt: "build it", Model: "claude-fable-5", MaxTurns: 30, Sandbox: SandboxWorkspaceWrite,
		AddDirs: []string{"/workspace"}, Env: []string{"A=b"}, Subagents: true, Workflows: true,
	}
	settings := `{"env":{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":"1",` +
		`"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS":"4","CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH":"1",` +
		`"CLAUDE_CODE_SUBAGENT_MODEL":"claude-fable-5","CLAUDE_CODE_SUBAGENT_MODEL_FORCE":"1",` +
		`"CLAUDE_CODE_WORKFLOW_MAX_CONCURRENT_AGENTS":"4"}}`
	want := []string{
		"claude", "-p",
		"--model", "claude-fable-5",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "30",
		"--allowedTools", "Read Glob Grep Edit Write NotebookEdit Bash Workflow",
		"--disallowedTools", "WebFetch WebSearch ScheduleWakeup CronCreate CronDelete CronList " +
			"EnterWorktree ExitWorktree Monitor",
		"--setting-sources", "user", "--permission-mode", "dontAsk",
		"--strict-mcp-config",
		"--settings", settings,
		"--add-dir", "/workspace/repo",
		"--add-dir", "/workspace",
	}
	c := NewClaude()
	spec := c.PromptSpec("/workspace/repo", req)
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("Argv =\n%q\nwant\n%q", spec.Argv, want)
	}
	if want := []string{"A=b", claudeMDFromAddDirs}; !slices.Equal(spec.Env, want) {
		t.Errorf("Env = %q, want the posture's own env unchanged: %q", spec.Env, want)
	}
	resume := c.ResumeSpec("/workspace/repo", claudeSessionID, req)
	if got := resume.Argv[:len(resume.Argv)-2]; !slices.Equal(got, want) {
		t.Errorf("resume Argv =\n%q\nwant the first run's\n%q", got, want)
	}
	if got := ClaudeWorkflowConcurrency; got != 4 {
		t.Errorf("ClaudeWorkflowConcurrency = %d, want the 4 the settings carry", got)
	}
}

// TestClaudeWorkflowsNeedSubagents: Workflows alone grants nothing. Without
// Subagents, and in every posture but the writable one, a request renders
// byte for byte as without it.
func TestClaudeWorkflowsNeedSubagents(t *testing.T) {
	c := NewClaude()
	for _, sandbox := range []Sandbox{SandboxDefault, SandboxReadOnly, SandboxWorkspaceWrite, SandboxEvaluate} {
		for _, subagents := range []bool{false, true} {
			if sandbox == SandboxWorkspaceWrite && subagents {
				continue
			}
			req := PromptRequest{
				Prompt: "p", Model: "m", MaxTurns: 5, Sandbox: sandbox, AddDirs: []string{"/workspace"},
				SessionID: claudeSessionID, Subagents: subagents,
			}
			on := req
			on.Workflows = true
			if got, want := c.PromptSpec("/ws", on), c.PromptSpec("/ws", req); !reflect.DeepEqual(got, want) {
				t.Errorf("sandbox %d, subagents %v: Workflows on renders\n%+v\nwant as off\n%+v",
					sandbox, subagents, got, want)
			}
		}
	}
}

// TestClaudeWorkflowsLeaveTablesIntact: rendering the workflows posture
// changes neither shared posture table, so a later subagents-only request
// still denies Workflow and does not allow it.
func TestClaudeWorkflowsLeaveTablesIntact(t *testing.T) {
	c := NewClaude()
	sub := PromptRequest{Prompt: "p", Model: "m", Sandbox: SandboxWorkspaceWrite, Subagents: true}
	before := c.PromptSpec("/ws", sub).Argv
	on := sub
	on.Workflows = true
	for range 3 {
		_ = c.PromptSpec("/ws", on)
	}
	after := c.PromptSpec("/ws", sub).Argv
	if !slices.Equal(after, before) {
		t.Errorf("subagents-only argv after rendering workflows =\n%q\nwant\n%q", after, before)
	}
	if !slices.Contains(strings.Fields(argvValue(t, after, "--disallowedTools")), "Workflow") {
		t.Errorf("subagents-only deny list %q lacks Workflow", argvValue(t, after, "--disallowedTools"))
	}
	if slices.Contains(strings.Fields(argvValue(t, after, "--allowedTools")), "Workflow") {
		t.Errorf("subagents-only allow list %q holds Workflow", argvValue(t, after, "--allowedTools"))
	}
}

// TestClaudeWorkflowsWithoutAModel: a request with no model sets no agent
// model at all, and the workflow agents' bound regardless.
func TestClaudeWorkflowsWithoutAModel(t *testing.T) {
	spec := NewClaude().PromptSpec("/ws", PromptRequest{
		Prompt: "p", Sandbox: SandboxWorkspaceWrite, Subagents: true, Workflows: true,
	})
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(argvValue(t, spec.Argv, "--settings")), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL"} {
		if _, ok := got.Env[key]; ok {
			t.Errorf("settings env %v sets %s with no model", got.Env, key)
		}
	}
	if got.Env["CLAUDE_CODE_WORKFLOW_MAX_CONCURRENT_AGENTS"] != "4" {
		t.Errorf("settings env %v lacks the workflow agents' bound", got.Env)
	}
}

// TestClaudeParseResultCountsWorkflows: a run whose workflow ran records
// what the workflow's agents spent, though the CLI counts no subagent for it
// (subagent_stats.spawned 0) and the agents print no turns: the per-model
// figures (9 in, 9 out) against the main thread's 3 and 3. Its turns are the
// main thread's over both result events, and its subagents' none.
func TestClaudeParseResultCountsWorkflows(t *testing.T) {
	res, ok := NewClaude().ParseResult(readFixture(t, "workflow"))
	checkResult(t, res, ok, wantResult{
		in: 9, out: 9, cost: 0.00021599999999999994, turns: 3, final: "done",
	})
}

// TestScanWorkflowProgress: a workflow's progress events are what show it
// working, since its agents print no turns; nothing else is.
func TestScanWorkflowProgress(t *testing.T) {
	c := NewClaude()
	var progress int
	for line := range strings.Lines(string(readFixture(t, "workflow"))) {
		if c.ScanProgress([]byte(line)) {
			progress++
			if !strings.Contains(line, `"task_progress"`) {
				t.Errorf("progress on a line that is no task's progress: %.120s", line)
			}
		}
	}
	if progress != 5 {
		t.Errorf("%d progress lines, want the fixture's 5 task_progress events", progress)
	}
	for _, line := range []string{
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"\"subtype\":\"task_progress\""}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"task_progress"}]}}`,
		`"task_progress"`,
	} {
		if c.ScanProgress([]byte(line)) {
			t.Errorf("progress on %s", line)
		}
	}
}

// TestClaudeTaskTurnsNoticeAWorkflow: a workflow's start is a notice under
// the call that started it, in maul's words alone: neither the workflow's
// name nor its script, which are the model's or the repository's.
func TestClaudeTaskTurnsNoticeAWorkflow(t *testing.T) {
	turns := scanStreamTurns([]byte(lineWorkflow))
	if len(turns) != 1 {
		t.Fatalf("turns = %+v, want one notice", turns)
	}
	got := turns[0]
	if got.Text != "workflow started" || got.Parent != "p_wf" {
		t.Errorf("notice = %+v, want \"workflow started\" under p_wf", got)
	}
}
