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
	// outcomeID is an outcome's id: the grammar of an acceptance
	// criterion's id (internal/acceptance's idPattern, which it does not
	// export), which slice 1's outcomes keep.
	outcomeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// Evaluation is the parsed evaluation report: the intent evaluate stage's
// contract. Its frontmatter is strict:
//
//	---
//	verdicts:                       # required, 1-20, one per outcome
//	  - outcome: "<outcome id>"     # or, as before, id (both: the same)
//	    verdict: pass | fail | unsure
//	    ledger: [<seq>, ...]        # required for pass and fail
//	    note: "<one line>"          # optional, at most 600 characters
//	anomalies:                      # at most 10
//	  - summary: "<one line>"          # required
//	    route: "<route or surface>"    # required, at most 300 characters
//	    ledger: [<seq>, ...]           # required: the evidence
//	    outcome: "<outcome id>"        # optional: an outcome it shows not holding
//	---
//
// followed by the agent's notes in markdown, of at most BodyMaxBytes, in a
// document of at most ReportMaxBytes. Ledger refs are seqs of the tools
// ledger, non-negative and none twice in one list, at most
// EvaluationMaxLedgerRefs in the whole report. The document is held to the
// plan's visible-text and layout rules (checkVisible, checkLayout) in a
// frontmatter of plain YAML (decodeFrontmatter).
//
// The keys an earlier release's evaluator wrote and the observed one
// retired (disputes, proposed, retire and a verdict's contrary) are unknown
// keys, so a report writing any of them, even as an empty list, is refused
// whole and claims nothing ("evaluator observed").
//
// Every value is the model's claim. That a seq exists and what it shows:
// the controller reads it from the round's verified ledger, never from
// here.
type Evaluation struct {
	Verdicts  []OutcomeVerdict `yaml:"verdicts"`
	Anomalies []Anomaly        `yaml:"anomalies"`
	// Body is the markdown following the frontmatter.
	Body string `yaml:"-"`
}

// OutcomeVerdict is the verdict on one outcome. The report names the
// outcome as outcome or, as before, id; once parsed, ID and Outcome both
// hold it.
type OutcomeVerdict struct {
	ID      string  `yaml:"id"`
	Outcome string  `yaml:"outcome"`
	Verdict Verdict `yaml:"verdict"`
	Ledger  []int32 `yaml:"ledger"`
	Note    string  `yaml:"note"`
}

// Anomaly is what the look-around found outside the outcomes.
type Anomaly struct {
	Summary string  `yaml:"summary"`
	Route   string  `yaml:"route"`
	Ledger  []int32 `yaml:"ledger"`
	// Outcome is the approved outcome the anomaly shows not holding, when
	// it shows one: maul then never passes that outcome on this report
	// (acceptance.ReasonContraryNote). Optional.
	Outcome string `yaml:"outcome,omitempty"`
	// PreExisting is the evaluator's claim that the anomaly was there
	// before the change, which an earlier prompt asked for. An anomaly is a
	// note that holds nothing, so maul no longer reads it; the key is still
	// accepted so a report that writes it is not refused whole.
	PreExisting bool `yaml:"pre_existing,omitempty"`
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
	for _, a := range e.Anomalies {
		refs = append(refs, a.Ledger...)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// validate checks the frontmatter and trims its free text in place.
func (e *Evaluation) validate() error {
	errs := e.validateVerdicts()
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
	for _, a := range e.Anomalies {
		n += len(a.Ledger)
	}
	return n
}

// validateVerdicts checks the verdicts.
func (e *Evaluation) validateVerdicts() []error {
	var errs []error
	switch {
	case len(e.Verdicts) == 0:
		return []error{errors.New("verdicts is required")}
	case len(e.Verdicts) > EvaluationMaxVerdicts:
		return []error{fmt.Errorf("verdicts has %d items, over %d", len(e.Verdicts), EvaluationMaxVerdicts)}
	}
	ids := make(map[string]bool, len(e.Verdicts))
	for i := range e.Verdicts {
		v := &e.Verdicts[i]
		field := fmt.Sprintf("verdicts[%d]", i)
		key := ".outcome" // the key the report wrote, for the errors
		if v.Outcome == "" && v.ID != "" {
			key = ".id"
		}
		errs = append(errs, verdictOutcome(field, v), uniqueID(field+key, v.ID, ids))
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
	return errs
}

// verdictOutcome settles which outcome verdict v names: its outcome, its
// id as before, or both when they agree. It leaves both fields holding it.
func verdictOutcome(field string, v *OutcomeVerdict) error {
	switch {
	case v.Outcome == "":
		v.Outcome = v.ID
	case v.ID == "":
		v.ID = v.Outcome
	case v.ID != v.Outcome:
		return fmt.Errorf("%s names its outcome twice, as id %q and outcome %q: name it once, as outcome", field,
			v.ID, v.Outcome)
	}
	return nil
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
		if a.Outcome != "" && !outcomeID.MatchString(a.Outcome) {
			errs = append(errs, fmt.Errorf("%s.outcome %q is not an outcome id", field, a.Outcome))
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
