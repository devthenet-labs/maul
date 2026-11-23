// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"math"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
)

// streamDelegated is a claude run that spawned a subagent, shaped as the
// 2.1.322 pin reports one: the child's assistant events carry the Agent
// call's id as parent_tool_use_id (its first message split over two
// events), and the result's top-level usage is the main thread's alone
// (30 in, 300 out) while model_usage_since_last_result holds everything
// the run spent. The fakeExec streams no line to the observer, so the
// delegation watcher never sees the child start; only the accounting does.
const streamDelegated = `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"id":"msg_p1","usage":{"input_tokens":10,"output_tokens":1},` +
	`"content":[{"type":"tool_use","id":"t_agent","name":"Agent","input":{"prompt":"look"}}]}}` + "\n" +
	`{"type":"assistant","parent_tool_use_id":"t_agent","message":{"id":"msg_c1",` +
	`"usage":{"input_tokens":20,"output_tokens":1},"content":[{"type":"thinking","thinking":""}]}}` + "\n" +
	`{"type":"assistant","parent_tool_use_id":"t_agent","message":{"id":"msg_c1",` +
	`"usage":{"input_tokens":20,"output_tokens":1},` +
	`"content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"true"}}]}}` + "\n" +
	`{"type":"assistant","parent_tool_use_id":"t_agent","message":{"id":"msg_c2",` +
	`"usage":{"input_tokens":20,"output_tokens":1},"content":[{"type":"text","text":"found it"}]}}` + "\n" +
	`{"type":"result","subtype":"success","is_error":false,"result":"Done.",` +
	`"session_id":"` + testSessionID + `","num_turns":3,"total_cost_usd":0.75,` +
	`"usage":{"input_tokens":30,"cache_creation_input_tokens":0,"cache_read_input_tokens":3000,"output_tokens":300},` +
	`"modelUsage":{"m":{"inputTokens":130,"outputTokens":35300,"cacheReadInputTokens":253000,` +
	`"cacheCreationInputTokens":40}},"model_usage_since_last_result":{"m":{"inputTokens":130,` +
	`"outputTokens":35300,"cacheReadInputTokens":253000,"cacheCreationInputTokens":40}},` +
	`"subagent_stats":{"spawned":1}}`

// delegatedUsage is what streamDelegated's run spent, its child included.
var delegatedUsage = envelope.Usage{InputTokens: 130, OutputTokens: 35300, CacheReadTokens: 253000,
	CacheCreationTokens: 40, CostUSD: 0.75}

// TestStagesRecordSubagents: every stage records what a run that delegated
// spent, its subagents included, so an intent's cost ceiling and the rollups,
// which read the stage's usage, see the child's tokens; num_turns stays the
// main thread's, and the children's turns are recorded beside it.
func TestStagesRecordSubagents(t *testing.T) {
	for _, c := range repairCases() {
		t.Run(string(c.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := c.setup(t, &out)
			first := c.first(ws, c.good)
			first.stdout = streamDelegated
			fx := &fakeExec{steps: []step{first}}
			stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
			if stage.Outcome != envelope.OutcomeOK || len(fx.specs) != 1 {
				t.Fatalf("outcome %q (detail %q) after %d commands, want ok after one", stage.Outcome, stage.Detail,
					len(fx.specs))
			}
			if stage.NumTurns != 3 || stage.SubagentTurns != 2 {
				t.Errorf("turns = (%d, %d subagent), want (3, 2)", stage.NumTurns, stage.SubagentTurns)
			}
			if !sameTokens(stage.Usage, delegatedUsage) || math.Abs(stage.Usage.CostUSD-0.75) > 1e-9 {
				t.Errorf("usage = %+v, want %+v: the child's tokens, not the main thread's alone", stage.Usage,
					delegatedUsage)
			}
		})
	}
}

// TestRepairWindowCountsSubagentTurns: the turns a repair may still take are
// what the stage's cap leaves after the main thread's turns and its
// subagents' together. 3 + 2 of the stage's 7 leave 2, fewer than a repair
// needs; counting the main thread alone would have left 4.
func TestRepairWindowCountsSubagentTurns(t *testing.T) {
	investigate := repairCases()[0]
	var out bytes.Buffer
	cfg, ws := investigate.setup(t, &out)
	cfg.InvestigateMaxTurns = 7
	first := investigate.first(ws, investigate.bad)
	first.stdout = streamDelegated
	fixed := step{ws: ws, stdout: streamResumed, writes: map[string]string{investigate.report: investigate.good}}
	fx := &fakeExec{steps: []step{first, fixed}}
	stage := eventStage(t, onlyEvent(t, cfg, fx, &out))
	want := investigate.detailOf(t, investigate.bad) +
		" (not repaired: 2 turns left of the stage's 7, fewer than the 3 a repair needs)"
	if stage.Detail != want || len(fx.specs) != 1 {
		t.Errorf("detail %q after %d commands, want %q after 1", stage.Detail, len(fx.specs), want)
	}
}

// TestAddRepairSumsSubagentTurns: a repair run's subagent turns add to the
// stage's as its main-thread turns do, each on its own field.
func TestAddRepairSumsSubagentTurns(t *testing.T) {
	st := envelope.Stage{NumTurns: 7, SubagentTurns: 4}
	addRepair(&st, envelope.Stage{NumTurns: 2, SubagentTurns: 3})
	if st.NumTurns != 9 || st.SubagentTurns != 7 {
		t.Errorf("turns = (%d, %d subagent), want (9, 7)", st.NumTurns, st.SubagentTurns)
	}
}

// TestAddSessionSumsSubagentTurns: a check review's sessions add their
// subagent turns to the stage's as they add their main-thread turns.
func TestAddSessionSumsSubagentTurns(t *testing.T) {
	st := envelope.Stage{NumTurns: 5, SubagentTurns: 1}
	addSession(&st, envelope.Stage{NumTurns: 4, SubagentTurns: 2})
	if st.NumTurns != 9 || st.SubagentTurns != 3 {
		t.Errorf("turns = (%d, %d subagent), want (9, 3)", st.NumTurns, st.SubagentTurns)
	}
}
