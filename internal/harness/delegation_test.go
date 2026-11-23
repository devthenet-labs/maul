// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"strings"
	"testing"
)

// Captured claude 2.1.322 task_started events, from the subagent spike's
// stream files (the prompts and the shell commands' scratch paths
// shortened; the last line's uuid and session id are made up).
const (
	// q2-today-fg: a foreground general-purpose agent the parent started.
	lineAgentForeground = `{"type":"system","subtype":"task_started","task_id":"ae281a09c5d7499eb",` +
		`"run_id":"0mwsx8gg6-3537d417","tool_use_id":"t_agent","description":"spike child",` +
		`"subagent_type":"general-purpose","is_backgrounded":false,"spawn_depth":1,"task_type":"local_agent",` +
		`"prompt":"CHILD-A-MARK do it","uuid":"ff729f71-7637-4885-ae4f-12ea3c3bd395",` +
		`"session_id":"236dd867-53be-4a72-af78-e12c00de88a3"}`
	// q2-today-fg: the agent that child started in turn.
	lineAgentDepth2 = `{"type":"system","subtype":"task_started","task_id":"a806e83a76cf1f13c",` +
		`"run_id":"0mwsx8gi8-3f2668e2","tool_use_id":"c_nested","description":"spike child",` +
		`"subagent_type":"general-purpose","is_backgrounded":false,"spawn_depth":2,` +
		`"parent_task_id":"ae281a09c5d7499eb","task_type":"local_agent","prompt":"CHILD-C-MARK nested",` +
		`"uuid":"2edd9936-033b-4ff6-a0fa-7fcc3bd5c4af","session_id":"236dd867-53be-4a72-af78-e12c00de88a3"}`
	// q7-baseline: an agent started with the call's defaults, which put it
	// in the background.
	lineAgentBackground = `{"type":"system","subtype":"task_started","task_id":"aabef5cf43477286a",` +
		`"run_id":"0mwsxkose-55ca03a2","tool_use_id":"t_default","description":"spike child",` +
		`"subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent",` +
		`"prompt":"CHILD-A-MARK do it","uuid":"58d74274-9395-4e01-b176-e4298821e6a4",` +
		`"session_id":"7439f290-fbb1-4a5c-ad65-2b5d72304431"}`
	// q7-baseline: a foreground Explore agent.
	lineAgentExplore = `{"type":"system","subtype":"task_started","task_id":"a302a190330e10a31",` +
		`"run_id":"0mwsxkov3-d2e28aef","tool_use_id":"t_opus_explore","description":"spike child",` +
		`"subagent_type":"Explore","is_backgrounded":false,"spawn_depth":1,"task_type":"local_agent",` +
		`"prompt":"CHILD-O-MARK do it","uuid":"e0d85062-4854-4c3c-9b36-4673ad6d6755",` +
		`"session_id":"7439f290-fbb1-4a5c-ad65-2b5d72304431"}`
	// q5-allow-Workflow: an inline workflow.
	lineWorkflow = `{"type":"system","subtype":"task_started","task_id":"wudbpnxib","run_id":"0mwsxbnt7-6b8e8fa3",` +
		`"tool_use_id":"p_wf","description":"inline spike","task_type":"local_workflow",` +
		`"workflow_name":"spike-wf","prompt":"export const meta = { name: 'spike-wf' }",` +
		`"uuid":"0f94eb7b-2948-463f-9a99-739f6072b047","session_id":"6de62539-6e90-493a-a232-af9b52fe3264"}`
	// q6-bg-agent-outlives-turn: a shell command a background agent ran.
	lineChildBash = `{"type":"system","subtype":"task_started","task_id":"bf2tm6q3i",` +
		`"run_id":"0mwsxefwk-40fdbf1f","owned_by_subagent":true,"tool_use_id":"c_sleep",` +
		`"description":"sleep 6.1; touch marks/bg-child-done","is_backgrounded":false,` +
		`"parent_task_id":"a838f993c401f071c","task_type":"local_bash",` +
		`"uuid":"38d8c8bc-bfa0-45dc-b682-e9d9f3c541db","session_id":"348699cb-af2d-4672-9e14-653488d14c4d"}`
	// q6-bash-bg-and-cron: a backgrounded shell command of the parent's.
	lineBashBackground = `{"type":"system","subtype":"task_started","task_id":"bteas9hyi",` +
		`"run_id":"0mwsxeiji-cd481855","tool_use_id":"p_bg","description":"sleep 6.2; touch marks/bash-bg-done",` +
		`"is_backgrounded":true,"task_type":"local_bash","uuid":"6d51febf-aa7f-42db-86e0-6a1b2c3d4e5f",` +
		`"session_id":"348699cb-af2d-4672-9e14-653488d14c4e"}`
)

// TestScanDelegation: a run granted subagents may start foreground agents
// one level down and nothing more; a run granted none may start no agent;
// no run may start a workflow; a shell command is never delegation. The
// reason names only maul's own words and the event's type, depth and
// background flag, never the model-written description, prompt or name.
func TestScanDelegation(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		subagents bool
		want      bool   // forbidden
		reason    string // a part of the reason, when forbidden
	}{
		{"a foreground depth-1 agent, granted", lineAgentForeground, true, false, ""},
		{"a foreground Explore agent, granted", lineAgentExplore, true, false, ""},
		{"a foreground depth-1 agent, not granted", lineAgentForeground, false, true,
			"this run grants no subagents"},
		{"a depth-2 agent, granted", lineAgentDepth2, true, true, "depth 2"},
		{"a depth-2 agent, not granted", lineAgentDepth2, false, true, "this run grants no subagents"},
		{"a background agent, granted", lineAgentBackground, true, true, "in the background"},
		{"a background agent, not granted", lineAgentBackground, false, true, "this run grants no subagents"},
		{"a workflow, granted subagents", lineWorkflow, true, true, "workflow"},
		{"a workflow, not granted", lineWorkflow, false, true, "workflow"},
		{"a child's shell command", lineChildBash, false, false, ""},
		{"a child's shell command, granted", lineChildBash, true, false, ""},
		{"a background shell command", lineBashBackground, false, false, ""},
		{"a foreground shell command", lineTaskStarted, false, false, ""},
		// Every measured agent start carries its depth; one that does not
		// cannot be shown to be one level down.
		{"an agent with no depth, granted", `{"type":"system","subtype":"task_started","task_id":"a1",` +
			`"task_type":"local_agent","is_backgrounded":false}`, true, true, "depth unknown"},
		{"an agent at depth 1 naming a parent task, granted", `{"type":"system","subtype":"task_started",` +
			`"task_id":"a2","task_type":"local_agent","is_backgrounded":false,"spawn_depth":1,` +
			`"parent_task_id":"a1"}`, true, true, "started by another subagent"},
		// A task type never measured (Monitor's, say) is not judged here.
		{"an unknown task type", `{"type":"system","subtype":"task_started","task_id":"m1",` +
			`"task_type":"local_monitor","is_backgrounded":true}`, false, false, ""},
		{"a task's end", lineTaskNotification, false, false, ""},
		// Only the CLI's own system events count: a tool result or the
		// model's text quoting one is inert.
		{"a tool result quoting a start", `{"type":"user","message":{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"toolu_01","content":` +
			`"{\"type\":\"system\",\"subtype\":\"task_started\",\"task_type\":\"local_workflow\"}"}]}}`,
			false, false, ""},
		{"model text quoting a start", `{"type":"assistant","message":{"content":[{"type":"text",` +
			`"text":"\"subtype\":\"task_started\",\"task_type\":\"local_agent\""}]}}`, false, false, ""},
		{"not json", `"task_started" local_agent`, false, false, ""},
		{"the result", `{"type":"result","subtype":"success"}`, false, false, ""},
	}
	c := NewClaude()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := c.ScanDelegation([]byte(tt.line), tt.subagents)
			if got != tt.want {
				t.Fatalf("ScanDelegation() = (%v, %q), want forbidden %v", got, reason, tt.want)
			}
			if !got {
				if reason != "" {
					t.Errorf("reason = %q for a permitted line, want none", reason)
				}
				return
			}
			if !strings.Contains(reason, tt.reason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.reason)
			}
			for _, leak := range []string{"spike", "CHILD-", "general-purpose", "Explore", "export const"} {
				if strings.Contains(reason, leak) {
					t.Errorf("reason = %q quotes the event's text %q", reason, leak)
				}
			}
		})
	}
}

// TestClaudeIsADelegationScanner: claude is the harness whose CLI can
// delegate, so it is the one that watches for it.
func TestClaudeIsADelegationScanner(t *testing.T) {
	var h Harness = NewClaude()
	if _, ok := h.(DelegationScanner); !ok {
		t.Fatal("claude does not implement DelegationScanner")
	}
}
