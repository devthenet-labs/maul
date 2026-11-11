// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/maultool"
	"github.com/devthenet-labs/maul/internal/runner"
)

// clockConfig is an evaluate Job on brokered claude whose stage has the
// default 20 minutes of wall clock (gate D-B8).
func clockConfig(t *testing.T, out *bytes.Buffer) (Config, string) {
	t.Helper()
	cfg, ws := turnCapConfig(t, out)
	cfg.InvestigateTimeout = 20 * time.Minute
	return cfg, ws
}

// deadlineOf is the MAUL_TOOLS_DEADLINE a run's environment carries, the
// zero time for none.
func deadlineOf(t *testing.T, spec runner.CommandSpec) time.Time {
	t.Helper()
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, maultool.EnvDeadline+"="); ok {
			d, err := time.Parse(time.RFC3339, v)
			if err != nil {
				t.Fatalf("%s = %q: %v", maultool.EnvDeadline, v, err)
			}
			return d
		}
	}
	return time.Time{}
}

// TestEvaluateWallClockCollectsTheReport (gate D-B8): an evaluate run keeps
// the last minutes of its stage's wall clock for its report, as it keeps
// its last turns. A first run stopped by its own, shorter clock is not
// thrown away: its session is resumed, told its time ran out, and the
// report it then writes is the stage's. The prompt states the first run's
// clock, and every run carries its own cutoff for maul-tool to show.
func TestEvaluateWallClockCollectsTheReport(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := clockConfig(t, &out)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}},
		{ws: ws, stdout: streamResumed, writes: map[string]string{"scratch/report.md": goodEvaluation}},
	}}
	before := time.Now()
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation {
		t.Fatalf("evaluation = %+v, want the report the resumed session wrote", e)
	}
	if len(fx.specs) != 2 || !slices.Contains(fx.specs[1].Argv, "--resume") {
		t.Fatalf("commands = %d, want the stage and one resumed run", len(fx.specs))
	}
	if fx.timeouts[0] != 16*time.Minute {
		t.Errorf("first run's clock = %s, want 16m: the stage's 20m less the 4m kept for the report", fx.timeouts[0])
	}
	// The fake runs take no time, so the stage has all but nothing of its
	// 20m left: the round gets its own cap. Live, the first run spent its
	// 16m, and the round gets the 4m kept.
	if got := fx.timeouts[1]; got != repairTimeout {
		t.Errorf("resumed run's clock = %s, want a round's %s of what the stage has left", got, repairTimeout)
	}
	first := strings.Join(strings.Fields(argvFlag(t, fx.specs[0].Argv, "-p")), " ")
	if !strings.Contains(first, "and 16 minutes.") {
		t.Errorf("the evaluate prompt does not state the first run's 16 minutes:\n%s", first)
	}
	repair := strings.Join(strings.Fields(argvFlag(t, fx.specs[1].Argv, "-p")), " ")
	for _, want := range []string{"reached its wall clock limit", "Write your report now", "/scratch/report.md"} {
		if !strings.Contains(repair, want) {
			t.Errorf("the resumed run's prompt lacks %q:\n%s", want, repair)
		}
	}
	for i, want := range []time.Duration{16 * time.Minute, fx.timeouts[1]} {
		d := deadlineOf(t, fx.specs[i])
		if d.Before(before.Add(want).Add(-time.Second).Truncate(time.Second)) ||
			d.After(time.Now().Add(want).Add(time.Second)) {
			t.Errorf("run %d's %s = %s, want its own cutoff, %s from its start", i+1, maultool.EnvDeadline, d, want)
		}
	}
}

// TestEvaluateWallClockUnrepaired: a first run stopped by its clock whose
// resumed session still writes no report ends classified as the timeout
// it was, saying why no report came; never as a success.
func TestEvaluateWallClockUnrepaired(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := clockConfig(t, &out)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1, Elapsed: 16 * time.Minute}},
		{ws: ws, stdout: streamResumed},
		{ws: ws, stdout: streamResumed},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeTimeout || e.ReportMarkdown != "" {
		t.Fatalf("evaluation = %+v, want timeout with no report", e)
	}
	if !strings.HasPrefix(e.Detail, "stage timed out after 16m0s") || !strings.Contains(e.Detail, "not repaired") {
		t.Errorf("detail = %q, want the timeout, then why no report came", e.Detail)
	}
}

// TestEvaluateShortClockKeepsNone: a stage too short to keep a report
// window gives its first run the whole clock, and a run it stops is not
// resumed: no window is left.
func TestEvaluateShortClockKeepsNone(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := clockConfig(t, &out)
	cfg.InvestigateTimeout = 6 * time.Minute
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamCutOff, result: runner.Result{TimedOut: true, ExitCode: -1}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeTimeout || len(fx.specs) != 1 {
		t.Fatalf("evaluation = %+v after %d commands, want the timeout and no resumed run", e, len(fx.specs))
	}
	if fx.timeouts[0] != 6*time.Minute {
		t.Errorf("first run's clock = %s, want the whole 6m", fx.timeouts[0])
	}
}

// TestClockReserve: the wall clock an evaluate stage keeps for its report is
// a fifth of it, at least two repair windows and at most one repair round's
// clock, never more than half the stage; a stage too short keeps none.
func TestClockReserve(t *testing.T) {
	for _, tt := range []struct{ timeout, want time.Duration }{
		{0, 0}, {time.Minute, 0}, {6 * time.Minute, 0}, {8 * time.Minute, 4 * time.Minute},
		{20 * time.Minute, 4 * time.Minute}, {30 * time.Minute, 6 * time.Minute},
		{time.Hour, repairTimeout}, {2 * time.Hour, repairTimeout},
	} {
		if got := clockReserve(tt.timeout); got != tt.want {
			t.Errorf("clockReserve(%s) = %s, want %s", tt.timeout, got, tt.want)
		}
	}
}
