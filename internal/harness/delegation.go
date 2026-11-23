// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// markTaskStarted is what a line must hold to carry a start; a line holding
// it is only a candidate, and the decode decides.
var markTaskStarted = []byte(`"task_started"`)

// ScanDelegation reports a start of delegated work the run was not granted;
// see scanDelegation.
func (c *Claude) ScanDelegation(line []byte, subagents bool) (bool, string) {
	return scanDelegation(line, subagents)
}

// scanDelegation judges one stream-json line. Only the CLI's own system
// events are read, so a tool result or the model's text quoting one is
// inert. A workflow is never granted. An agent is granted only to a run
// granted subagents, and then only one the run itself started (depth 1, no
// parent task) in the foreground: a subagent's own subagents and background
// ones are not, nor one whose depth the event does not state. Shell
// commands and task types never measured pass. The reason is maul's words
// and the event's type, depth and background flag: its description, prompt
// and names are the model's or the repository's, and the reason is bound
// for the stage's detail and its transcript.
func scanDelegation(line []byte, subagents bool) (bool, string) {
	if !bytes.Contains(line, markTaskStarted) {
		return false, ""
	}
	var ev claudeTaskEvent
	if json.Unmarshal(line, &ev) != nil || ev.Type != "system" || ev.Subtype != "task_started" {
		return false, ""
	}
	switch ev.TaskType {
	case taskTypeWorkflow:
		return true, "the agent delegated to a workflow, which no run is granted"
	case taskTypeAgent:
		return agentRefusal(ev, subagents)
	}
	return false, ""
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
