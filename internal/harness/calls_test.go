// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"bytes"
	"testing"
)

// TestClaudeScanCall: a model call is a top-level assistant event, the
// main thread's or a subagent's (one carrying a parent_tool_use_id); every
// other line, the user event carrying a tool result that quotes one
// included, is none.
func TestClaudeScanCall(t *testing.T) {
	cases := []struct {
		name, line string
		id         string
		subagent   bool
		ok         bool
	}{
		{"main thread", `{"type":"assistant","message":{"id":"msg_p1","content":[{"type":"text","text":"x"}]}}`,
			"msg_p1", false, true},
		{"subagent", `{"type":"assistant","message":{"id":"msg_c1","content":[]},"parent_tool_use_id":"t_agent"}`,
			"msg_c1", true, true},
		{"no id", `{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`, "", false, true},
		{"subagent, no id", `{"type":"assistant","message":{},"parent_tool_use_id":"t_agent"}`, "", true, true},
		{"user", `{"type":"user","message":{"role":"user","content":[]},"parent_tool_use_id":"t_agent"}`,
			"", false, false},
		{"quoted in a tool result", `{"type":"user","message":{"content":[{"type":"tool_result",` +
			`"content":"{\"type\":\"assistant\",\"message\":{\"id\":\"msg_fake\"}}"}]}}`, "", false, false},
		{"result", `{"type":"result","subtype":"success","num_turns":4}`, "", false, false},
		{"system", `{"type":"system","subtype":"task_progress"}`, "", false, false},
		{"not JSON", `assistant`, "", false, false},
		{"empty", ``, "", false, false},
	}
	c := NewClaude()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, subagent, ok := c.ScanCall([]byte(tc.line))
			if id != tc.id || subagent != tc.subagent || ok != tc.ok {
				t.Errorf("ScanCall = (%q, %v, %v), want (%q, %v, %v)", id, subagent, ok, tc.id, tc.subagent, tc.ok)
			}
		})
	}
}

// TestClaudeIsACallScanner: claude is the harness whose stream shows a
// subagent's model calls, so it is the one a combined turn grant reads.
func TestClaudeIsACallScanner(t *testing.T) {
	var h Harness = NewClaude()
	if _, ok := h.(CallScanner); !ok {
		t.Fatal("claude does not implement CallScanner")
	}
}

// TestScanCallAgreesWithTheResult: over the pinned CLI's own streams, the
// distinct subagent calls ScanCall finds, line by line, are the subagent
// turns ParseResult records for the run, and the main thread's are the
// calls the scripted model was asked for (q4-fg: 3; its num_turns is 4,
// since the CLI counts the Agent call's result as a turn of its own).
func TestScanCallAgreesWithTheResult(t *testing.T) {
	cases := []struct {
		fixture string
		parent  int
	}{
		{"subagent-foreground", 3},
		{"subagent-background", 5},
		{"subagent-nested", 3},
		{"subagent-resume", 2},
	}
	c := NewClaude()
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			stream := readFixture(t, tc.fixture)
			parents, children := map[string]bool{}, map[string]bool{}
			for line := range bytes.SplitSeq(stream, []byte{'\n'}) {
				id, subagent, ok := c.ScanCall(line)
				if !ok {
					continue
				}
				if id == "" {
					t.Fatalf("a call with no id in a fixture: %s", line)
				}
				if subagent {
					children[id] = true
				} else {
					parents[id] = true
				}
			}
			res, ok := c.ParseResult(stream)
			if !ok {
				t.Fatal("no result")
			}
			if len(children) != res.SubagentTurns {
				t.Errorf("subagent calls = %d, want ParseResult's SubagentTurns %d", len(children), res.SubagentTurns)
			}
			if len(parents) != tc.parent {
				t.Errorf("main-thread calls = %d, want %d", len(parents), tc.parent)
			}
		})
	}
}
