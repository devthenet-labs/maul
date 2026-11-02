// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"strings"
	"unicode"
)

// LintRule names what LintOutcomes flags in an outcome.
type LintRule string

const (
	// LintSharedData flags an outcome that quantifies over data a check does
	// not create ("every", "each", "others", "existing") or that only says
	// something "still works": the evaluator's checks run against a preview
	// whose data persists and is shared, so such an outcome is one no check
	// can show. "all" and "other" are left out: they name a check's own
	// items as often ("all 16 steps", "the other beat you create").
	LintSharedData LintRule = "shared-data"
	// LintNarrowedFrom flags an outcome that answers a request line saying
	// "each", "every" or "all" while the outcome itself speaks of fewer:
	// what the evaluator checks is the outcome, on items its checks create.
	LintNarrowedFrom LintRule = "narrowed-from"
)

// OutcomeFlag is one advisory flag on an outcome: its id, the rule, and the
// lint's own word that raised it (never the outcome's text).
type OutcomeFlag struct {
	Outcome string
	Rule    LintRule
	Word    string
}

// sharedDataWords are what LintSharedData looks for, in the order it
// reports the first: whole words, or a phrase of whole words.
var sharedDataWords = []string{"every", "each", "others", "existing", "still works"}

// narrowingWords are what LintNarrowedFrom looks for in a from line.
var narrowingWords = []string{"each", "every", "all"}

// LintOutcomes returns the advisory flags of os, in plan order: at most one
// of each rule per outcome, none for a manual outcome (a person judges it).
// It is advice for the approver only: it never refuses a plan and never
// feeds a verdict.
func LintOutcomes(os []PlanOutcome) []OutcomeFlag {
	var out []OutcomeFlag
	for _, o := range os {
		if o.Manual {
			continue
		}
		outcome := lintWords(o.Outcome)
		if w := firstWord(outcome, sharedDataWords); w != "" {
			out = append(out, OutcomeFlag{Outcome: o.ID, Rule: LintSharedData, Word: w})
		}
		if w := firstWord(lintWords(o.From), narrowingWords); w != "" && firstWord(outcome, narrowingWords) == "" {
			out = append(out, OutcomeFlag{Outcome: o.ID, Rule: LintNarrowedFrom, Word: w})
		}
	}
	return out
}

// lintWords is s as the lint reads it: its runs of letters and digits,
// lowercased, one space between each and one at either end.
func lintWords(s string) string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return " " + strings.Join(fields, " ") + " "
}

// hasWord reports whether words (lintWords) holds w as whole words.
func hasWord(words, w string) bool { return strings.Contains(words, " "+w+" ") }

// firstWord is the first of ws that words holds, or "".
func firstWord(words string, ws []string) string {
	for _, w := range ws {
		if hasWord(words, w) {
			return w
		}
	}
	return ""
}
