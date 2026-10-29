// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// Form is how a check runs.
type Form string

const (
	// FormHTTP checks are requests to a previewed component.
	FormHTTP Form = "http"
	// FormBrowser checks are browser steps maul compiles to Playwright.
	FormBrowser Form = "browser"
	// FormExec checks run a program in the slot's sandbox container.
	FormExec Form = "exec"
)

// Check is one saved check, bound to one approved outcome. Exactly one of
// HTTP, Browser and Exec is set, the one Form names.
type Check struct {
	// ID names the check: an outcome-style id with an optional
	// ".<name>" suffix, e.g. "winner-rises.order".
	ID string `json:"id"`
	// Outcome is the id of the outcome the check is bound to.
	Outcome string        `json:"outcome"`
	Form    Form          `json:"form"`
	HTTP    *HTTPCheck    `json:"http,omitempty"`
	Browser *BrowserCheck `json:"browser,omitempty"`
	Exec    *ExecCheck    `json:"exec,omitempty"`
}

// Parse decodes one check strictly: unknown fields and trailing data are
// refused.
func Parse(b []byte) (Check, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Check
	if err := dec.Decode(&c); err != nil {
		return Check{}, fmt.Errorf("check is not valid JSON of the grammar: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Check{}, errors.New("trailing data after the check")
	}
	return c, nil
}

// Canonical is the check's canonical JSON: normalised (every http default
// explicit, empty maps dropped), struct fields in declaration order, object
// keys sorted, expected values in canonical form. It is total and
// idempotent: Canonical of a Parse of Canonical(c) is Canonical(c).
func Canonical(c Check) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalize(c)); err != nil {
		// Unreachable: every field is a plain value or a canonical Value.
		return []byte("!unencodable")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// normalize returns a deep copy of c with its defaults made explicit.
func normalize(c Check) Check {
	if c.HTTP != nil {
		h := HTTPCheck{Target: c.HTTP.Target}
		if c.HTTP.Steps != nil {
			// acceptance.Normalize copies, defaults method and status, and
			// drops empty maps.
			h.Steps = acceptance.Normalize([]acceptance.Criterion{{Kind: acceptance.KindHTTP,
				Steps: c.HTTP.Steps}})[0].Steps
		}
		c.HTTP = &h
	}
	if c.Browser != nil {
		b := BrowserCheck{Steps: slices.Clone(c.Browser.Steps)}
		c.Browser = &b
	}
	if c.Exec != nil {
		e := *c.Exec
		e.Argv = slices.Clone(e.Argv)
		e.Files = slices.Clone(e.Files)
		if len(e.Expect.StdoutJSON) == 0 {
			e.Expect.StdoutJSON = nil
		}
		c.Exec = &e
	}
	return c
}

// Rule names what a refused check broke.
type Rule string

const (
	// RuleGrammar is any refusal of the grammar not named below.
	RuleGrammar Rule = "grammar"
	// RuleTooLarge: the check, or its program files, is over its bound.
	RuleTooLarge Rule = "too-large"
	// RuleTooManySteps: more than MaxSteps steps.
	RuleTooManySteps Rule = "too-many-steps"
	// RuleSnapshotRef: a snapshot ref (e21, f1e19, aria-ref=) anywhere.
	RuleSnapshotRef Rule = "snapshot-ref"
	// RuleColourLiteral: a colour value in an assertion.
	RuleColourLiteral Rule = "colour-literal"
	// RuleCSSDepth: a CSS locator of more than MaxCSSCompounds compounds.
	RuleCSSDepth Rule = "css-depth"
	// RuleNotSubstantive: no step acting on a surface, or no assertion
	// about content (DESIGN 4.3 rule 2).
	RuleNotSubstantive Rule = "not-substantive"
	// RuleRunToken: a list, row, count, $order or $includes assertion not
	// filtered by {{run}} text (DESIGN 4.3 rule 2).
	RuleRunToken Rule = "run-token"
	// RuleOffPreview: navigation, or a request target, off the preview.
	RuleOffPreview Rule = "off-preview"
)

// Refusal is a check the grammar refuses: the only error Validate returns.
type Refusal struct {
	// Check is the refused check's id, as written.
	Check string
	Rule  Rule
	// Reason says what is wrong, in maul's words.
	Reason string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("check %q refused (%s): %s", r.Check, r.Rule, r.Reason)
}

// refuse builds a refusal; Validate fills in the check's id.
func refuse(rule Rule, format string, args ...any) *Refusal {
	return &Refusal{Rule: rule, Reason: fmt.Sprintf(format, args...)}
}
