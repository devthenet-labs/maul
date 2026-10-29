// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/runner"
)

// goodEvaluation is an evaluation report the agent writes under scratch.
const goodEvaluation = `---
verdicts:
  - id: rate-battle
    verdict: pass
    ledger: [12, 14]
    note: "The rating moved by 16 points."
  - id: leaderboard
    verdict: unsure
anomalies:
  - summary: "Console error on /battles"
    route: /battles
    ledger: [20]
---
Rated two battles.
`

// evaluateHandoff is an evaluator's handoff, as the controller stages it in
// input/issue.md; its zero-width space must reach the prompt escaped.
const evaluateHandoff = "Outcome rate-battle: a rated battle moves the rating.\u200b"

// roleWorkspace builds a role Job's /work as its prepare init leaves it: the
// handoff in input/issue.md and an empty scratch directory, no clone.
func roleWorkspace(t *testing.T, handoff string) string {
	t.Helper()
	ws := t.TempDir()
	for _, dir := range []string{"input", "scratch"} {
		if err := os.MkdirAll(filepath.Join(ws, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "input", "issue.md"), []byte(handoff), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// roleConfig is an evaluate or checkreview Job's configuration over a fresh
// role workspace holding handoff.
func roleConfig(t *testing.T, phase Phase, handoff string, out *bytes.Buffer) (Config, string) {
	t.Helper()
	ws := roleWorkspace(t, handoff)
	cfg := newConfig(t, ws, out)
	cfg.Phase = phase
	cfg.Finding = "target-1-ev-r1-a1"
	// A role stage has the wall clock a repair round needs (repairMinWindow).
	cfg.InvestigateTimeout = 30 * time.Minute
	return cfg, ws
}

func TestEvaluateEvent(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md": goodEvaluation,
	}}}}
	ev := onlyEvent(t, cfg, fx, &out)

	if ev.Type != envelope.TypeEvaluation || ev.Evaluation == nil {
		t.Fatalf("event = %+v, want an evaluation event", ev)
	}
	if ev.Repo != "acme/shop" || ev.Finding != "target-1-ev-r1-a1" {
		t.Errorf("event keyed %s/%s, want the run it belongs to", ev.Repo, ev.Finding)
	}
	e := ev.Evaluation
	if e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation {
		t.Fatalf("evaluation = %+v, want ok with the report verbatim", e)
	}
	if e.Harness != cfg.InvestigateHarness || e.Model != cfg.InvestigateModel {
		t.Errorf("stage ran on %s/%s, want the investigate stage's", e.Harness, e.Model)
	}
	if e.Usage.OutputTokens != 30 || e.NumTurns != 7 {
		t.Errorf("stage accounting = %+v, want the run's usage", e.Stage)
	}
	if len(e.Verdicts) != 2 || e.Verdicts[0].ID != "rate-battle" || e.Verdicts[0].Verdict != "pass" ||
		!slices.Equal(e.Verdicts[0].Ledger, []int32{12, 14}) || e.Verdicts[0].Note != "The rating moved by 16 points." {
		t.Errorf("verdicts = %+v, want the report's", e.Verdicts)
	}
	if len(e.Anomalies) != 1 || e.Anomalies[0].Route != "/battles" || !slices.Equal(e.Anomalies[0].Ledger, []int32{20}) {
		t.Errorf("anomalies = %+v, want the report's", e.Anomalies)
	}
	if len(e.Cited) != 0 {
		t.Errorf("cited = %v, want none: nothing asks the tools container for evidence yet", e.Cited)
	}
	checkEvaluateCommand(t, fx.specs[0], ws)
}

// checkEvaluateCommand: the evaluate stage runs in scratch, and its prompt
// names the report under scratch and carries the handoff escaped.
func checkEvaluateCommand(t *testing.T, spec runner.CommandSpec, ws string) {
	t.Helper()
	if spec.Dir != filepath.Join(ws, "scratch") {
		t.Errorf("stage ran in %s, want the scratch directory", spec.Dir)
	}
	prompt := argvFlag(t, spec.Argv, "-p")
	if !strings.Contains(prompt, filepath.Join(ws, "scratch", "report.md")) {
		t.Errorf("prompt does not name the report path under scratch:\n%s", prompt)
	}
	if !strings.Contains(prompt, "a rated battle moves the rating.<U+200B>") {
		t.Errorf("prompt does not carry the handoff escaped as untrusted data:\n%s", prompt)
	}
}

// TestEvaluateReportRefused: a report that does not parse, and is still
// refused after the repair rounds, ends the stage report_invalid with no
// claims.
func TestEvaluateReportRefused(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	bad := "---\nverdicts:\n  - id: rate-battle\n    verdict: pass\n---\n" // a pass with no ledger refs
	s := step{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": bad}}
	fx := &fakeExec{steps: []step{s, s, s}}
	ev := onlyEvent(t, cfg, fx, &out)
	e := ev.Evaluation
	if e == nil || e.Outcome != envelope.OutcomeReportInvalid || e.ReportMarkdown != "" || len(e.Verdicts) != 0 {
		t.Fatalf("evaluation = %+v, want report_invalid with no claims", e)
	}
	if len(fx.specs) != 1+repairRounds {
		t.Errorf("commands run = %d, want the stage and %d repair rounds", len(fx.specs), repairRounds)
	}
	for i, spec := range fx.specs {
		if spec.Dir != filepath.Join(ws, "scratch") {
			t.Errorf("command %d ran in %s, want the scratch directory", i, spec.Dir)
		}
	}
}

// TestRoleStagesNeedNoClone: a role Job's workspace has no repository
// clone, and that is no fatal; a missing handoff is.
func TestRoleStagesNeedNoClone(t *testing.T) {
	for _, phase := range []Phase{PhaseEvaluate, PhaseCheckReview} {
		t.Run(string(phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, phase, "", &out)
			if err := os.Remove(filepath.Join(ws, "input", "issue.md")); err != nil {
				t.Fatal(err)
			}
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "handoff missing") {
				t.Fatalf("Run() error = %v, want the missing handoff", err)
			}
			evs := events(t, out.String())
			if len(evs) != 1 || evs[0].Type != envelope.TypeFatal {
				t.Errorf("events = %+v, want one fatal", evs)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
		})
	}
}

// TestRoleStagesRefuseUnfitHarnesses: an evaluate or checkreview stage runs
// on brokered claude, brokered codex or the fake harness only, and never on
// one that cannot hold it to the evaluate posture.
func TestRoleStagesRefuseUnfitHarnesses(t *testing.T) {
	tests := []struct {
		name, harness, want string
	}{
		{"copilot", "copilot", "refused"},
		{"unbrokered claude", "claude", "must be brokered"},
	}
	for _, phase := range []Phase{PhaseEvaluate, PhaseCheckReview} {
		for _, tt := range tests {
			t.Run(string(phase)+"/"+tt.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, _ := roleConfig(t, phase, reviewHandoff(1), &out)
				cfg.InvestigateHarness = tt.harness
				fx := &fakeExec{}
				err := New(cfg, fx).Run(context.Background())
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Run() error = %v, want %q", err, tt.want)
				}
				if len(fx.specs) != 0 {
					t.Errorf("commands run = %d, want none", len(fx.specs))
				}
			})
		}
	}
}

// TestEvaluatePostureChecked: the evaluate posture is checked before a
// stage renders it, so a harness that cannot hold it is refused by name.
func TestEvaluatePostureChecked(t *testing.T) {
	h, _ := harness.ByID("copilot")
	if err := evaluatePosture(h); err == nil || !strings.Contains(err.Error(), "copilot") {
		t.Errorf("evaluatePosture(copilot) = %v, want a refusal naming it", err)
	}
	for _, id := range []string{"claude", "codex", "fake"} {
		h, _ := harness.ByID(id)
		if err := evaluatePosture(h); err != nil {
			t.Errorf("evaluatePosture(%s) = %v, want nil", id, err)
		}
	}
}

// TestDryEvaluateOnFakeHarness runs an evaluate stage end to end through the
// real runner on the fake harness: the fixture is replayed through cat, the
// report the fixture's agent "wrote" is settled, and the evaluation event
// carries the fixture's usage.
func TestDryEvaluateOnFakeHarness(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	fixture := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(fixture, []byte(streamSuccess+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(harness.FakeFixtureEnv, fixture)
	if err := os.WriteFile(filepath.Join(ws, "scratch", "report.md"), []byte(goodEvaluation), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New(cfg, &runner.Exec{}).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1:\n%s", len(evs), out.String())
	}
	ev := evs[0]
	e := ev.Evaluation
	if ev.Type != envelope.TypeEvaluation || e == nil || e.Outcome != envelope.OutcomeOK {
		t.Fatalf("event = %+v, want an ok evaluation", ev)
	}
	if e.Usage.InputTokens != 100 || e.Usage.OutputTokens != 30 || e.Usage.CostUSD == 0 || e.SessionID != testSessionID {
		t.Errorf("stage = %+v, want the fixture's usage and session", e.Stage)
	}
	if len(e.Verdicts) != 2 {
		t.Errorf("verdicts = %+v, want the report's two", e.Verdicts)
	}
}
