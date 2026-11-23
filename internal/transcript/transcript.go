// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Prefix marks a turn line on the agent-runner's stdout. It is deliberately
// distinct from envelope.Prefix: the controller's result path scans for stage
// results, and a run's few hundred turns must not ride that slice.
const Prefix = "MAUL-TURN: "

// Version is the current turn schema version.
const Version = 1

// Role is who produced a turn.
type Role string

// The roles. Tool results are reported by the harness as user-role turns,
// matching how the model sees them.
const (
	RoleAssistant Role = "assistant"
	RoleUser      Role = "user"
	RoleSystem    Role = "system"
)

// Kind discriminates what a turn carries.
type Kind string

// The turn kinds. Notice is the recorder's own voice — session banners,
// truncation, and budget aborts — and never the agent's.
const (
	KindText       Kind = "text"
	KindThinking   Kind = "thinking"
	KindToolUse    Kind = "tool_use"
	KindToolResult Kind = "tool_result"
	KindNotice     Kind = "notice"
)

// Turn is one entry in an agent's conversation, normalised across harnesses.
// Text carries the message body, the tool invocation's rendered input, or the
// tool result's excerpt depending on Kind; Truncated reports that the recorder
// capped it.
//
// Call is the id of the tool call a tool_use turn makes, and Parent the id of
// the call a turn ran under: a subagent's turns, and the notices of its start
// and end, name the Agent call that started it, so a reader can nest them
// there. A main-thread turn has no Parent. Both are omitted when empty, so a
// turn of a run without subagents, and every line written before they
// existed, is unchanged and Version stays 1. Both are held to CleanRef.
type Turn struct {
	V         int    `json:"v"`
	Seq       int    `json:"seq"`
	At        string `json:"at,omitempty"`
	Role      Role   `json:"role"`
	Kind      Kind   `json:"kind"`
	Tool      string `json:"tool,omitempty"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Call      string `json:"call,omitempty"`
	Parent    string `json:"parent,omitempty"`
}

// MaxRefBytes bounds a call or parent id. The ids are the model API's
// (toolu_ and about 24 more characters on Anthropic's), well under it.
const MaxRefBytes = 64

// CleanRef is ref when it is a plain id (ASCII letters, digits, '_', '-' and
// '.', at most MaxRefBytes), and "" otherwise: an id is matched, never shown
// as text, so one that is anything else is dropped whole rather than cut or
// escaped, and a turn that loses it is shown where it falls, unnested.
func CleanRef(ref string) string {
	if len(ref) > MaxRefBytes {
		return ""
	}
	for i := range len(ref) {
		switch c := ref[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return ""
		}
	}
	return ref
}

// Encode renders a turn as its stdout line, without the trailing newline.
func Encode(t Turn) (string, error) {
	t.V = Version
	raw, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("transcript: encode: %w", err)
	}
	return Prefix + string(raw), nil
}

// Decode recovers a turn from one log line; ok is false for any line that is
// not a turn line.
func Decode(line []byte) (Turn, bool) {
	rest, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte(Prefix))
	if !found {
		// Kubernetes log lines may carry timestamps or the runtime may have
		// wrapped the line; find the prefix anywhere.
		if i := strings.Index(string(line), Prefix); i >= 0 {
			rest = line[i+len(Prefix):]
		} else {
			return Turn{}, false
		}
	}
	var t Turn
	if err := json.Unmarshal(rest, &t); err != nil {
		return Turn{}, false
	}
	if t.V != Version || t.Kind == "" {
		return Turn{}, false
	}
	return t, true
}

// HasPrefix reports whether a log line is a turn line. Callers scanning for
// some other prefix use it to skip turns cheaply, without a full decode.
func HasPrefix(line []byte) bool {
	return bytes.Contains(line, []byte(Prefix))
}

// Truncated reports whether any turn in a recorded transcript was cut, so a
// reader can be told the record is partial. It is derived from the turns
// themselves rather than reported alongside them: what survived into storage
// is the only honest account of what was kept.
func Truncated(turns []Turn) bool {
	for _, t := range turns {
		if t.Truncated {
			return true
		}
	}
	return false
}

// Truncate caps s at limit bytes on a rune boundary (the API server rejects
// invalid UTF-8, and a turn is stored in a ConfigMap). It reports whether it
// cut anything.
func Truncate(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}
