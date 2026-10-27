// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// Build report bounds, beyond the ones every intent report shares
// (SummaryMaxChars, ItemMaxChars, BodyMaxBytes, ReportMaxBytes).
const (
	// BuildMaxNotes is the most notes a build report may carry.
	BuildMaxNotes = 10
	// ReasonMaxChars bounds the reason a build could not be done.
	ReasonMaxChars = 1000
	// BuildMaxRefused is the most refused lines a build report may carry.
	BuildMaxRefused = 5
)

// ProxyMarker is what every line the egress proxy refuses a request with
// carries; a refused line must quote it. It is netprofile's, so the lines
// this report admits are the lines netprofile.PolicyRefusal reads.
const ProxyMarker = netprofile.RefusalMarker

// Build is the parsed build report: the intent build stage's contract, for
// the first build of an approved plan and for every revise round after it.
// Its frontmatter is strict:
//
//	---
//	success: true | false          # required: the plan is built and committed
//	no_defect: true | false        # optional; only with success false, on an
//	                               # acceptance-fix round: the code already
//	                               # does what the failing criteria ask, so
//	                               # nothing was changed (reason says why the
//	                               # verification failed)
//	summary: "<one line>"          # required, at most 200 characters
//	tests:                         # required
//	  ran: true | false            # required
//	  passed: true | false         # required; false when ran is false
//	  command: "<go test ./...>"   # required when ran is true
//	notes: []                      # at most 10 one-line items for reviewers;
//	                               # not held to the 500-character item bound
//	reason: "<one line>"           # required exactly when success is false
//	refused: []                    # optional, only when success is false: at
//	                               # most 5 lines, each the egress proxy's
//	                               # exact refusal of a dependency the plan
//	                               # needs ("403 egress-proxy: …")
//	---
//
// followed by the change's description in markdown — what changed and why,
// how it was verified — of at most BodyMaxBytes, in a document of at most
// ReportMaxBytes. maul records it on the build's run for people to read;
// the pull request's description is maul's own, rendered from the
// approved plan, never from this report. It holds the plan's visible-text
// rule: only visible characters, tabs and line breaks, in the free text and
// everywhere else (checkVisible), in a frontmatter of plain YAML
// (decodeFrontmatter) — nothing a reader of it is shown may render
// invisibly or reorder the text around it.
//
// It is not held to the plan's layout rule (checkLayout). No approver reads
// it in a code block, and the verification it reports is tool output that
// routinely passes the rule's bounds — pytest right-aligns its progress
// after a wide run of spaces, and `go tool cover -func` aligns its columns
// with tabs — so the rule would throw away a build that had implemented and
// committed its plan, for the layout of a description no approval rests on.
// A renderer that ever puts the report before someone deciding on the
// change neutralises its layout there, as maul's renderers already
// sanitise the agent text they show.
//
// Refused is the build's structured account of a dependency the operator's
// egress proxy refused (the network profile's tunnel hosts, its Go
// allowlist, or a registry grant): each item quotes the proxy's own line,
// so the controller can tell a refusal no retry gets past from a transient
// one (netprofile.PolicyRefusal) without reading the free-text reason. It
// is optional, so a report written before it existed stays valid.
//
// NoDefect is an acceptance-fix round's explicit conclusion that the code
// has no defect to fix: it replayed the failing criteria against this code
// and they hold, so it changed nothing. It is optional, so a report written
// before it existed stays valid and claims no such thing; it stands only
// with success false and a reason, and never beside refused. It is only the
// agent's claim: agent-runner refuses it outside an acceptance-fix round,
// and the controller honours it only on a round of trigger verify, where it
// ends the round without a retry and halts automatic fixes for a human.
// Without it, a build that changed nothing is a failure, as before.
//
// A report claiming success while its own tests failed contradicts itself
// and is refused. Success is still only the agent's claim: the runner
// downgrades it unless commit.sh leaves real commits, exactly as it does a
// remediation's.
type Build struct {
	// Success reports whether the agent built the approved plan. Pointer so
	// absence is detectable.
	Success *bool `yaml:"success"`
	// Summary is the change in one line.
	Summary string `yaml:"summary"`
	// Tests is what the agent ran to verify the change in this image.
	Tests *BuildTests `yaml:"tests"`
	// Notes are what a reviewer should look at closely.
	Notes []string `yaml:"notes"`
	// Reason says why the plan could not be built; set exactly when Success
	// is false.
	Reason string `yaml:"reason"`
	// Refused are the egress proxy's refusals of dependencies the plan
	// needs, each the proxy's exact line; set only when Success is false.
	Refused []string `yaml:"refused"`
	// NoDefect is an acceptance-fix round's conclusion that the failing
	// criteria already hold, so nothing was changed; set only when Success
	// is false.
	NoDefect bool `yaml:"no_defect"`
	// Body is the markdown description following the frontmatter.
	Body string `yaml:"-"`
}

// BuildTests is what a build ran to verify its change.
type BuildTests struct {
	// Ran reports whether any tests ran. Pointer so absence is detectable.
	Ran *bool `yaml:"ran"`
	// Passed reports whether every test that ran passed. Pointer so absence
	// is detectable.
	Passed *bool `yaml:"passed"`
	// Command is the command that ran them.
	Command string `yaml:"command"`
}

// buildFreeText are the build keys whose values are prose, and buildProse
// the top-level keys the free-text repair may touch (decodeRepairing).
var (
	buildFreeText = map[string]bool{"summary": true, "command": true, "reason": true}
	buildProse    = map[string]bool{"summary": true, "tests": true, "notes": true, "reason": true, "refused": true}
)

// ParseBuild parses and validates a build report: the document bound, that
// every byte of it is visible — refused, as a plan is, with the first
// offending character's code point, line and column — the body bound, and
// the frontmatter. Its layout is not bounded (see Build).
func ParseBuild(data []byte) (*Build, error) {
	if err := checkDocument("build", data); err != nil {
		return nil, err
	}
	block, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if err := checkBody("build", body); err != nil {
		return nil, err
	}
	var b Build
	if err := decodeRepairing(block, &b, buildFreeText, buildProse, func() { b = Build{} }); err != nil {
		return nil, err
	}
	b.Body = body
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("report: build: %w", err)
	}
	return &b, nil
}

// validate checks the frontmatter and trims its free text in place.
func (b *Build) validate() error {
	var errs []error
	var err error
	if b.Success == nil {
		errs = append(errs, errors.New("success is required"))
	}
	if b.Summary, err = oneLine("summary", b.Summary, SummaryMaxChars); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, b.validateTests())
	// A note is a pointer for reviewers, not something the build rests on, so
	// an over-long one is no reason to throw away a build that implemented
	// and committed its plan (overdub-10 lost one to a 574-character note).
	// It is accepted whole, not cut: maul records the report's raw bytes,
	// so a cut would reach no reader, and the whole note is still held to
	// every other rule of a one-line item, along its whole length. Each is
	// still capped, at the whole document's bound (ReportMaxBytes). The
	// plan's lists keep ItemMaxChars: a plan is approved as its exact bytes.
	errs = append(errs, lines("notes", b.Notes, BuildMaxNotes, ReportMaxBytes))
	if b.Success != nil {
		switch {
		case !*b.Success:
			if b.Reason, err = oneLine("reason", b.Reason, ReasonMaxChars); err != nil {
				errs = append(errs, fmt.Errorf("%w when success is false", err))
			}
		case b.Reason != "":
			errs = append(errs, errors.New("reason is set but success is true"))
		}
	}
	errs = append(errs, b.validateRefused(), b.validateNoDefect())
	return errors.Join(errs...)
}

// validateNoDefect checks no_defect against the claims it may not stand
// beside: a success, and a dependency the proxy refused. Its reason is
// success false's own (validate).
func (b *Build) validateNoDefect() error {
	switch {
	case !b.NoDefect:
		return nil
	case b.Success != nil && *b.Success:
		return errors.New("no_defect is set but success is true")
	case len(b.Refused) > 0:
		return errors.New("refused is set with no_defect: a build that found no defect needed no dependency")
	}
	return nil
}

// validateRefused checks the refused lines: one-line items, each quoting the
// egress proxy's line, only on a build that could not be done.
func (b *Build) validateRefused() error {
	if len(b.Refused) == 0 {
		return nil
	}
	if b.Success != nil && *b.Success {
		return errors.New("refused is set but success is true")
	}
	if err := lines("refused", b.Refused, BuildMaxRefused, ItemMaxChars); err != nil {
		return err
	}
	var errs []error
	for i, line := range b.Refused {
		if !strings.Contains(line, ProxyMarker) {
			errs = append(errs, fmt.Errorf("refused[%d] does not quote the egress proxy's line (%q): "+
				"copy the proxy's refusal exactly, or leave refused out", i, strings.TrimSpace(ProxyMarker)))
		}
	}
	return errors.Join(errs...)
}

// validateTests checks the tests block, including against the success claim.
func (b *Build) validateTests() error {
	t := b.Tests
	if t == nil {
		return errors.New("tests is required")
	}
	var errs []error
	if t.Ran == nil {
		errs = append(errs, errors.New("tests.ran is required"))
	}
	if t.Passed == nil {
		errs = append(errs, errors.New("tests.passed is required"))
	}
	if t.Ran == nil || t.Passed == nil {
		return errors.Join(errs...)
	}
	ran, passed := *t.Ran, *t.Passed
	var err error
	t.Command = strings.TrimSpace(t.Command)
	switch {
	case ran:
		if t.Command, err = oneLine("tests.command", t.Command, ItemMaxChars); err != nil {
			errs = append(errs, fmt.Errorf("%w when tests ran", err))
		}
	case passed:
		errs = append(errs, errors.New("tests.passed is true but tests.ran is false"))
	case t.Command != "":
		// A command that never ran is still named on one bounded line.
		if t.Command, err = oneLine("tests.command", t.Command, ItemMaxChars); err != nil {
			errs = append(errs, err)
		}
	}
	if b.Success != nil && *b.Success && ran && !passed {
		errs = append(errs, errors.New("success is true but the tests that ran did not pass"))
	}
	return errors.Join(errs...)
}
