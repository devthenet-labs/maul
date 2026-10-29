// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Evaluation report bounds, beyond the ones every intent report shares
// (BodyMaxBytes, ReportMaxBytes).
const (
	// NoteMaxChars bounds every free-text value an evaluator or check
	// reviewer writes: a verdict's note, a reason, an anomaly's summary.
	NoteMaxChars = 600
	// RouteMaxChars bounds an anomaly's route.
	RouteMaxChars = 300
	// EvaluationMaxVerdicts is the most verdicts a report may carry: one
	// per approved outcome, and an Intent holds at most 20.
	EvaluationMaxVerdicts = 20
	// EvaluationMaxProposed is the most checks a report may propose: the
	// most a Preview verification carries.
	EvaluationMaxProposed = 40
	// EvaluationMaxDisputes and EvaluationMaxRetire bound the saved checks
	// a report may dispute or retire: at most every one of them.
	EvaluationMaxDisputes = 40
	EvaluationMaxRetire   = 40
	// EvaluationMaxAnomalies is the most anomalies the look-around may
	// report.
	EvaluationMaxAnomalies = 10
	// EvaluationMaxLedgerRefs bounds the ledger refs a report cites, in all
	// its lists together.
	EvaluationMaxLedgerRefs = 300
)

// Verdict is the evaluator's verdict on one outcome.
type Verdict string

// The verdicts. A pass or a fail must cite the ledger entries it rests on;
// with no evidence the only verdict is unsure (D11).
const (
	VerdictPass   Verdict = "pass"
	VerdictFail   Verdict = "fail"
	VerdictUnsure Verdict = "unsure"
)

var (
	// outcomeID is an outcome's and a proposed check's id: the grammar of
	// an acceptance criterion's id (internal/acceptance's idPattern, which
	// it does not export), which slice 1's outcomes keep.
	outcomeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// checkDigest is a saved check's, or an outcome's, digest.
	checkDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Evaluation is the parsed evaluation report: the intent evaluate stage's
// contract. Its frontmatter is strict:
//
//	---
//	verdicts:                       # required, 1-20, one per outcome
//	  - id: "<outcome id>"
//	    verdict: pass | fail | unsure
//	    ledger: [<seq>, ...]        # required for pass and fail
//	    note: "<one line>"          # optional, at most 600 characters
//	disputes:                       # at most 40
//	  - check_digest: "sha256:<hex>"   # the failing saved check
//	    reason: "<one line>"           # required
//	    replacement: "<proposed id>"   # optional; names an entry of proposed
//	    ledger: [<seq>, ...]           # required: the failure disputed
//	proposed:                       # at most 40; no bodies here
//	  - id: "<check id>"
//	    outcome: "<outcome id>"        # names an entry of verdicts
//	    digest: "sha256:<hex>"
//	retire:                         # at most 40
//	  - check_digest: "sha256:<hex>"   # never one disputed here too
//	    reason: "<one line>"           # required
//	anomalies:                      # at most 10
//	  - summary: "<one line>"          # required
//	    route: "<route or surface>"    # required, at most 300 characters
//	    ledger: [<seq>, ...]           # required: the evidence
//	---
//
// followed by the agent's notes in markdown, of at most BodyMaxBytes, in a
// document of at most ReportMaxBytes. Ledger refs are seqs of the tools
// ledger, non-negative and none twice in one list, at most
// EvaluationMaxLedgerRefs in the whole report. The document is held to the
// plan's visible-text and layout rules (checkVisible, checkLayout) in a
// frontmatter of plain YAML (decodeFrontmatter).
//
// Every value is the model's claim. That a seq exists, what it shows, that
// a proposed check was dry-run green and what its body is: the controller
// reads all of it from the ledger, never from here.
type Evaluation struct {
	Verdicts  []OutcomeVerdict `yaml:"verdicts"`
	Disputes  []Dispute        `yaml:"disputes"`
	Proposed  []ProposedCheck  `yaml:"proposed"`
	Retire    []Retire         `yaml:"retire"`
	Anomalies []Anomaly        `yaml:"anomalies"`
	// Body is the markdown following the frontmatter.
	Body string `yaml:"-"`
}

// OutcomeVerdict is the verdict on one outcome.
type OutcomeVerdict struct {
	ID      string  `yaml:"id"`
	Verdict Verdict `yaml:"verdict"`
	Ledger  []int32 `yaml:"ledger"`
	Note    string  `yaml:"note"`
}

// Dispute claims a failing saved check is not a defect.
type Dispute struct {
	CheckDigest string  `yaml:"check_digest"`
	Reason      string  `yaml:"reason"`
	Replacement string  `yaml:"replacement"`
	Ledger      []int32 `yaml:"ledger"`
}

// ProposedCheck names a check the agent asks maul to save.
type ProposedCheck struct {
	ID      string `yaml:"id"`
	Outcome string `yaml:"outcome"`
	Digest  string `yaml:"digest"`
}

// Retire asks maul to retire a saved check.
type Retire struct {
	CheckDigest string `yaml:"check_digest"`
	Reason      string `yaml:"reason"`
}

// Anomaly is what the look-around found outside the outcomes.
type Anomaly struct {
	Summary string  `yaml:"summary"`
	Route   string  `yaml:"route"`
	Ledger  []int32 `yaml:"ledger"`
}

// ParseEvaluation parses and validates an evaluation report: the document
// bound, that every byte of it is visible and in view, the body bound, and
// the frontmatter. Its frontmatter is not repaired: every list holds
// mappings, which the free-text repair would turn into strings.
func ParseEvaluation(data []byte) (*Evaluation, error) {
	if err := checkDocument("evaluation", data); err != nil {
		return nil, err
	}
	if err := checkLayout("evaluation", data); err != nil {
		return nil, err
	}
	block, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	if err := checkBody("evaluation", body); err != nil {
		return nil, err
	}
	var e Evaluation
	if err := decodeFrontmatter(block, &e); err != nil {
		return nil, err
	}
	e.Body = body
	if err := e.validate(); err != nil {
		return nil, fmt.Errorf("report: evaluation: %w", err)
	}
	return &e, nil
}

// LedgerRefs returns every ledger seq the report cites, sorted, each once.
func (e *Evaluation) LedgerRefs() []int32 {
	var refs []int32
	for _, v := range e.Verdicts {
		refs = append(refs, v.Ledger...)
	}
	for _, d := range e.Disputes {
		refs = append(refs, d.Ledger...)
	}
	for _, a := range e.Anomalies {
		refs = append(refs, a.Ledger...)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// validate checks the frontmatter and trims its free text in place.
func (e *Evaluation) validate() error {
	outcomes, errs := e.validateVerdicts()
	proposed, perrs := e.validateProposed(outcomes)
	errs = append(errs, perrs...)
	disputed, derrs := e.validateDisputes(proposed)
	errs = append(errs, derrs...)
	errs = append(errs, e.validateRetire(disputed)...)
	errs = append(errs, e.validateAnomalies()...)
	if n := e.refCount(); n > EvaluationMaxLedgerRefs {
		errs = append(errs, fmt.Errorf("the report cites %d ledger refs, over %d", n, EvaluationMaxLedgerRefs))
	}
	return errors.Join(errs...)
}

// refCount counts the ledger refs as written, across every list.
func (e *Evaluation) refCount() int {
	n := 0
	for _, v := range e.Verdicts {
		n += len(v.Ledger)
	}
	for _, d := range e.Disputes {
		n += len(d.Ledger)
	}
	for _, a := range e.Anomalies {
		n += len(a.Ledger)
	}
	return n
}

// validateVerdicts checks the verdicts and returns the outcome ids they
// name.
func (e *Evaluation) validateVerdicts() (map[string]bool, []error) {
	var errs []error
	switch {
	case len(e.Verdicts) == 0:
		return nil, []error{errors.New("verdicts is required")}
	case len(e.Verdicts) > EvaluationMaxVerdicts:
		return nil, []error{fmt.Errorf("verdicts has %d items, over %d", len(e.Verdicts), EvaluationMaxVerdicts)}
	}
	ids := make(map[string]bool, len(e.Verdicts))
	for i := range e.Verdicts {
		v := &e.Verdicts[i]
		field := fmt.Sprintf("verdicts[%d]", i)
		errs = append(errs, uniqueID(field+".id", v.ID, ids))
		switch v.Verdict {
		case VerdictPass, VerdictFail:
			if len(v.Ledger) == 0 {
				errs = append(errs, fmt.Errorf("%s is %s with no ledger refs: a verdict without evidence is unsure",
					field, v.Verdict))
			}
		case VerdictUnsure:
		default:
			errs = append(errs, fmt.Errorf("%s.verdict %q is not pass, fail or unsure", field, v.Verdict))
		}
		errs = append(errs, ledgerRefs(field+".ledger", v.Ledger))
		if v.Note != "" {
			var err error
			v.Note, err = oneLine(field+".note", v.Note, NoteMaxChars)
			errs = append(errs, err)
		}
	}
	return ids, errs
}

// validateProposed checks the proposed checks against the outcomes and
// returns their ids.
func (e *Evaluation) validateProposed(outcomes map[string]bool) (map[string]bool, []error) {
	if len(e.Proposed) > EvaluationMaxProposed {
		return nil, []error{fmt.Errorf("proposed has %d items, over %d", len(e.Proposed), EvaluationMaxProposed)}
	}
	var errs []error
	ids := make(map[string]bool, len(e.Proposed))
	digests := make(map[string]bool, len(e.Proposed))
	for i, p := range e.Proposed {
		field := fmt.Sprintf("proposed[%d]", i)
		errs = append(errs, uniqueID(field+".id", p.ID, ids))
		if outcomes != nil && !outcomes[p.Outcome] {
			errs = append(errs, fmt.Errorf("%s.outcome %q names no verdict's outcome", field, p.Outcome))
		}
		errs = append(errs, uniqueDigest(field+".digest", p.Digest, digests))
	}
	return ids, errs
}

// validateDisputes checks the disputes against the proposed checks and
// returns the digests they dispute.
func (e *Evaluation) validateDisputes(proposed map[string]bool) (map[string]bool, []error) {
	if len(e.Disputes) > EvaluationMaxDisputes {
		return nil, []error{fmt.Errorf("disputes has %d items, over %d", len(e.Disputes), EvaluationMaxDisputes)}
	}
	var errs []error
	digests := make(map[string]bool, len(e.Disputes))
	for i := range e.Disputes {
		d := &e.Disputes[i]
		field := fmt.Sprintf("disputes[%d]", i)
		errs = append(errs, uniqueDigest(field+".check_digest", d.CheckDigest, digests))
		var err error
		d.Reason, err = oneLine(field+".reason", d.Reason, NoteMaxChars)
		errs = append(errs, err)
		if d.Replacement != "" && !proposed[d.Replacement] {
			errs = append(errs, fmt.Errorf("%s.replacement %q names no proposed check", field, d.Replacement))
		}
		if len(d.Ledger) == 0 {
			errs = append(errs, fmt.Errorf("%s cites no ledger refs: a dispute names the failure it disputes", field))
		}
		errs = append(errs, ledgerRefs(field+".ledger", d.Ledger))
	}
	return digests, errs
}

// validateRetire checks the retirements; a check disputed in this report
// is retired through its dispute, never here too.
func (e *Evaluation) validateRetire(disputed map[string]bool) []error {
	if len(e.Retire) > EvaluationMaxRetire {
		return []error{fmt.Errorf("retire has %d items, over %d", len(e.Retire), EvaluationMaxRetire)}
	}
	var errs []error
	digests := make(map[string]bool, len(e.Retire))
	for i := range e.Retire {
		r := &e.Retire[i]
		field := fmt.Sprintf("retire[%d]", i)
		errs = append(errs, uniqueDigest(field+".check_digest", r.CheckDigest, digests))
		if disputed[r.CheckDigest] {
			errs = append(errs, fmt.Errorf("%s.check_digest is also disputed: a disputed check is retired "+
				"through its dispute", field))
		}
		var err error
		r.Reason, err = oneLine(field+".reason", r.Reason, NoteMaxChars)
		errs = append(errs, err)
	}
	return errs
}

// validateAnomalies checks the anomalies: each with its evidence.
func (e *Evaluation) validateAnomalies() []error {
	if len(e.Anomalies) > EvaluationMaxAnomalies {
		return []error{fmt.Errorf("anomalies has %d items, over %d", len(e.Anomalies), EvaluationMaxAnomalies)}
	}
	var errs []error
	for i := range e.Anomalies {
		a := &e.Anomalies[i]
		field := fmt.Sprintf("anomalies[%d]", i)
		var err error
		a.Summary, err = oneLine(field+".summary", a.Summary, NoteMaxChars)
		errs = append(errs, err)
		a.Route, err = oneLine(field+".route", a.Route, RouteMaxChars)
		errs = append(errs, err)
		if len(a.Ledger) == 0 {
			errs = append(errs, fmt.Errorf("%s cites no ledger refs: an anomaly needs its evidence", field))
		}
		errs = append(errs, ledgerRefs(field+".ledger", a.Ledger))
	}
	return errs
}

// uniqueID checks an id's grammar and that seen does not hold it yet, then
// records it.
func uniqueID(field, id string, seen map[string]bool) error {
	if !outcomeID.MatchString(id) {
		return fmt.Errorf("%s %q is not a lowercase id of at most 32 characters ([a-z][a-z0-9-]*)", field, id)
	}
	if seen[id] {
		return fmt.Errorf("%s %q is listed twice", field, id)
	}
	seen[id] = true
	return nil
}

// uniqueDigest checks a digest's shape and that seen does not hold it yet,
// then records it.
func uniqueDigest(field, digest string, seen map[string]bool) error {
	if !checkDigest.MatchString(digest) {
		return fmt.Errorf("%s %q is not a sha256:<64 hex> digest", field, digest)
	}
	if seen[digest] {
		return fmt.Errorf("%s is listed twice", field)
	}
	seen[digest] = true
	return nil
}

// ledgerRefs checks one list of ledger seqs: non-negative, none twice.
func ledgerRefs(field string, refs []int32) error {
	var errs []error
	seen := make(map[int32]bool, len(refs))
	for i, seq := range refs {
		switch {
		case seq < 0:
			errs = append(errs, fmt.Errorf("%s[%d] is %d, not a ledger seq", field, i, seq))
		case seen[seq]:
			errs = append(errs, fmt.Errorf("%s[%d] repeats %d", field, i, seq))
		}
		seen[seq] = true
	}
	return errors.Join(errs...)
}

// ValidDigest reports whether s has the shape of a saved check's or an
// outcome's digest, sha256:<64 lowercase hex>, as the reports name them.
func ValidDigest(s string) bool { return checkDigest.MatchString(s) }
