// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/transcript"
)

// Captured claude 2.1.322 task_started events (the subagent spike's q2-today-fg
// and q5-allow-Workflow; prompts shortened).
const (
	lineSubagentStarted = `{"type":"system","subtype":"task_started","task_id":"ae281a09c5d7499eb",` +
		`"tool_use_id":"t_agent","description":"spike child","subagent_type":"general-purpose",` +
		`"is_backgrounded":false,"spawn_depth":1,"task_type":"local_agent","prompt":"CHILD-A-MARK do it",` +
		`"session_id":"236dd867-53be-4a72-af78-e12c00de88a3"}`
	lineWorkflowStarted = `{"type":"system","subtype":"task_started","task_id":"wudbpnxib",` +
		`"tool_use_id":"p_wf","description":"inline spike","task_type":"local_workflow",` +
		`"workflow_name":"spike-wf","session_id":"6de62539-6e90-493a-a232-af9b52fe3264"}`
	lineShellStarted = `{"type":"system","subtype":"task_started","task_id":"b578qoc1g",` +
		`"tool_use_id":"toolu_01","description":"Run the suite","task_type":"local_bash",` +
		`"is_backgrounded":true,"session_id":"5e3f9a1c-8b2d-4f6e-9c7a-1d2e3f4a5b6c"}`
)

// TestStagesEndOnUngrantedDelegation: no stage grants delegation, so a
// stream showing the CLI start a subagent or a workflow ends the run at
// that line, whichever stage and posture it is, with a token budget or none.
// The stage is budget_exceeded, as the runner's other kill switch makes it,
// with no report read and no repair asked; the reason is the envelope's
// detail and the transcript's last word. A subagent's start is recorded
// before the kill, as the notice of its start under the Agent call, so the
// transcript shows what was started and then why the run ended; so is a
// workflow's, under the Workflow call.
func TestStagesEndOnUngrantedDelegation(t *testing.T) {
	starts := []struct {
		name, line, reason string
		started            []transcript.Turn // the notices the start itself records
	}{
		{"a subagent", lineSubagentStarted, "this run grants no subagents", []transcript.Turn{{
			Kind: transcript.KindNotice, Text: "subagent general-purpose started", Parent: "t_agent",
		}}},
		{"a workflow", lineWorkflowStarted, "workflow", []transcript.Turn{{
			Kind: transcript.KindNotice, Text: "workflow started", Parent: "p_wf",
		}}},
	}
	for _, c := range repairCases() {
		for _, s := range starts {
			t.Run(string(c.phase)+"/"+s.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := c.setup(t, &out)
				// No budget for the read-only stages, so the kill is not the
				// budget watcher's.
				cfg.InvestigateTokenBudget = 0
				first := c.first(ws, c.good)
				first.budgetLines = []string{
					`{"type":"assistant","message":{"content":[{"type":"text","text":"delegating"}]}}`,
					s.line,
					`{"type":"assistant","message":{"content":[{"type":"text","text":"never seen"}]}}`,
				}
				fx := &fakeExec{steps: []step{first}}
				stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
				if stage.Outcome != envelope.OutcomeBudgetExceeded {
					t.Fatalf("outcome = %q (detail %q), want %q", stage.Outcome, stage.Detail,
						envelope.OutcomeBudgetExceeded)
				}
				if !strings.Contains(stage.Detail, "delegat") || !strings.Contains(stage.Detail, s.reason) {
					t.Errorf("detail = %q, want the delegation it refused, %q", stage.Detail, s.reason)
				}
				if len(fx.specs) != 1 {
					t.Errorf("%d runs, want the first alone: an ended run is not repaired", len(fx.specs))
				}
				got := turns(t, out.String())
				if len(got) != 2+len(s.started) {
					t.Fatalf("got %d turns, want the turn before the start, the start's own notices, then the "+
						"reason: %+v", len(got), got)
				}
				if got[0].Text != "delegating" {
					t.Errorf("turn 0 = %q, want the turn before the start", got[0].Text)
				}
				for i, w := range s.started {
					if g := got[1+i]; g.Kind != w.Kind || g.Text != w.Text || g.Parent != w.Parent {
						t.Errorf("turn %d = %+v, want %+v", 1+i, g, w)
					}
				}
				last := got[len(got)-1]
				if last.Kind != transcript.KindNotice || last.Text != stage.Detail || last.Parent != "" {
					t.Errorf("last turn = %+v, want a notice saying %q", last, stage.Detail)
				}
			})
		}
	}
}

// TestStagesKeepShellCommands: a shell command, a background one included,
// is the CLI's own work, not delegation; the run goes on.
func TestStagesKeepShellCommands(t *testing.T) {
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			first := c.first(ws, c.good)
			first.budgetLines = []string{lineShellStarted}
			stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
			if stage.Outcome != envelope.OutcomeOK {
				t.Fatalf("outcome = %q (detail %q), want ok", stage.Outcome, stage.Detail)
			}
		})
	}
}

// TestRepairEndsOnDelegation: a repair run is held to the same rule; its
// ending is named in the stage's detail.
func TestRepairEndsOnDelegation(t *testing.T) {
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			fx := &fakeExec{steps: []step{
				c.first(ws, c.bad),
				{ws: ws, stdout: streamResumed, budgetLines: []string{lineSubagentStarted},
					writes: map[string]string{c.report: c.good}},
			}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
			if stage.Outcome != envelope.OutcomeReportInvalid {
				t.Fatalf("outcome = %q (detail %q), want %q", stage.Outcome, stage.Detail,
					envelope.OutcomeReportInvalid)
			}
			want := "the last repair run ended budget_exceeded: "
			if !strings.Contains(stage.Detail, want) || !strings.Contains(stage.Detail, "grants no subagents") {
				t.Errorf("detail = %q, want %q and the delegation it refused", stage.Detail, want)
			}
		})
	}
}

// TestEvaluateCoverAskEndsOnDelegation: the evaluator's ask for the checks
// of its uncovered passes is held to the same rule. An ask that delegates
// is ended there; the report it rewrote, a good one, is not read, the one
// before stands and no further ask is made.
func TestEvaluateCoverAskEndsOnDelegation(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	cfg.InvestigateMaxTurns = 100
	useTools(t, &cfg)
	rewritten := proposingEvaluation("rate-battle.api", "rate-battle", checks.Digest(rateBattleCheck))
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": goodEvaluation}},
		{ws: ws, stdout: streamResumed, budgetLines: []string{lineSubagentStarted}, writes: map[string]string{
			"scratch/report.md":                   rewritten,
			"scratch/checks/rate-battle-api.json": string(checks.Canonical(rateBattleCheck)),
		}},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation {
		t.Fatalf("evaluation = %+v, want the report before the ask", e)
	}
	if len(e.Proposed) != 0 {
		t.Errorf("proposed = %+v, want none: the ask's check is not read", e.Proposed)
	}
	if len(fx.specs) != 2 {
		t.Errorf("commands = %d, want the stage and the one ask", len(fx.specs))
	}
	got := turns(t, out.String())
	if n := len(got); n == 0 || got[n-1].Kind != transcript.KindNotice ||
		!strings.Contains(got[n-1].Text, "grants no subagents") {
		t.Errorf("transcript = %+v, want it to end on the delegation refused", got)
	}
}
