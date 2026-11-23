// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// What claude 2.1.322 prints when work leaves the main thread (probed): a
// system event of subtype task_started, task_type "local_agent" for a
// subagent (spawn_depth 1 for one the run started, 2 and a parent_task_id
// for one a subagent started; is_backgrounded true unless the call asked
// for the foreground) and "local_workflow" for a workflow. A subagent's
// shell commands are "local_bash" starts like the run's own.

// The task types a start is judged by.
const (
	taskTypeAgent    = "local_agent"
	taskTypeWorkflow = "local_workflow"
)

// markTaskStarted is what a line must hold to carry a start, and
// markTaskProgress a task's progress; a line holding one is only a
// candidate, and the decode decides.
var (
	markTaskStarted  = []byte(`"task_started"`)
	markTaskProgress = []byte(`"task_progress"`)
)

// claudeBuiltinWorkflows are the workflows claude 2.1.322 carries itself,
// the ones a run granted workflows may ask for by name (probed: a call for
// an unknown name, with nothing under HOME, answers "Available:
// deep-research").
var claudeBuiltinWorkflows = []string{"deep-research"}

// ClaudeBuiltinWorkflows is the workflows a run granted workflows may ask
// for by name: the ones the pinned CLI carries itself.
func ClaudeBuiltinWorkflows() []string { return slices.Clone(claudeBuiltinWorkflows) }

// ScanDelegation reports a start of delegated work the run was not granted;
// see scanDelegation.
func (c *Claude) ScanDelegation(line []byte, grant Delegation) (bool, string) {
	return scanDelegation(line, grant)
}

// ScanProgress reports a task's progress event, which is how a workflow
// shows its agents working: they print no turns (claude 2.1.322, probed).
// Only the CLI's own system events count.
func (c *Claude) ScanProgress(line []byte) bool {
	if !bytes.Contains(line, markTaskProgress) {
		return false
	}
	var ev claudeTaskEvent
	return json.Unmarshal(line, &ev) == nil && ev.Type == "system" && ev.Subtype == "task_progress"
}

// scanDelegation judges one stream-json line. Only the CLI's own system
// events and the model's own tool calls are read, so a tool result or the
// model's text quoting one is inert. A workflow is granted only to a run
// granted workflows, and then only one asked for inline or by a built-in
// name (claudeWorkflowCall), which its start does not show, so its call is
// judged. An agent is granted only to a run granted subagents, and then only
// one the run itself started (depth 1, no parent task) in the foreground: a
// subagent's own subagents and background ones are not, nor one whose depth
// the event does not state. Shell commands and task types never measured
// pass. The reason is maul's words and the event's type, depth and
// background flag: its description, prompt, names and paths are the model's
// or the repository's, and the reason is bound for the stage's detail and
// its transcript.
func scanDelegation(line []byte, grant Delegation) (bool, string) {
	if grant.Workflows && bytes.Contains(line, markWorkflowCall) {
		if forbidden, reason := claudeWorkflowCall(line); forbidden {
			return true, reason
		}
	}
	if !bytes.Contains(line, markTaskStarted) {
		return false, ""
	}
	var ev claudeTaskEvent
	if json.Unmarshal(line, &ev) != nil || ev.Type != "system" || ev.Subtype != "task_started" {
		return false, ""
	}
	switch ev.TaskType {
	case taskTypeWorkflow:
		if grant.Workflows {
			return false, ""
		}
		return true, "the agent delegated to a workflow, which this run is not granted"
	case taskTypeAgent:
		return agentRefusal(ev, grant.Subagents)
	}
	return false, ""
}

// workflowAbsent reports whether a workflow call's value is one the CLI
// reads as absent: null, false or the empty string.
func workflowAbsent(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "null", "false", `""`:
		return true
	}
	return false
}

// markWorkflowCall is what a line must hold to carry a workflow call: its
// tool's name, which its alias (RunWorkflow) holds too.
var markWorkflowCall = []byte(`Workflow"`)

// claudeWorkflowCall judges the workflow calls on one assistant event of a
// run granted workflows: a call must ask for a script written inline
// (script) or a built-in workflow by name (name), each with or without args,
// and nothing else. A call naming a script path, a run to resume, a run
// operation or another workflow's name is forbidden: a path reads a script
// from a file, the tree's included, and a name other than a built-in one
// reads one from the user source, which no rule of the CLI's refuses (its
// permission rules match a call's name only; probed on 2.1.322). The call is
// judged before the CLI runs it, but the run is ended only as the CLI may
// already be starting it: like every delegation check, after the fact. An
// inline script that itself calls workflow({scriptPath}) passes: it cannot
// be told from any other inline script, and the pinned CLI runs it.
func claudeWorkflowCall(line []byte) (bool, string) {
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" {
		return false, ""
	}
	for _, b := range ev.Message.Content {
		if b.Type != "tool_use" || (b.Name != claudeWorkflowTool && b.Name != "RunWorkflow") {
			continue
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(b.Input, &input) != nil {
			return true, "the agent asked for a workflow this run could not read; it is granted inline and " +
				"built-in workflows only"
		}
		if forbidden, reason := workflowInputRefusal(input); forbidden {
			return true, reason
		}
	}
	return false, ""
}

// workflowInputRefusal judges one workflow call's input; see
// claudeWorkflowCall. A key the CLI reads as absent (null, false or empty,
// claude 2.1.322: it tests a script path, a name and a resume for truth) is
// absent here too, so a call the CLI runs inline is never ended for one; a
// run operation is one by its key's type alone, an empty one included.
func workflowInputRefusal(input map[string]json.RawMessage) (bool, string) {
	const granted = "; this run is granted inline and built-in workflows only"
	var script, name string
	for key, raw := range input {
		switch {
		case key == "args":
		case key == "script":
			if json.Unmarshal(raw, &script) != nil {
				script = ""
			}
		case key == "name" && workflowAbsent(raw):
		case key == "name":
			if json.Unmarshal(raw, &name) != nil || name == "" {
				name = "\x00"
			}
		case key != "runId" && workflowAbsent(raw):
		default:
			return true, "the agent asked for a workflow by a script path, a resumed run or a run operation" + granted
		}
	}
	switch {
	case script != "" && name == "":
		return false, ""
	case script == "" && slices.Contains(claudeBuiltinWorkflows, name):
		return false, ""
	case script == "" && name != "":
		return true, "the agent asked for a workflow by a name that is not a built-in one" + granted
	}
	return true, "the agent asked for a workflow that is not one script written inline" + granted
}

// agentRefusal judges a subagent's start; see scanDelegation.
func agentRefusal(ev claudeTaskEvent, subagents bool) (bool, string) {
	const started = "the agent delegated to a subagent"
	switch {
	case !subagents:
		return true, started + ", and this run grants no subagents"
	case ev.SpawnDepth == 0:
		return true, started + " at a depth unknown; this run grants subagents at depth 1 only"
	case ev.SpawnDepth != 1:
		return true, fmt.Sprintf("%s at depth %d; this run grants subagents at depth 1 only", started,
			ev.SpawnDepth)
	case ev.ParentTaskID != "":
		return true, started + " started by another subagent; this run grants subagents at depth 1 only"
	case ev.IsBackgrounded:
		return true, started + " in the background; this run grants foreground subagents only"
	}
	return false, ""
}
