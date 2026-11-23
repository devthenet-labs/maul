// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentresult

import (
	"errors"
	"fmt"
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/report"
)

const (
	// EvaluationMaxCited is the most ledger entries agent-runner may cite as
	// evidence: the screenshots the tools container emits.
	EvaluationMaxCited = 6
)

// Evaluation is an intent evaluate stage's result as the controller
// records it: the run's accounting and, when the stage ended ok, the
// evaluator's claims. Claims only (DESIGN 4.4): what ran, what it returned
// and which bytes are evidence come from the tools ledger, never from here.
type Evaluation struct {
	// Stage is the run's stage result, as every run records it.
	Stage *v1alpha1.StageResult
	// Report is the evaluation report exactly as the agent wrote it (at most
	// report.ReportMaxBytes, so it fits the run's status whole). Empty
	// unless the stage ended ok.
	Report string
	// Verdicts and Anomalies are the report's frontmatter, trimmed and
	// bounded by report.ParseEvaluation.
	Verdicts  []report.OutcomeVerdict
	Anomalies []report.Anomaly
	// Cited are the ledger seqs agent-runner asked the tools container to
	// emit as evidence: each one the report cites, none twice, at most
	// EvaluationMaxCited.
	Cited []int32
}

// FromEvaluation converts an evaluation event's payload. The claims are
// re-derived from the report, as a plan's are (FromPlan): the payload's
// parsed fields are ignored, so what is recorded can never disagree with
// the report a human reads. A stage that did not end ok converts to its
// Stage alone. An ok stage whose report does not parse, or whose cited
// seqs the report does not cite, is an error; the Stage is still returned,
// for the controller to record the run as report_invalid.
func FromEvaluation(p *envelope.Evaluation) (*Evaluation, error) {
	if p == nil {
		return nil, errors.New("agentresult: no evaluation payload")
	}
	out := &Evaluation{Stage: FromStage(&p.Stage)}
	if p.Outcome != envelope.OutcomeOK {
		return out, nil
	}
	parsed, err := report.ParseEvaluation([]byte(p.ReportMarkdown))
	if err != nil {
		return out, fmt.Errorf("agentresult: evaluation report: %w", err)
	}
	if err := checkCited(p.Cited, parsed.LedgerRefs()); err != nil {
		return out, fmt.Errorf("agentresult: evaluation: %w", err)
	}
	out.Report = p.ReportMarkdown
	out.Verdicts = parsed.Verdicts
	out.Anomalies = parsed.Anomalies
	out.Cited = slices.Clone(p.Cited)
	return out, nil
}

// checkCited holds the runner's cited seqs to the report: at most
// EvaluationMaxCited, none twice, each one the report cites (refs, sorted).
func checkCited(cited, refs []int32) error {
	if len(cited) > EvaluationMaxCited {
		return fmt.Errorf("cited has %d ledger seqs, over %d", len(cited), EvaluationMaxCited)
	}
	seen := make(map[int32]bool, len(cited))
	for i, seq := range cited {
		if seen[seq] {
			return fmt.Errorf("cited[%d] repeats %d", i, seq)
		}
		seen[seq] = true
		if _, found := slices.BinarySearch(refs, seq); !found {
			return fmt.Errorf("cited[%d] is %d, which the report does not cite", i, seq)
		}
	}
	return nil
}
