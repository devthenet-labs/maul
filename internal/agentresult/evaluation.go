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
	// CheckReviewMaxAnswers is the most answers one review round carries:
	// one per new check, and a round confirms at most 40.
	CheckReviewMaxAnswers = 40
)

// Evaluation is an intent evaluate stage's result as the controller
// records it: the run's accounting and, when the stage ended ok, the
// evaluator's claims. Claims only (DESIGN 4.4): which checks ran, what they
// observed and which bytes are evidence come from the tools ledger, never
// from here.
type Evaluation struct {
	// Stage is the run's stage result, as every run records it.
	Stage *v1alpha1.StageResult
	// Report is the evaluation report exactly as the agent wrote it (at most
	// report.ReportMaxBytes, so it fits the run's status whole). Empty
	// unless the stage ended ok.
	Report string
	// Verdicts, Disputes, Proposed, Retire and Anomalies are the report's
	// frontmatter, trimmed and bounded by report.ParseEvaluation.
	Verdicts  []report.OutcomeVerdict
	Disputes  []report.Dispute
	Proposed  []report.ProposedCheck
	Retire    []report.Retire
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
	out.Disputes = parsed.Disputes
	out.Proposed = parsed.Proposed
	out.Retire = parsed.Retire
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

// CheckReview is an intent checkreview stage's result as the controller
// records it: the run's accounting and, when the stage ended ok, the
// reviewer's answers.
type CheckReview struct {
	// Stage is the run's stage result, as every run records it.
	Stage *v1alpha1.StageResult
	// Answers are the reviewer's answers, held to report.CheckReview's
	// rules, one per (check, outcome) pair. Nil unless the stage ended ok.
	Answers []ReviewAnswer
}

// ReviewAnswer is one answer, keyed by the digests agent-runner was handed.
//
// Unreviewed marks agent-runner's own unclear for a review that ended
// without an accepted report (envelope.ReviewAnswer.Unreviewed).
type ReviewAnswer struct {
	CheckDigest   string
	OutcomeDigest string
	Answer        report.ReviewAnswer
	Reason        string
	Unreviewed    bool `json:",omitempty"`
}

// FromCheckReview converts a checkreview event's payload. There is no one
// report to re-derive the answers from (each comes from its own session),
// so each answer on the payload is held to the report's rules
// (report.CheckReview.Validate) and its digests to their shape. A stage
// that did not end ok converts to its Stage alone; an ok stage with no
// answers, or any answer refused, is an error, with the Stage returned.
func FromCheckReview(p *envelope.CheckReview) (*CheckReview, error) {
	if p == nil {
		return nil, errors.New("agentresult: no checkreview payload")
	}
	out := &CheckReview{Stage: FromStage(&p.Stage)}
	if p.Outcome != envelope.OutcomeOK {
		return out, nil
	}
	answers, err := reviewAnswers(p.Answers)
	if err != nil {
		return out, fmt.Errorf("agentresult: checkreview: %w", err)
	}
	out.Answers = answers
	return out, nil
}

// reviewAnswers validates and converts a round's answers.
func reviewAnswers(in []envelope.ReviewAnswer) ([]ReviewAnswer, error) {
	switch {
	case len(in) == 0:
		return nil, errors.New("no answers")
	case len(in) > CheckReviewMaxAnswers:
		return nil, fmt.Errorf("%d answers, over %d", len(in), CheckReviewMaxAnswers)
	}
	type pair struct{ check, outcome string }
	seen := make(map[pair]bool, len(in))
	out := make([]ReviewAnswer, 0, len(in))
	var errs []error
	for i, a := range in {
		if !report.ValidDigest(a.CheckDigest) || !report.ValidDigest(a.OutcomeDigest) {
			errs = append(errs, fmt.Errorf("answers[%d]: check and outcome digests must be sha256:<64 hex>", i))
			continue
		}
		key := pair{a.CheckDigest, a.OutcomeDigest}
		if seen[key] {
			errs = append(errs, fmt.Errorf("answers[%d] answers its check and outcome twice", i))
			continue
		}
		seen[key] = true
		c := report.CheckReview{Answer: report.ReviewAnswer(a.Answer), Reason: a.Reason}
		if err := c.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("answers[%d]: %w", i, err))
			continue
		}
		if a.Unreviewed && c.Answer != report.AnswerUnclear {
			errs = append(errs, fmt.Errorf("answers[%d] is unreviewed, so unclear, not %s", i, c.Answer))
			continue
		}
		out = append(out, ReviewAnswer{CheckDigest: a.CheckDigest, OutcomeDigest: a.OutcomeDigest,
			Answer: c.Answer, Reason: c.Reason, Unreviewed: a.Unreviewed})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}
