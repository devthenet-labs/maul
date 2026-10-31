// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/runner"
)

// streamMaxTurns is how claude ends a run that reached its --max-turns, as
// every live evaluate run of S1 bug 3 ended: an error result of subtype
// error_max_turns, exit status 1.
const streamMaxTurns = `{"type":"system","subtype":"init","session_id":"` + testSessionID + `"}` + "\n" +
	`{"type":"assistant","message":{"usage":{"output_tokens":12},` +
	`"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"maul-tool check run checks/a.json"}}]}}` +
	"\n" +
	`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"",` +
	`"session_id":"` + testSessionID + `","num_turns":90,"total_cost_usd":1.70,` +
	`"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":50,"output_tokens":30},` +
	`"errors":["Reached maximum number of turns (90)"]}`

// turnCapConfig is an evaluate Job on brokered claude whose stage may take
// 100 turns.
func turnCapConfig(t *testing.T, out *bytes.Buffer) (Config, string) {
	t.Helper()
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, out)
	brokeredClaude(t, &cfg)
	cfg.InvestigateMaxTurns = 100
	return cfg, ws
}

// TestEvaluateTurnCapCollectsTheReport (S1 bug 3): an evaluate run keeps the
// last turns of its stage for its report. A run that reaches its own cap
// before writing one is not thrown away: its session is resumed once, told
// it ran out of turns, and the report it then writes from what it has is
// the stage's, the turns of both runs counted. The prompt states the turns
// the first run has and that it should report before they run out.
func TestEvaluateTurnCapCollectsTheReport(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := turnCapConfig(t, &out)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamMaxTurns, result: runner.Result{ExitCode: 1}},
		{ws: ws, stdout: streamResumed, writes: map[string]string{"scratch/report.md": goodEvaluation}},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation || len(e.Verdicts) != 2 {
		t.Fatalf("evaluation = %+v, want the report the resumed session wrote", e)
	}
	if len(fx.specs) != 2 || !slices.Contains(fx.specs[1].Argv, "--resume") {
		t.Fatalf("commands = %d, want the stage and one resumed run", len(fx.specs))
	}
	if got := argvFlag(t, fx.specs[0].Argv, "--max-turns"); got != "90" {
		t.Errorf("first run --max-turns = %s, want 90: the stage's 100 less the 10 kept for the report", got)
	}
	if got := argvFlag(t, fx.specs[1].Argv, "--max-turns"); got != "6" {
		t.Errorf("resumed run --max-turns = %s, want the 6 turns kept for the report", got)
	}
	first := strings.Join(strings.Fields(argvFlag(t, fx.specs[0].Argv, "-p")), " ")
	if !strings.Contains(first, "at most 90 agent turns") {
		t.Errorf("the evaluate prompt does not state the first run's 90 turns:\n%s", first)
	}
	repair := strings.Join(strings.Fields(argvFlag(t, fx.specs[1].Argv, "-p")), " ")
	for _, want := range []string{"reached its turn limit", "Write your report now", "/scratch/report.md"} {
		if !strings.Contains(repair, want) {
			t.Errorf("the resumed run's prompt lacks %q:\n%s", want, repair)
		}
	}
	if e.NumTurns != 90+2 {
		t.Errorf("num turns = %d, want both runs' 92", e.NumTurns)
	}
}

// TestEvaluateTurnCapReportAlreadyWritten: a run that wrote its report and
// then reached its cap (amending it, say) keeps that report, with no
// resumed run.
func TestEvaluateTurnCapReportAlreadyWritten(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := turnCapConfig(t, &out)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamMaxTurns, result: runner.Result{ExitCode: 1},
		writes: map[string]string{"scratch/report.md": goodEvaluation}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation || len(fx.specs) != 1 {
		t.Fatalf("evaluation = %+v after %d commands, want the written report and no resumed run", e,
			len(fx.specs))
	}
}

// TestEvaluateTurnCapUnrepaired: a capped run whose resumed session still
// writes no report ends as it did before (budget_exceeded, the cap named),
// after the bounded rounds, and no round runs past the stage's turns.
func TestEvaluateTurnCapUnrepaired(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := turnCapConfig(t, &out)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamMaxTurns, result: runner.Result{ExitCode: 1}},
		{ws: ws, stdout: streamResumed},
		{ws: ws, stdout: streamResumed},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeBudgetExceeded || e.ReportMarkdown != "" {
		t.Fatalf("evaluation = %+v, want budget_exceeded with no report", e)
	}
	if !strings.HasPrefix(e.Detail, "claude run error (error_max_turns)") ||
		!strings.Contains(e.Detail, "not repaired in 2 rounds") {
		t.Errorf("detail = %q, want the cap, then the rounds that did not repair it", e.Detail)
	}
	if len(fx.specs) != 3 {
		t.Fatalf("commands = %d, want the stage and two rounds", len(fx.specs))
	}
	if got := argvFlag(t, fx.specs[2].Argv, "--max-turns"); got != "6" {
		t.Errorf("second round --max-turns = %s, want a round's 6: 8 of the stage's turns are left", got)
	}
}

// TestEvaluateOtherLimitsNotResumed: a run the broker's spend limit, the
// wall clock or a crash ended is not resumed: the broker refuses the
// session any further turn, and the clock has no room for one.
func TestEvaluateOtherLimitsNotResumed(t *testing.T) {
	for name, s := range map[string]step{
		"the broker's limit": {stdout: streamBrokerLimit, result: runner.Result{ExitCode: 1}},
		"the wall clock":     {stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}},
		"a crash":            {stdout: streamExecError, result: runner.Result{ExitCode: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := turnCapConfig(t, &out)
			s.ws = ws
			fx := &fakeExec{steps: []step{s}}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome == envelope.OutcomeOK || len(fx.specs) != 1 {
				t.Errorf("evaluation = %+v after %d commands, want the run's failure and no resumed run", e,
					len(fx.specs))
			}
		})
	}
}

// TestReportReserve: the turns an evaluate stage keeps for its report are a
// tenth of its cap, at least every repair round's minimum and at most every
// round's maximum, never more than half the cap; a cap too small to keep
// one round's minimum keeps none.
func TestReportReserve(t *testing.T) {
	for _, tt := range []struct{ turns, want int }{
		{0, 0}, {1, 0}, {3, 0}, {5, 0}, {6, 3}, {10, 5}, {12, 6}, {25, 6}, {60, 6}, {80, 8}, {100, 10},
		{1000, 12},
	} {
		if got := reportReserve(tt.turns); got != tt.want {
			t.Errorf("reportReserve(%d) = %d, want %d", tt.turns, got, tt.want)
		}
	}
}

// TestReportReserveFitsEveryRound (S1 bug 5): whatever the cap, a reserve
// gives every repair round at least the turns to read the report, write it
// again and reply, the rounds after the first included.
func TestReportReserveFitsEveryRound(t *testing.T) {
	for turns := range 1001 {
		left := reportReserve(turns)
		if left == 0 {
			continue
		}
		for round := 1; round <= repairRounds && left >= repairMinTurns; round++ {
			got := repairTurns(left, round)
			if got < repairMinTurns || got > repairMaxTurns || got > left {
				t.Fatalf("cap %d, round %d with %d left: %d turns, want %d to %d and at most what is left", turns,
					round, left, got, repairMinTurns, repairMaxTurns)
			}
			left -= got // a round may spend every turn it is given
		}
		if r := reportReserve(turns); r >= repairRounds*repairMinTurns && left < 0 {
			t.Fatalf("cap %d: the rounds overspent the reserve %d", turns, r)
		}
	}
	for _, tt := range []struct{ left, round, want int }{
		{10, 1, 6}, {4, 2, 4}, {6, 1, 3}, {3, 2, 3}, {12, 1, 6}, {6, 2, 6}, {4, 1, 3},
	} {
		if got := repairTurns(tt.left, tt.round); got != tt.want {
			t.Errorf("repairTurns(%d, %d) = %d, want %d", tt.left, tt.round, got, tt.want)
		}
	}
}

// streamMaxTurnsOverReported is a capped run whose reported num_turns is
// past the cap it ran under, as a live run reported 99 under 94 (S1 bug 5).
var streamMaxTurnsOverReported = strings.Replace(streamMaxTurns, `"num_turns":90`, `"num_turns":99`, 1)

// TestEvaluateTurnCapCountsTheCap (S1 bug 5): the reserve is counted in the
// CLI's own turns: a first run held to 90 that reports 99 still leaves its
// repair the 10 kept turns, not 1.
func TestEvaluateTurnCapCountsTheCap(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := turnCapConfig(t, &out)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamMaxTurnsOverReported, result: runner.Result{ExitCode: 1}},
		{ws: ws, stdout: streamResumed, writes: map[string]string{"scratch/report.md": goodEvaluation}},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || len(fx.specs) != 2 {
		t.Fatalf("evaluation = %+v after %d commands, want the repaired report", e, len(fx.specs))
	}
	if got := argvFlag(t, fx.specs[1].Argv, "--max-turns"); got != "6" {
		t.Errorf("resumed run --max-turns = %s, want a round's 6 of the 10 kept", got)
	}
	if e.NumTurns != 99+2 {
		t.Errorf("num turns = %d, want the reported 101: accounting is unchanged", e.NumTurns)
	}
}
