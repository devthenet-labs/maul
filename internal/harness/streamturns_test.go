// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
)

func TestClaudeStreamTurns(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   int
	}{
		// Two API messages, the first split over two content-block events.
		{"cut off: each message once", claudeStreamCutOff, 2},
		{"no assistant event", `{"type":"system","subtype":"init"}` + "\n" + `{"type":"user","message":{}}`, 0},
		{"empty", "", 0},
		{"an event with no id counts as its own message",
			`{"type":"assistant","message":{"content":[]}}` + "\n" + `{"type":"assistant","message":{"content":[]}}` +
				"\n" + `{"type":"assistant","message":{"id":"m","content":[]}}`, 3},
		{"a result event is not a turn", `{"type":"result","num_turns":9}`, 0},
		{"garbage lines are skipped", "not json\n{\n" + `{"type":"assistant","message":{"id":"m"}}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, h := range []StreamTurnCounter{NewClaude(), NewFake()} {
				if got := h.StreamTurns([]byte(tt.stdout)); got != tt.want {
					t.Errorf("%T.StreamTurns = %d, want %d", h, got, tt.want)
				}
			}
		})
	}
}

// TestClaudeStreamTurnsProperty: however many events the CLI splits a
// message over, and however they interleave with other events, the count is
// the number of distinct message ids.
func TestClaudeStreamTurnsProperty(t *testing.T) {
	property := func(events []uint8, seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var lines []string
		for i, n := range events {
			for range n%4 + 1 {
				lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"id":"msg_%d","content":[]}}`, i))
			}
			lines = append(lines, `{"type":"user","message":{"content":[]}}`, `{"type":"system","subtype":"x"}`)
		}
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		return streamTurns([]byte(strings.Join(lines, "\n"))) == len(events)
	}
	cfg := &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(20261007))}
	if err := quick.Check(property, cfg); err != nil {
		t.Error(err)
	}
}
