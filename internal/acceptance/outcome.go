// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// OutcomeKind says what an outcome expects of the change: that it makes
// something true (change, the default) or keeps something true (keep).
type OutcomeKind string

const (
	// OutcomeKindChange outcomes are what the change makes true: a check of one
	// fails at the base.
	OutcomeKindChange OutcomeKind = "change"
	// OutcomeKindKeep outcomes are what the change must leave true: a check of
	// one passes at both.
	OutcomeKindKeep OutcomeKind = "keep"
)

// The outcome grammar's bounds (DESIGN 3, the AcceptanceOutcome schema).
const (
	MaxOutcomes     = 20
	MaxOutcomeChars = 500
	MaxFromChars    = 300
)

// PlanOutcome is one acceptance outcome of an approved plan under the agent
// evaluator: what must hold, in plain words, and the issue line it answers.
// It carries no steps: the evaluator writes the checks. The JSON tags are
// the CRD form (v1alpha1.AcceptanceOutcome mirrors this type field for
// field); the YAML tags are the plan frontmatter's.
type PlanOutcome struct {
	// ID names the outcome: ^[a-z][a-z0-9-]{0,31}$, unique in the list.
	ID string `json:"id" yaml:"id"`
	// Outcome is what must hold, one line of at most MaxOutcomeChars
	// characters.
	Outcome string `json:"outcome" yaml:"outcome"`
	// From quotes the issue line the outcome answers, at most MaxFromChars
	// characters; NotCovered matches it against the issue's list items.
	From string `json:"from,omitempty" yaml:"from,omitempty"`
	// Kind is change (the default when empty) or keep.
	Kind OutcomeKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	// Manual outcomes are judged by a human and closed by `/maul accept`.
	Manual bool `json:"manual,omitempty" yaml:"manual,omitempty"`
	// Reason says why a manual outcome needs a human, at most
	// MaxReasonChars characters.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// OutcomeError is an outcome the grammar refuses: the only error
// ValidateOutcomes returns.
type OutcomeError struct {
	// ID is the refused outcome's id, as written.
	ID string
	// Reason says what is wrong, in maul's words.
	Reason string
}

func (e *OutcomeError) Error() string {
	return fmt.Sprintf("acceptance outcome %q: %s", e.ID, e.Reason)
}

// ValidateOutcomes checks os against the outcome grammar: at most
// MaxOutcomes outcomes with unique ids, each a required one-line outcome,
// an optional one-line from and reason within their bounds, and a kind of
// change, keep or none. A list-level refusal names the outcome that crossed
// the bound.
func ValidateOutcomes(os []PlanOutcome) error {
	if len(os) > MaxOutcomes {
		return &OutcomeError{ID: os[MaxOutcomes].ID, Reason: fmt.Sprintf("more than %d outcomes", MaxOutcomes)}
	}
	seen := make(map[string]bool, len(os))
	for _, o := range os {
		if seen[o.ID] {
			return &OutcomeError{ID: o.ID, Reason: "id is used twice"}
		}
		seen[o.ID] = true
		if err := validateOutcome(o); err != nil {
			return &OutcomeError{ID: o.ID, Reason: err.Error()}
		}
	}
	return nil
}

func validateOutcome(o PlanOutcome) error {
	if !idPattern.MatchString(o.ID) {
		return fmt.Errorf("id must match %s", idPattern)
	}
	if err := checkLine("outcome", o.Outcome, MaxOutcomeChars); err != nil {
		return err
	}
	if strings.TrimSpace(o.Outcome) == "" {
		return errors.New("outcome is required")
	}
	if err := checkLine("from", o.From, MaxFromChars); err != nil {
		return err
	}
	if err := checkLine("reason", o.Reason, MaxReasonChars); err != nil {
		return err
	}
	switch o.Kind {
	case "", OutcomeKindChange, OutcomeKindKeep:
		return nil
	default:
		return fmt.Errorf("kind %q is not one of change, keep", o.Kind)
	}
}

// NormalizeOutcomes returns a copy of os with every default explicit: an
// empty kind is change. nil stays nil.
func NormalizeOutcomes(os []PlanOutcome) []PlanOutcome {
	if os == nil {
		return nil
	}
	out := make([]PlanOutcome, len(os))
	for i, o := range os {
		if o.Kind == "" {
			o.Kind = OutcomeKindChange
		}
		out[i] = o
	}
	return out
}

// OutcomesDigest binds an outcome list as Digest binds criteria: "sha256:"
// and the hex sha256 of the canonical JSON of the FULL list (manual
// outcomes included, plan order kept), normalised first. An empty list
// digests "[]".
func OutcomesDigest(os []PlanOutcome) string {
	n := NormalizeOutcomes(os)
	if n == nil {
		n = []PlanOutcome{}
	}
	b, err := json.Marshal(n)
	if err != nil {
		// Unreachable: every field is a string or a bool.
		b = []byte("!unencodable")
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
