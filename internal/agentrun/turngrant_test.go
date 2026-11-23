// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/transcript"
)

// parentCall is one main-thread model call's event; a call's blocks repeat
// its id.
func parentCall(id string) string {
	return `{"type":"assistant","message":{"id":"` + id + `","content":[{"type":"text","text":"parent ` + id +
		`"}]}}`
}

// childCall is one call of the foreground subagent lineSubagentStarted
// starts (its Agent call is t_agent).
func childCall(id string) string {
	return `{"type":"assistant","message":{"id":"` + id + `","content":[{"type":"text","text":"child ` + id +
		`"}]},"parent_tool_use_id":"t_agent"}`
}

// toolResult is the main thread's user event between two of its calls.
const toolResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result",` +
	`"tool_use_id":"t_x","content":"ok"}]}}`

// wakeResult is a result event a woken session prints before its next
// calls: a wake must not reset the run's count.
const wakeResult = `{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":2}`

// delegatingStream is a granted build's stream: the parent makes calls
// p1..p<parents> (p1 as two blocks of one id), its last one the Agent call,
// whose foreground child then makes c1..c<children> (c1 as two blocks).
// Before its last call the parent may be woken (wake).
func delegatingStream(parents, children int, wake bool) []string {
	var lines []string
	for i := 1; i <= parents; i++ {
		if i == parents && wake {
			lines = append(lines, wakeResult)
		}
		id := fmt.Sprintf("msg_p%d", i)
		lines = append(lines, parentCall(id))
		if i == 1 {
			lines = append(lines, parentCall(id))
		}
		if i < parents {
			lines = append(lines, toolResult)
		}
	}
	lines = append(lines, lineSubagentStarted)
	for i := 1; i <= children; i++ {
		id := fmt.Sprintf("msg_c%d", i)
		lines = append(lines, childCall(id))
		if i == 1 {
			lines = append(lines, childCall(id))
		}
	}
	return lines
}

// buildGranted is a build stage granted subagents and turns, on brokered
// claude, with its repair case.
func buildGranted(t *testing.T, out *bytes.Buffer, turns int) (Config, string, repairCase) {
	t.Helper()
	i := slices.IndexFunc(repairCases(), func(c repairCase) bool { return c.phase == PhaseBuild })
	c := repairCases()[i]
	cfg, ws := c.setup(t, out)
	cfg.GrantedMaxTurns = turns
	cfg.BuildSubagents = true
	return cfg, ws, c
}

// TestTurnGrantCoversSubagents: a build granted subagents is held to its
// turn grant by its parent's and its subagent's model calls together, which
// the CLI's own --max-turns (the parent's alone) does not do. The run that
// passes it ends at that line, budget_exceeded as the token kill switch
// makes it, with no report read and no repair: its first run's report is a
// bad one, which settleReport would have asked to have repaired.
func TestTurnGrantCoversSubagents(t *testing.T) {
	cases := []struct {
		name string
		wake bool
	}{
		{"one answer", false},
		{"woken before its Agent call", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws, c := buildGranted(t, &out, 10)
			first := c.first(ws, c.bad)
			first.budgetLines = delegatingStream(3, 12, tc.wake)
			fx := &fakeExec{steps: []step{first}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))

			want := "turn grant exceeded (3 parent + 8 subagent turns > 10)"
			if stage.Outcome != envelope.OutcomeBudgetExceeded || stage.Detail != want {
				t.Fatalf("stage = %q (detail %q), want %q (%q)", stage.Outcome, stage.Detail,
					envelope.OutcomeBudgetExceeded, want)
			}
			if len(fx.specs) != 1 {
				t.Errorf("%d runs, want the first alone: an ended run is not repaired", len(fx.specs))
			}
			argv := fx.specs[0].Argv
			if i := slices.Index(argv, "--max-turns"); i < 0 || i+1 >= len(argv) || argv[i+1] != "10" {
				t.Errorf("argv %v, want the parent still capped at --max-turns 10", argv)
			}
			got := turns(t, out.String())
			last := got[len(got)-1]
			if last.Kind != transcript.KindNotice || last.Text != want {
				t.Errorf("last turn = %+v, want the notice %q", last, want)
			}
			if prev := got[len(got)-2]; !strings.Contains(prev.Text, "child msg_c8") {
				t.Errorf("turn before the notice = %+v, want the subagent's eighth call", prev)
			}
		})
	}
}

// TestTurnGrantAdmitsItsLastTurn: a run whose calls come to its grant, and
// no more, goes on.
func TestTurnGrantAdmitsItsLastTurn(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildGranted(t, &out, 10)
	first := c.first(ws, c.good)
	first.budgetLines = delegatingStream(3, 7, false)
	stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
	if stage.Outcome != envelope.OutcomeOK {
		t.Fatalf("outcome = %q (detail %q), want ok", stage.Outcome, stage.Detail)
	}
}

// TestTurnGrantAbsentWithoutSubagents: a run not granted subagents has no
// combined turn watcher, whatever its stage: its turns are the CLI's
// --max-turns, as before. The stream here shows no subagent start, so the
// delegation switch does not end it either.
func TestTurnGrantAbsentWithoutSubagents(t *testing.T) {
	stream := delegatingStream(3, 300, false)
	stream = slices.DeleteFunc(stream, func(l string) bool { return l == lineSubagentStarted })
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			cfg.GrantedMaxTurns = 10
			first := c.first(ws, c.good)
			first.budgetLines = stream
			stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
			if stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (detail %q), want ok", stage.Outcome, stage.Detail)
			}
		})
	}
}

// TestSubagentGrantIsTheDelegationSwitchs: the grant is the one the
// delegation switch reads, so a granted build's foreground depth-1 subagent
// passes it (TestTurnGrantAdmitsItsLastTurn) and what the grant never
// admits, one in the background, still ends the run.
func TestSubagentGrantIsTheDelegationSwitchs(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildGranted(t, &out, 10)
	first := c.first(ws, c.good)
	first.budgetLines = []string{strings.Replace(lineSubagentStarted, `"is_backgrounded":false`,
		`"is_backgrounded":true`, 1)}
	stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
	if stage.Outcome != envelope.OutcomeBudgetExceeded || !strings.Contains(stage.Detail, "in the background") {
		t.Fatalf("stage = %q (detail %q), want budget_exceeded on the background subagent", stage.Outcome,
			stage.Detail)
	}
}

// TestRepairIsNotGrantedSubagents: a granted build's report repair is a
// short fix, run without subagents, so a repair that delegates is refused
// as on any other stage.
func TestRepairIsNotGrantedSubagents(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildGranted(t, &out, 0)
	fx := &fakeExec{steps: []step{
		c.first(ws, c.bad),
		{ws: ws, stdout: streamResumed, budgetLines: []string{lineSubagentStarted},
			writes: map[string]string{c.report: c.good}},
	}}
	stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
	if stage.Outcome != envelope.OutcomeReportInvalid || !strings.Contains(stage.Detail, "grants no subagents") {
		t.Fatalf("stage = %q (detail %q), want report_invalid on the repair's delegation", stage.Outcome,
			stage.Detail)
	}
}
