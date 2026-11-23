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

// TestClaudeSubagentsPosture pins the writable posture with Subagents on
// (TestClaudePostureProbe holds the pinned CLI to it): Agent and Task leave
// the deny list and nothing else does, so workflows, wake-ups, cron,
// worktrees, monitors and the web tools stay denied at every depth; and the
// levers that keep every subagent in the foreground, at depth 1, on the run's
// own model and a few at a time are one --settings JSON on the command line,
// which outranks the settings under HOME the run can write mid-session. The
// request's Env is unchanged, the first run and a resume render the same, and
// every other flag is as with Subagents off.
func TestClaudeSubagentsPosture(t *testing.T) {
	req := PromptRequest{
		Prompt: "build it", Model: "claude-fable-5", MaxTurns: 30, Sandbox: SandboxWorkspaceWrite,
		AddDirs: []string{"/workspace"}, Env: []string{"A=b"}, Subagents: true,
	}
	settings := `{"env":{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":"1","CLAUDE_CODE_DISABLE_WORKFLOWS":"1",` +
		`"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS":"4","CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH":"1",` +
		`"CLAUDE_CODE_SUBAGENT_MODEL":"claude-fable-5","CLAUDE_CODE_SUBAGENT_MODEL_FORCE":"1"}}`
	want := []string{
		"claude", "-p",
		"--model", "claude-fable-5",
		"--output-format", "stream-json",
		"--verbose",
		"--max-turns", "30",
		"--allowedTools", "Read Glob Grep Edit Write NotebookEdit Bash",
		"--disallowedTools", "WebFetch WebSearch Workflow ScheduleWakeup CronCreate CronDelete CronList " +
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

	off := req
	off.Subagents = false
	offSpec := c.PromptSpec("/workspace/repo", off)
	if slices.Contains(offSpec.Argv, "--settings") {
		t.Errorf("Subagents off renders --settings: %q", offSpec.Argv)
	}
	onWithout := slices.DeleteFunc(slices.Clone(spec.Argv), func(a string) bool {
		return a == "--settings" || a == settings
	})
	i := slices.Index(onWithout, "--disallowedTools")
	onWithout[i+1] = argvValue(t, offSpec.Argv, "--disallowedTools")
	if !slices.Equal(onWithout, offSpec.Argv) {
		t.Errorf("Subagents on differs from off beyond the deny list and --settings:\non  %q\noff %q",
			spec.Argv, offSpec.Argv)
	}
}

// TestClaudeSubagentsLeavesOffIntact: rendering the ON posture does not
// change the shared posture table, so a later OFF request still denies the
// subagent tool.
func TestClaudeSubagentsLeavesOffIntact(t *testing.T) {
	c := NewClaude()
	off := PromptRequest{Prompt: "p", Model: "m", Sandbox: SandboxWorkspaceWrite}
	before := argvValue(t, c.PromptSpec("/ws", off).Argv, "--disallowedTools")
	on := off
	on.Subagents = true
	for range 3 {
		_ = c.PromptSpec("/ws", on)
	}
	after := argvValue(t, c.PromptSpec("/ws", off).Argv, "--disallowedTools")
	if after != before {
		t.Errorf("OFF deny list after rendering ON = %q, want %q", after, before)
	}
	for _, tool := range []string{"Agent", "Task"} {
		if !slices.Contains(strings.Fields(after), tool) {
			t.Errorf("OFF deny list %q lacks %s", after, tool)
		}
	}
}

// TestClaudeSubagentsOnlyInTheWritablePosture: every other posture ignores
// Subagents and renders byte for byte as without it, an unset posture's nil
// Env included.
func TestClaudeSubagentsOnlyInTheWritablePosture(t *testing.T) {
	c := NewClaude()
	for _, sandbox := range []Sandbox{SandboxDefault, SandboxReadOnly, SandboxEvaluate} {
		req := PromptRequest{
			Prompt: "p", Model: "m", MaxTurns: 5, Sandbox: sandbox, AddDirs: []string{"/workspace"},
			WriteDirs: []string{"/workspace/reports"}, SessionID: claudeSessionID,
		}
		on := req
		on.Subagents = true
		if got, want := c.PromptSpec("/ws", on), c.PromptSpec("/ws", req); !reflect.DeepEqual(got, want) {
			t.Errorf("sandbox %d: Subagents on renders\n%+v\nwant as off\n%+v", sandbox, got, want)
		}
		got, want := c.ResumeSpec("/ws", claudeSessionID, on), c.ResumeSpec("/ws", claudeSessionID, req)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("sandbox %d: resume with Subagents on renders\n%+v\nwant as off\n%+v", sandbox, got, want)
		}
	}
}

// TestClaudeSubagentsWithoutAModel: a request with no model sets no child
// model either, rather than forcing children onto an empty one.
func TestClaudeSubagentsWithoutAModel(t *testing.T) {
	spec := NewClaude().PromptSpec("/ws", PromptRequest{Prompt: "p", Sandbox: SandboxWorkspaceWrite, Subagents: true})
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(argvValue(t, spec.Argv, "--settings")), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE"} {
		if _, ok := got.Env[key]; ok {
			t.Errorf("settings env %v sets %s with no model", got.Env, key)
		}
	}
	if got.Env["CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH"] != "1" {
		t.Errorf("settings env %v lacks the depth bound", got.Env)
	}
}
