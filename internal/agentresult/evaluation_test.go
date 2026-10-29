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

const (
	digestA = "sha256:aa00000000000000000000000000000000000000000000000000000000000000"
	digestB = "sha256:bb00000000000000000000000000000000000000000000000000000000000000"
)

const evaluationReport = `---
verdicts:
  - id: rate-battle
    verdict: pass
    ledger: [12, 14]
    note: "  Rating moved by 16.  "
  - id: leaderboard
    verdict: unsure
proposed:
  - id: own-rows
    outcome: leaderboard
    digest: "` + digestB + `"
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
	if len(got.Proposed) != 1 || got.Proposed[0].Digest != digestB || len(got.Anomalies) != 1 {
		t.Errorf("FromEvaluation() = %+v, want the report's proposals and anomalies", got)
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

func TestFromCheckReview(t *testing.T) {
	ev := &envelope.CheckReview{Stage: okStage(), Answers: []envelope.ReviewAnswer{
		{CheckDigest: digestA, OutcomeDigest: digestB, Answer: "proves", Reason: "  rates its own battle  "},
		{CheckDigest: digestB, OutcomeDigest: digestB, Answer: "unclear", Reason: "no rule"},
	}}
	got, err := FromCheckReview(ev)
	if err != nil {
		t.Fatalf("FromCheckReview() error = %v", err)
	}
	if len(got.Answers) != 2 || got.Answers[0] != (ReviewAnswer{CheckDigest: digestA, OutcomeDigest: digestB,
		Answer: report.AnswerProves, Reason: "rates its own battle"}) || got.Answers[1].Answer != report.AnswerUnclear {
		t.Errorf("Answers = %+v", got.Answers)
	}
	if got.Stage == nil || got.Stage.Outcome != "ok" {
		t.Errorf("Stage = %+v", got.Stage)
	}
}

func TestFromCheckReviewFailures(t *testing.T) {
	if _, err := FromCheckReview(nil); err == nil {
		t.Error("FromCheckReview(nil) error = nil, want an error")
	}
	got, err := FromCheckReview(&envelope.CheckReview{Stage: envelope.Stage{Outcome: envelope.OutcomeRuntimeError},
		Answers: []envelope.ReviewAnswer{{CheckDigest: digestA, OutcomeDigest: digestB, Answer: "proves", Reason: "r"}}})
	if err != nil || got.Stage.Outcome != "runtime_error" || got.Answers != nil {
		t.Errorf("FromCheckReview(runtime_error) = %+v, %v; want the stage alone", got, err)
	}

	valid := envelope.ReviewAnswer{CheckDigest: digestA, OutcomeDigest: digestB, Answer: "proves", Reason: "r"}
	many := make([]envelope.ReviewAnswer, CheckReviewMaxAnswers+1)
	for i := range many {
		many[i] = valid
		many[i].CheckDigest = "sha256:" + strings.Repeat("0", 62) + string("0123456789abcdef"[i/16]) +
			string("0123456789abcdef"[i%16])
	}
	for name, answers := range map[string][]envelope.ReviewAnswer{
		"none":        nil,
		"bad answer":  {{CheckDigest: digestA, OutcomeDigest: digestB, Answer: "yes", Reason: "r"}},
		"no reason":   {{CheckDigest: digestA, OutcomeDigest: digestB, Answer: "proves"}},
		"bad check":   {{CheckDigest: "abc", OutcomeDigest: digestB, Answer: "proves", Reason: "r"}},
		"bad outcome": {{CheckDigest: digestA, OutcomeDigest: "", Answer: "proves", Reason: "r"}},
		"hidden reason": {{CheckDigest: digestA, OutcomeDigest: digestB, Answer: "proves",
			Reason: "ok" + string(rune(0x202e))}},
		"answered twice": {valid, valid},
		"too many":       many,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := FromCheckReview(&envelope.CheckReview{Stage: okStage(), Answers: answers})
			if err == nil {
				t.Fatal("FromCheckReview() error = nil, want it refused")
			}
			if got == nil || got.Stage == nil || got.Answers != nil {
				t.Errorf("FromCheckReview() = %+v, want the stage alone", got)
			}
		})
	}
}
