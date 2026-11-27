// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/report"
)

const evaluationReport = `---
verdicts:
  - id: rate-battle
    verdict: pass
    ledger: [12, 14]
    note: "  Rating moved by 16.  "
  - id: leaderboard
    verdict: unsure
anomalies:
  - summary: "Console error"
    route: /battles
    ledger: [20]
---
## Notes
`

func okStage() envelope.Stage {
	return envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: "anthropic/claude-sonnet-5",
		NumTurns: 30, Usage: envelope.Usage{OutputTokens: 900, CostUSD: 0.5}}
}

func TestFromEvaluation(t *testing.T) {
	// The payload's parsed fields disagree with its report — a buggy or
	// hostile pod — and the report wins.
	ev := &envelope.Evaluation{
		Stage:          okStage(),
		ReportMarkdown: evaluationReport,
		Verdicts:       []envelope.OutcomeVerdict{{ID: "leaderboard", Verdict: "pass", Ledger: []int32{1}}},
		Cited:          []int32{14, 20},
	}
	got, err := FromEvaluation(ev)
	if err != nil {
		t.Fatalf("FromEvaluation() error = %v", err)
	}
	if got.Report != evaluationReport {
		t.Errorf("Report = %q, want the report byte-exact", got.Report)
	}
	if len(got.Verdicts) != 2 || got.Verdicts[0].Verdict != report.VerdictPass ||
		got.Verdicts[0].Note != "Rating moved by 16." || got.Verdicts[1].ID != "leaderboard" ||
		got.Verdicts[1].Verdict != report.VerdictUnsure {
		t.Errorf("Verdicts = %+v, want the report's", got.Verdicts)
	}
	if len(got.Anomalies) != 1 {
		t.Errorf("FromEvaluation() = %+v, want the report's anomalies", got)
	}
	if !slices.Equal(got.Cited, []int32{14, 20}) {
		t.Errorf("Cited = %v, want the runner's choice", got.Cited)
	}
	if got.Stage == nil || got.Stage.Outcome != "ok" || got.Stage.NumTurns != 30 || got.Stage.Usage.CostUSD != "0.500000" {
		t.Errorf("Stage = %+v, want the run's accounting", got.Stage)
	}
}

func TestFromEvaluationFailures(t *testing.T) {
	if _, err := FromEvaluation(nil); err == nil {
		t.Error("FromEvaluation(nil) error = nil, want an error")
	}
	got, err := FromEvaluation(&envelope.Evaluation{Stage: envelope.Stage{Outcome: envelope.OutcomeTimeout},
		ReportMarkdown: evaluationReport, Cited: []int32{14}})
	if err != nil || got.Stage.Outcome != "timeout" || got.Report != "" || got.Verdicts != nil || got.Cited != nil {
		t.Errorf("FromEvaluation(timeout) = %+v, %v; want the stage alone", got, err)
	}

	for name, tt := range map[string]struct {
		raw   string
		cited []int32
	}{
		"pass with no evidence": {strings.Replace(evaluationReport, "    ledger: [12, 14]\n", "", 1), nil},
		"no report":             {"", nil},
		"hidden character":      {evaluationReport + "pass" + string(rune(0x200b)) + "\n", nil},
		// The runner may only cite what the report cites.
		"cited outside the report": {evaluationReport, []int32{99}},
		"cited twice":              {evaluationReport, []int32{14, 14}},
		"cited too many":           {evaluationReport, []int32{12, 14, 20, 12, 14, 20, 12}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FromEvaluation(&envelope.Evaluation{Stage: okStage(), ReportMarkdown: tt.raw, Cited: tt.cited})
			if err == nil {
				t.Fatal("FromEvaluation() error = nil, want it refused")
			}
			if got == nil || got.Stage == nil || got.Report != "" || got.Verdicts != nil || got.Cited != nil {
				t.Errorf("FromEvaluation() = %+v, want the stage alone", got)
			}
		})
	}
}
