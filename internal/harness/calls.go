// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"encoding/json"
)

// markAssistant is what a line must hold to carry a model call; a line
// holding it is only a candidate, and the decode decides.
var markAssistant = []byte(`"assistant"`)

// ScanCall reports the model call one stream line shows; see modelCall.
func (c *Claude) ScanCall(line []byte) (string, bool, bool) {
	if !bytes.Contains(line, markAssistant) {
		return "", false, false
	}
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil {
		return "", false, false
	}
	return modelCall(ev)
}

// modelCall reads a model call off one decoded stream-json event: a
// top-level assistant event, one per content block of the call, all of one
// message id. A subagent's carries the parent_tool_use_id of the Agent call
// it runs under. Only a foreground depth-1 subagent's calls are printed
// (measured on the 2.1.322 pin, and without --forward-subagent-text not its
// closing text-only one): see scanStream for what the stream leaves out.
func modelCall(ev claudeEvent) (id string, subagent, ok bool) {
	if ev.Type != "assistant" {
		return "", false, false
	}
	return ev.Message.ID, ev.ParentToolUseID != "", true
}
