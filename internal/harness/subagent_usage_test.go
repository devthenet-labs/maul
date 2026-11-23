// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are streams of the pinned CLI (2.1.322) run against a
// scripted model on loopback, with the init events dropped and paths
// scrubbed. The scripted model gives the parent 100 output tokens a call and
// a subagent 7,000, so the figures are traceable.
//
//   - subagent-foreground: one foreground child; one result whose top-level
//     usage is the main thread's (30 in, 300 out) while modelUsage holds the
//     child too (130 in, 35,300 out).
//   - subagent-background: three background children; four result events
//     (num_turns 4, 0, 0, 1), the middle two empty.
//   - subagent-nested: a child that spawned two of its own; only the first
//     child's events are in the stream.
//   - subagent-resume: a resumed session (a report repair) that delegated:
//     modelUsage is cumulative over the session (90 in, 14,500 out), while
//     model_usage_since_last_result counts this invocation alone (60, 14,200).
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude-2.1.322-"+name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type wantResult struct {
	in, out, read, write int
	cost                 float64
	turns, subagentTurns int
	final                string
}

func checkResult(t *testing.T, res AgentResult, ok bool, want wantResult) {
	t.Helper()
	if !ok {
		t.Fatal("ok = false, want a parsed result")
	}
	if res.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	got := [4]int{derefInt(res.Usage.InputTokens), derefInt(res.Usage.OutputTokens),
		derefInt(res.Usage.CacheReadTokens), derefInt(res.Usage.CacheCreationTokens)}
	if exp := [4]int{want.in, want.out, want.read, want.write}; got != exp {
		t.Errorf("usage (in, out, cache read, cache write) = %v, want %v", got, exp)
	}
	if res.Usage.CostUSD == nil || math.Abs(*res.Usage.CostUSD-want.cost) > 1e-12 {
		t.Errorf("CostUSD = %v, want total_cost_usd %v", res.Usage.CostUSD, want.cost)
	}
	if res.NumTurns != want.turns {
		t.Errorf("NumTurns = %d, want %d", res.NumTurns, want.turns)
	}
	if res.SubagentTurns != want.subagentTurns {
		t.Errorf("SubagentTurns = %d, want %d", res.SubagentTurns, want.subagentTurns)
	}
	if res.FinalText != want.final {
		t.Errorf("FinalText = %q, want %q", res.FinalText, want.final)
	}
}

// TestClaudeParseResultCountsSubagents: a run that spawned a subagent records
// the tokens every agent of this invocation spent, not the main thread's
// alone, its parent's turns over every result event, and its children's
// turns apart. The cost stays total_cost_usd, the last one reported.
func TestClaudeParseResultCountsSubagents(t *testing.T) {
	tests := []struct {
		fixture string
		want    wantResult
	}{
		// 300 is the main thread's; 35,300 includes the child's 5 calls.
		// The child shows 4 messages: its closing text-only call is not
		// streamed without --forward-subagent-text.
		{"subagent-foreground", wantResult{in: 130, out: 35300, read: 253000, cost: 0.75712,
			turns: 4, subagentTurns: 4, final: "parent-final"}},
		// Four results: turns 4+0+0+1, tokens summed over each result's
		// delta (29,000 then 100), the answer from the last that took turns.
		{"subagent-background", wantResult{in: 170, out: 29100, cost: 0.58268,
			turns: 5, subagentTurns: 6, final: "parent-final"}},
		// The grandchildren show no events, so they add no turns here; their
		// tokens are in the total all the same (118,322, against 300).
		{"subagent-nested", wantResult{in: 210, out: 118322, cost: 2.36728,
			turns: 6, subagentTurns: 3, final: "parent-final"}},
		// A resumed session: this invocation's 60 and 14,200, never the
		// session's cumulative 90 and 14,500, which would count the first
		// run's again when a repair's accounting is added to it.
		{"subagent-resume", wantResult{in: 60, out: 14200, cost: 0.29036,
			turns: 2, subagentTurns: 1, final: "parent-final"}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			res, ok := NewClaude().ParseResult(readFixture(t, tt.fixture))
			checkResult(t, res, ok, tt.want)
		})
	}
}

// emptyResult is a result event as the CLI emits one for a task
// notification that woke the session without a model call: no turns, no
// text, zero usage, the cumulative cost.
const emptyResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":0,"result":"",` +
	`"session_id":"` + claudeSessionID + `","total_cost_usd":0.58268,` +
	`"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},` +
	`"model_usage_since_last_result":{},"subagent_stats":{"spawned":3}}`

// TestClaudeParseResultTrailingEmptyResult: an empty zero-turn result after
// the one that answered blanks neither the answer nor the figures, and every
// reading of the run's end (runtime error, exhaustion, terminal error) reads
// the same result event.
func TestClaudeParseResultTrailingEmptyResult(t *testing.T) {
	c := NewClaude()
	stream := append(readFixture(t, "subagent-background"), []byte(emptyResult+"\n")...)
	res, ok := c.ParseResult(stream)
	checkResult(t, res, ok, wantResult{in: 170, out: 29100, cost: 0.58268,
		turns: 5, subagentTurns: 6, final: "parent-final"})
	if res.IsError || res.Subtype != "success" {
		t.Errorf("envelope = (%v, %q), want a clean success", res.IsError, res.Subtype)
	}

	failed := claudeStreamMaxTurnsPartial + "\n" + emptyResult
	if got, want := c.RuntimeError([]byte(failed), 1, false),
		"claude run error (error_max_turns): hit max turns"; got != want {
		t.Errorf("RuntimeError = %q, want %q", got, want)
	}
	if !c.Exhausted([]byte(failed)) {
		t.Error("Exhausted = false, want the max-turns result read past the empty one")
	}
	if got := c.TerminalError([]byte(failed)); got != "hit max turns; Partial fix applied." {
		t.Errorf("TerminalError = %q, want the max-turns result's", got)
	}
	if res, _ := c.ParseResult([]byte(failed)); !res.IsError || res.FinalText != "Partial fix applied." ||
		res.NumTurns != 30 {
		t.Errorf("ParseResult = %+v, want the max-turns result's envelope, text and turns", res)
	}
}

// TestClaudeParseResultWithoutSubagents: a run that spawned no subagent keeps
// the top-level figures exactly, though its modelUsage reports more (the
// CLI's own helper calls, never counted); a run woken more than once (a
// background shell's notification) sums them over its results.
func TestClaudeParseResultWithoutSubagents(t *testing.T) {
	c := NewClaude()
	helpers := `{"type":"result","subtype":"success","is_error":false,"result":"Done.",` +
		`"session_id":"` + claudeSessionID + `","num_turns":7,"total_cost_usd":0.0123,` +
		`"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":50,"output_tokens":30},` +
		`"modelUsage":{"m":{"inputTokens":900,"outputTokens":900,"cacheReadInputTokens":900,` +
		`"cacheCreationInputTokens":900}},"model_usage_since_last_result":{"m":{"inputTokens":900,` +
		`"outputTokens":900,"cacheReadInputTokens":900,"cacheCreationInputTokens":900}},` +
		`"subagent_stats":{"spawned":0}}`
	res, ok := c.ParseResult([]byte(helpers))
	checkResult(t, res, ok, wantResult{in: 100, out: 30, read: 50, write: 20, cost: 0.0123, turns: 7, final: "Done."})

	woken := helpers + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"Still done.",` +
		`"session_id":"` + claudeSessionID + `","num_turns":2,"total_cost_usd":0.02,` +
		`"usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":5,"output_tokens":3}}`
	res, ok = c.ParseResult([]byte(woken))
	checkResult(t, res, ok, wantResult{in: 110, out: 33, read: 55, write: 22, cost: 0.02, turns: 9,
		final: "Still done."})
}

// TestClaudeParseResultSubagentFallback: a CLI that reports no per-result
// delta has only the cumulative modelUsage, which a run that delegated then
// records from its last result.
func TestClaudeParseResultSubagentFallback(t *testing.T) {
	var lines []string
	for line := range strings.Lines(string(readFixture(t, "subagent-foreground"))) {
		if strings.Contains(line, `"type":"result"`) {
			line = strings.Replace(line, `"model_usage_since_last_result"`, `"ignored"`, 1)
		}
		lines = append(lines, line)
	}
	res, ok := NewClaude().ParseResult([]byte(strings.Join(lines, "")))
	checkResult(t, res, ok, wantResult{in: 130, out: 35300, read: 253000, cost: 0.75712,
		turns: 4, subagentTurns: 4, final: "parent-final"})
}

// TestClaudeStreamUsageCountsSubagents: a run cut off before its result
// event records the calls its stream shows, a subagent's beside the main
// thread's (7 messages: 110 in, 203,000 cache read), never the result's
// figures. Output stays the floor the stream stamps, one token a call here.
func TestClaudeStreamUsageCountsSubagents(t *testing.T) {
	var cut []string
	for line := range strings.Lines(string(readFixture(t, "subagent-foreground"))) {
		if !strings.Contains(line, `"type":"result"`) {
			cut = append(cut, line)
		}
	}
	got := usageOf(NewClaude().StreamUsage([]byte(strings.Join(cut, ""))))
	if want := [4]int{110, 203000, 0, 7}; got != want {
		t.Errorf("StreamUsage = %v, want %v", got, want)
	}
}
