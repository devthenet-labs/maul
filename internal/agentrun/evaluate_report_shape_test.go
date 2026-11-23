// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// liveShapedReport is a report shaped exactly like the ones live evaluators
// wrote on 0.13.7 (S1 bug 5): each verdict naming its outcome as outcome,
// the check-era keys present, and a proposal naming the file it ran as file.
const liveShapedReport = "---\nverdicts:\n  - outcome: \"rate-battle\"\n    verdict: pass\n    ledger: [12, 14]\n" +
	"    note: \"The rating moved by 16 points.\"\n  - outcome: \"leaderboard\"\n    verdict: unsure\n" +
	"    ledger: []\ndisputes: []\nproposed:\n  - id: \"rate-battle.api\"\n    outcome: \"rate-battle\"\n" +
	"    file: \"checks/rate-battle-api.json\"\nretire: []\nanomalies: []\n---\nRated a battle.\n"

// TestEvaluateLiveShapedReport (S1 bug 5): the live report is accepted
// verbatim and its verdicts are the outcomes'. The proposal it still carries
// is the model's claim as written: no check file is read for it and nothing
// is confirmed, the stage's one tool call is its finish.
func TestEvaluateLiveShapedReport(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	tools := useTools(t, &cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/report.md": liveShapedReport,
	}}}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeOK || e.ReportMarkdown != liveShapedReport {
		t.Fatalf("evaluation = %+v, want the live report accepted verbatim", e)
	}
	if len(e.Verdicts) != 2 || e.Verdicts[0].ID != "rate-battle" || e.Verdicts[0].Verdict != "pass" ||
		e.Verdicts[1].ID != "leaderboard" {
		t.Errorf("verdicts = %+v, want the outcomes'", e.Verdicts)
	}
	if got := tools.verbs(); !slices.Equal(got, []toolwire.Verb{toolwire.VerbFinish}) {
		t.Errorf("tool calls = %v, want finish alone", got)
	}
}

// TestEvaluateRefusedReportNamesItsKeys (diagnosability): a refused
// report's detail names its frontmatter's key paths, never its values, so
// the next failure is visible without the transcript; and the repair
// message restates the exact frontmatter example and the decoder's lines.
func TestEvaluateRefusedReportNamesItsKeys(t *testing.T) {
	var out bytes.Buffer
	cfg, ws := roleConfig(t, PhaseEvaluate, evaluateHandoff, &out)
	brokeredClaude(t, &cfg)
	bad := "---\nverdicts:\n  - outcome: rate-battle\n    verdict: pass\n    evidence: [12]\n" +
		"    note: \"SECRET-NOTE\"\n---\nbody\n"
	s := step{ws: ws, stdout: streamSuccess, writes: map[string]string{"scratch/report.md": bad}}
	fx := &fakeExec{steps: []step{s, s, s}}
	e := onlyEvent(t, cfg, fx, &out).Evaluation
	if e == nil || e.Outcome != envelope.OutcomeReportInvalid {
		t.Fatalf("evaluation = %+v, want report_invalid", e)
	}
	for _, want := range []string{"field evidence not found",
		"frontmatter keys: verdicts, verdicts[].outcome, verdicts[].verdict, verdicts[].evidence, verdicts[].note"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail = %q, want %q", e.Detail, want)
		}
	}
	if strings.Contains(e.Detail, "SECRET-NOTE") {
		t.Errorf("detail %q carries a value", e.Detail)
	}
	if len(fx.specs) < 2 {
		t.Fatalf("commands = %d, want a repair", len(fx.specs))
	}
	repair := fx.specs[1].Stdin
	for _, want := range []string{strings.TrimSuffix(templates.ExampleEvaluationReport, "\n"),
		"line 5: field evidence not found"} {
		if !strings.Contains(repair, want) {
			t.Errorf("repair prompt lacks %q:\n%s", want, repair)
		}
	}
}
