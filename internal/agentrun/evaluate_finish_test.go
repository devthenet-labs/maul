// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// checkEraEvaluation is goodEvaluation still carrying the check-era keys an
// earlier evaluator wrote: a proposal by a digest, and one by a file. The
// observed evaluator writes neither, and the report is refused for them.
var checkEraEvaluation = strings.Replace(goodEvaluation, "anomalies:", "proposed:\n"+
	"  - id: \"rate-battle.api\"\n    outcome: rate-battle\n    digest: \"sha256:"+strings.Repeat("ab", 32)+"\"\n"+
	"  - id: \"rate-battle.page\"\n    outcome: rate-battle\n    file: \"checks/missing.json\"\nanomalies:", 1)

// TestEvaluateFinishesWithoutConfirming: the stage explores, settles its
// report and finishes the tool pod, and that is all. A report still
// carrying the check-era keys is refused (report.ParseEvaluation), so it is
// repaired in the agent's own session like any refused report: repaired,
// the repaired report is the stage's; still carrying them after the
// repairs, the stage ends report_invalid naming the key. Either way
// agent-runner sends no confirm: its only call is the finish.
func TestEvaluateFinishesWithoutConfirming(t *testing.T) {
	for _, tt := range []struct {
		name    string
		repairs []string
		outcome envelope.Outcome
	}{
		{"repaired", []string{goodEvaluation}, envelope.OutcomeOK},
		{"never repaired", []string{checkEraEvaluation, checkEraEvaluation}, envelope.OutcomeReportInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
			brokeredClaude(t, &cfg)
			tools := useTools(t, &cfg)
			steps := []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": checkEraEvaluation}}}
			for _, r := range tt.repairs {
				steps = append(steps, step{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": r}})
			}
			fx := &fakeExec{steps: steps}
			e := onlyEvent(t, cfg, fx, &out).Evaluation
			if e == nil || e.Outcome != tt.outcome {
				t.Fatalf("evaluation = %+v, want %s", e, tt.outcome)
			}
			if len(fx.specs) != len(steps) || !strings.Contains(fx.specs[1].Stdin, "proposed") {
				t.Errorf("commands = %d, want the stage and %d repairs naming the retired key", len(fx.specs),
					len(tt.repairs))
			}
			switch tt.outcome {
			case envelope.OutcomeOK:
				if e.ReportMarkdown != goodEvaluation {
					t.Errorf("report = %q, want the repaired one", e.ReportMarkdown)
				}
			default:
				if e.ReportMarkdown != "" || !strings.Contains(e.Detail, "proposed") {
					t.Errorf("evaluation = %+v, want no report and the retired key named", e)
				}
			}
			if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
				t.Errorf("tool calls = %v, want finish alone: agent-runner confirms nothing", got)
			}
		})
	}
}

// TestEvaluateRunsNoCoverRound: a report that passes an outcome on what the
// evaluator saw, with no check, is the stage's report. The evaluator's
// session is not resumed to ask for a check of it.
func TestEvaluateRunsNoCoverRound(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	cfg.InvestigateMaxTurns = 100
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": goodEvaluation}},
		{ws: ws, stdout: streamResumed, writes: map[string]string{"scratch/report.md": checkEraEvaluation}},
	}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != goodEvaluation {
		t.Fatalf("evaluation = %+v, want the first run's report", e)
	}
	if len(fx.specs) != 1 {
		t.Errorf("commands = %d, want the stage's one run and no resumed ask", len(fx.specs))
	}
	if e.NumTurns != 7 {
		t.Errorf("num turns = %d, want the one run's 7", e.NumTurns)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want finish alone", got)
	}
}

// TestEvaluateEvidenceFavoursVerdictCites: the finish names at most
// toolwire.MaxEvidence screenshots, and the calls the verdicts cite come
// before the ones only an anomaly cites, since a verdict's cited calls are
// what the reviewers audit. The evidence is named in seq order.
func TestEvaluateEvidenceFavoursVerdictCites(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	report := strings.Replace(goodEvaluation, "ledger: [12, 14]", "ledger: [16, 10, 12, 11, 13, 15, 14]", 1)
	report = strings.Replace(report, "ledger: [20]", "ledger: [1, 2, 3]", 1)
	writes := map[string]string{"scratch/report.md": report}
	for n := 1; n <= 16; n++ {
		writes["scratch/shots/"+strconv.Itoa(n)+".png"] = pngBytes + strconv.Itoa(n)
	}
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: writes}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	want := []int32{10, 11, 12, 13, 14, 15}
	if !slices.Equal(e.Cited, want) {
		t.Errorf("cited = %v, want the verdict's first %d seqs %v", e.Cited, toolwire.MaxEvidence, want)
	}
	finish := tools.finish(t)
	if len(finish.Evidence) != toolwire.MaxEvidence {
		t.Fatalf("finish evidence = %+v, want %d", finish.Evidence, toolwire.MaxEvidence)
	}
	for i, ref := range finish.Evidence {
		if ref.Seq != int64(want[i]) || ref.SHA256 != sha256Hex(pngBytes+strconv.Itoa(int(want[i]))) {
			t.Errorf("finish evidence %d = %+v, want seq %d and its screenshot's sha256", i, ref, want[i])
		}
	}
}

// TestEvaluateEvidenceFillsFromAnomalies: with room left after the
// verdicts' screenshots, an anomaly's are named too, the whole in seq order.
func TestEvaluateEvidenceFillsFromAnomalies(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md":    goodEvaluation,
		"scratch/shots/20.png": pngBytes + "20",
		"scratch/shots/14.png": pngBytes + "14",
	}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if !slices.Equal(e.Cited, []int32{14, 20}) {
		t.Errorf("cited = %v, want [14 20]", e.Cited)
	}
	finish := tools.finish(t)
	if len(finish.Evidence) != 2 || finish.Evidence[0].Seq != 14 || finish.Evidence[1].Seq != 20 {
		t.Errorf("finish evidence = %+v, want seqs 14 then 20", finish.Evidence)
	}
}
