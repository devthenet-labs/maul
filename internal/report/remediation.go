// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Reproduction bounds. Evidence is quoted, not argued over: an over-long
// value is cut to its bound, never refused (see Reproduction).
const (
	// ReproductionCommandMaxBytes bounds the test command.
	ReproductionCommandMaxBytes = 500
	// ReproductionExcerptMaxBytes bounds each of the before and after
	// excerpts.
	ReproductionExcerptMaxBytes = 2000
)

// Truncated ends a reproduction value patchy cut to its bound.
const Truncated = " [... cut by patchy]"

// Remediation is the parsed remediation report. Its frontmatter is strict:
//
//	---
//	success: true | false                # required
//	confidence: <0.0 to 1.0>             # required
//	reproduction:                        # optional; absent in older reports
//	  reproduced: true | false           # required in the block
//	  command: "<go test ./pkg -run X>"  # required when reproduced is true
//	  before: "<the failing output>"     # required when reproduced is true
//	  after: "<the passing output>"      # required when reproduced is true
//	---
type Remediation struct {
	// Success reports whether the agent believes the finding is fully
	// remediated. Pointer so absence is detectable; the runner verifies the
	// repository state regardless and may downgrade a claimed success.
	Success *bool `yaml:"success"`
	// Confidence is how confident the agent is that the issue is
	// remediated without breaking functionality.
	Confidence *float64 `yaml:"confidence"`
	// Reproduction is the agent's account of proving the bug with its
	// regression test before fixing it. Nil when the report has no
	// reproduction block: reports written before it existed stay valid, and
	// a reader tells "said nothing" from "said it could not".
	Reproduction *Reproduction `yaml:"reproduction"`
	// Body is the markdown summary following the frontmatter.
	Body string `yaml:"-"`
}

// Reproduction is what the remediation agent reports about proving the bug:
// it wrote the regression test first, ran it against the unfixed code
// (Before), applied the fix and ran it again (After). Every field is the
// agent's own claim — patchy runs no test itself — so it is shown to the
// reviewer as the agent's account, never as patchy's verification.
//
// The values are evidence quoted from tool output, which is routinely long,
// so a value over its bound is cut (ending in Truncated) rather than refused:
// a fix is not thrown away for the length of the output it quotes. The
// command is kept on one line.
type Reproduction struct {
	// Reproduced reports that the regression test failed on the unfixed
	// code for the expected reason and passes with the fix. Pointer so
	// absence is detectable.
	Reproduced *bool `yaml:"reproduced"`
	// Command is the command that ran the regression test.
	Command string `yaml:"command"`
	// Before is a short excerpt of the test's output on the unfixed code.
	Before string `yaml:"before"`
	// After is a short excerpt of the test's output with the fix applied.
	After string `yaml:"after"`
}

// ParseRemediation parses and validates a remediation report.
func ParseRemediation(data []byte) (*Remediation, error) {
	block, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	var r Remediation
	if err := decodeStrict(block, &r); err != nil {
		return nil, err
	}
	r.Body = body
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("report: remediation: %w", err)
	}
	return &r, nil
}

func (r *Remediation) validate() error {
	var errs []error
	if r.Success == nil {
		errs = append(errs, errors.New("success is required"))
	}
	if err := checkConfidence(r.Confidence); err != nil {
		errs = append(errs, err)
	}
	if r.Reproduction != nil {
		errs = append(errs, r.Reproduction.validate())
	}
	return errors.Join(errs...)
}

// validate checks the reproduction block and bounds its values in place.
func (p *Reproduction) validate() error {
	p.Command = Bound(strings.Join(strings.Fields(p.Command), " "), ReproductionCommandMaxBytes)
	p.Before = Bound(strings.TrimSpace(p.Before), ReproductionExcerptMaxBytes)
	p.After = Bound(strings.TrimSpace(p.After), ReproductionExcerptMaxBytes)
	if p.Reproduced == nil {
		return errors.New("reproduction: reproduced is required")
	}
	if !*p.Reproduced {
		return nil
	}
	var errs []error
	for _, f := range []struct{ name, value string }{
		{"command", p.Command}, {"before", p.Before}, {"after", p.After},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("reproduction: %s is required when reproduced is true", f.name))
		}
	}
	return errors.Join(errs...)
}

// Bound cuts s to at most limit bytes on a rune boundary, ending a cut value
// in Truncated; invalid UTF-8 is replaced first (the API server refuses it).
// It is idempotent, so a reader that bounds again (a controller re-bounding
// what an untrusted pod sent) changes nothing already in bound.
func Bound(s string, limit int) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	if len(s) <= limit {
		return s
	}
	cut := max(0, limit-len(Truncated))
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + Truncated
}
