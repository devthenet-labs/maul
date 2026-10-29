// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
)

// ReviewAnswer is the check reviewer's answer on whether one check proves
// one outcome.
type ReviewAnswer string

// The answers. Only proves lets a check support a pass; the other two
// withhold it.
const (
	AnswerProves       ReviewAnswer = "proves"
	AnswerDoesNotProve ReviewAnswer = "does_not_prove"
	AnswerUnclear      ReviewAnswer = "unclear"
)

// CheckReview is the parsed check review report: what one fresh reviewer
// session writes about the one check it was shown. Its frontmatter is
// strict:
//
//	---
//	answer: proves | does_not_prove | unclear   # required
//	reason: "<one line>"                        # required, at most 600 characters
//	---
//
// followed by optional markdown of at most BodyMaxBytes, in a document of
// at most ReportMaxBytes, held to the plan's visible-text and layout rules.
// The session never names the check or the outcome: agent-runner keys the
// answer by the digests it was handed (envelope.ReviewAnswer).
type CheckReview struct {
	Answer ReviewAnswer `yaml:"answer"`
	Reason string       `yaml:"reason"`
	// Body is the markdown following the frontmatter.
	Body string `yaml:"-"`
}

// checkReviewFreeText are the keys the free-text repair may quote.
var checkReviewFreeText = map[string]bool{"reason": true}

// ParseCheckReview parses and validates a check review report: the document
// bound, that every byte of it is visible and in view, the body bound, and
// the frontmatter.
func ParseCheckReview(data []byte) (*CheckReview, error) {
	if err := checkDocument("checkreview", data); err != nil {
		return nil, err
	}
	if err := checkLayout("checkreview", data); err != nil {
		return nil, err
	}
	block, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if err := checkBody("checkreview", body); err != nil {
		return nil, err
	}
	var c CheckReview
	reset := func() { c = CheckReview{} }
	if err := decodeRepairing(block, &c, checkReviewFreeText, checkReviewFreeText, reset); err != nil {
		return nil, err
	}
	c.Body = body
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("report: checkreview: %w", err)
	}
	return &c, nil
}

// Validate checks the answer and its reason, trimming the reason in place.
// A controller holds an answer that reached it on the envelope to the same
// rules (agentresult.FromCheckReview).
func (c *CheckReview) Validate() error {
	var errs []error
	switch c.Answer {
	case AnswerProves, AnswerDoesNotProve, AnswerUnclear:
	default:
		errs = append(errs, fmt.Errorf("answer %q is not one of proves, does_not_prove, unclear", c.Answer))
	}
	var err error
	c.Reason, err = oneLine("reason", c.Reason, NoteMaxChars)
	errs = append(errs, err)
	return errors.Join(errs...)
}
