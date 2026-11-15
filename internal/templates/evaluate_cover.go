// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"errors"
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// CoverPrompt is the data for the message an evaluator is sent, in its own
// session, when the report maul accepted gives outcomes pass that no check
// it can count is bound to (gate 4, G4-B-LIB-1: a correct build held
// Unconfirmed because its evaluator rested eight passes on one exploring
// run). It names those outcomes and asks for one check each, or an unsure
// verdict, and nothing else may change in the report.
//
// Outcomes are ids, which the report parser has held to the outcome id
// grammar. Dropped and Refused are UNTRUSTED: each reason is derived from
// a file the agent wrote, so RenderCoverPrompt bounds it and fences it under
// a statement that it is data, as a repair's reason is quoted.
type CoverPrompt struct {
	// ReportPath is the evaluator's report; ChecksDir where it writes its
	// checks.
	ReportPath, ChecksDir string
	// Workspace is a round on a Project with no preview, whose checks run at
	// the head and at the base.
	Workspace bool
	// Outcomes are the ids of the outcomes the report passes with no check
	// maul can count, in the report's order: 1 to acceptance.MaxOutcomes.
	Outcomes []string
	// Dropped are the report's proposals for those outcomes that maul
	// dropped, with why.
	Dropped []CoverDropped
	// Refused is why maul refused the report the ask before this one
	// rewrote, "" for none: maul put back the report it kept.
	Refused string
	// Round is this ask's ordinal, of at most Rounds.
	Round, Rounds int
}

// CoverDropped is one proposal maul dropped: its id, its outcome, and why
// its file did not resolve to a check.
type CoverDropped struct {
	ID, Outcome, Reason string
}

// coverView is a CoverPrompt as its template reads it.
type coverView struct {
	CoverPrompt
	DroppedBlock string
}

// coverDroppedMax bounds the dropped proposals the prompt quotes, and
// coverReasonMaxBytes each reason.
const (
	coverDroppedMax     = 2 * acceptance.MaxOutcomes
	coverReasonMaxBytes = 300
)

// RenderCoverPrompt renders the cover prompt.
func RenderCoverPrompt(p CoverPrompt) (string, error) {
	if len(p.Outcomes) == 0 || len(p.Outcomes) > acceptance.MaxOutcomes {
		return "", fmt.Errorf("cover prompt: %d outcomes, want 1 to %d", len(p.Outcomes), acceptance.MaxOutcomes)
	}
	if p.ReportPath == "" || p.ChecksDir == "" || p.Round < 1 || p.Round > p.Rounds {
		return "", errors.New("cover prompt: the report path, the checks directory and the round are required")
	}
	var b strings.Builder
	for i, d := range p.Dropped {
		if i == coverDroppedMax {
			break
		}
		fmt.Fprintf(&b, "- proposal %s, of outcome %s, was dropped. Why, quoted: %s\n", quotedLine(d.ID, 80),
			quotedLine(d.Outcome, 64), quotedLine(plainText(d.Reason), coverReasonMaxBytes))
	}
	p.Refused = quotableError(p.Refused)
	return render("prompt_evaluate_cover.md.tmpl", coverView{CoverPrompt: p,
		DroppedBlock: strings.TrimSuffix(b.String(), "\n")})
}
