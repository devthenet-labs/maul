// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The grammar's bounds.
const (
	// MaxSteps bounds a check's steps (http requests or browser steps).
	MaxSteps = 30
	// MaxCanonicalBytes bounds an http or browser check's canonical JSON,
	// so it fits inline in a PreviewVerification (SPEC Q1).
	MaxCanonicalBytes = 8 << 10
	// MaxProgramBytes bounds an exec check's program files together.
	MaxProgramBytes = 64 << 10
	// MaxTextChars bounds one text field: a locator's text, a fill, an
	// expected text.
	MaxTextChars = 200
	// MaxCSSCompounds bounds a CSS locator's compound selectors.
	MaxCSSCompounds = 4
)

// RunPlaceholder is the run token's placeholder: a check names the data it
// creates with it, and filters every list assertion by it.
const RunPlaceholder = "{{run}}"

// Preview is what a check may reach: the preview's path prefixes (a goto
// must stay under one; none means "/") and its component keys (an http
// target must be one; none means unchecked).
type Preview struct {
	Paths      []string
	Components []string
}

var (
	// idPattern is an outcome's id, as acceptance criteria are named.
	idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// checkIDPattern is an outcome-style id and an optional ".<name>".
	checkIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}(\.[a-z0-9][a-z0-9-]{0,31})?$`)
)

// Validate holds c to the grammar and to DESIGN 4.3's static rules. A
// refused check is a *Refusal naming the rule it broke; the checks run in
// a fixed order (shape, size, steps, snapshot refs, colours, the form's
// grammar, substance, run-token scoping), so a check breaking several
// rules is refused for the first.
func Validate(c Check, p Preview) error {
	if r := validate(c, p); r != nil {
		r.Check = c.ID
		return r
	}
	return nil
}

func validate(c Check, p Preview) *Refusal {
	if r := validateShape(c); r != nil {
		return r
	}
	if r := validateSize(c); r != nil {
		return r
	}
	if n := stepCount(c); n > MaxSteps {
		return refuse(RuleTooManySteps, "%d steps, over %d", n, MaxSteps)
	}
	if r := findSnapshotRef(c); r != nil {
		return r
	}
	for _, s := range assertionTexts(c) {
		if colourLiteral(s) {
			return refuse(RuleColourLiteral, "the assertion text %q holds a colour value; assert on what a person "+
				"sees (visible, legible) instead of a colour", s)
		}
	}
	switch c.Form {
	case FormHTTP:
		return validateHTTP(*c.HTTP, p)
	case FormBrowser:
		return validateBrowser(*c.Browser, p)
	default:
		return validateExec(*c.Exec)
	}
}

// validateShape checks the id, the outcome and that exactly the body the
// form names is set.
func validateShape(c Check) *Refusal {
	switch {
	case !checkIDPattern.MatchString(c.ID):
		return refuse(RuleGrammar, `id must match ^[a-z][a-z0-9-]{0,31}(\.[a-z0-9][a-z0-9-]{0,31})?$`)
	case !idPattern.MatchString(c.Outcome):
		return refuse(RuleGrammar, `outcome must be an outcome id, ^[a-z][a-z0-9-]{0,31}$`)
	}
	bodies := 0
	for _, set := range []bool{c.HTTP != nil, c.Browser != nil, c.Exec != nil} {
		if set {
			bodies++
		}
	}
	ok := bodies == 1 && (c.Form == FormHTTP && c.HTTP != nil || c.Form == FormBrowser && c.Browser != nil ||
		c.Form == FormExec && c.Exec != nil)
	if !ok {
		return refuse(RuleGrammar, "form must be one of http, browser, exec, with exactly that body")
	}
	return nil
}

func validateSize(c Check) *Refusal {
	limit := MaxCanonicalBytes
	if c.Exec != nil {
		total := 0
		for _, f := range c.Exec.Files {
			total += len(f.Content)
		}
		if total > MaxProgramBytes {
			return refuse(RuleTooLarge, "program files are %d bytes, over %d", total, MaxProgramBytes)
		}
		limit += MaxProgramBytes
	}
	if n := len(Canonical(c)); n > limit {
		return refuse(RuleTooLarge, "the check is %d bytes of canonical JSON, over %d", n, limit)
	}
	return nil
}

func stepCount(c Check) int {
	switch {
	case c.HTTP != nil:
		return len(c.HTTP.Steps)
	case c.Browser != nil:
		return len(c.Browser.Steps)
	}
	return 0
}

// Snapshot refs.

var (
	// snapshotRef is a snapshot ref token: e21, or f1e19 inside a frame.
	snapshotRef = regexp.MustCompile(`^f?\d*e\d+$`)
	// tokenSplit separates the tokens a ref is looked for in.
	tokenSplit = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// findSnapshotRef refuses a snapshot ref in any string of the check, keys
// included. An exec check's program files are left out: they run in the
// sandbox, never against the browser, and source text such as 1e5 would
// read as a ref.
func findSnapshotRef(c Check) *Refusal {
	if c.Exec != nil {
		e := *c.Exec
		e.Files = nil
		c.Exec = &e
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(Canonical(c)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return refuse(RuleGrammar, "the check does not encode: %v", err)
	}
	var found string
	walkStrings(doc, func(s string) bool {
		if strings.Contains(strings.ToLower(s), "aria-ref=") {
			found = s
			return false
		}
		for _, tok := range tokenSplit.Split(s, -1) {
			if snapshotRef.MatchString(tok) {
				found = s
				return false
			}
		}
		return true
	})
	if found != "" {
		return refuse(RuleSnapshotRef, "%q holds a snapshot ref; refs change on every page and never belong in a "+
			"check: use a role and name, a label, text or a test id", found)
	}
	return nil
}

// walkStrings calls f on every string and object key of a decoded JSON
// value until f returns false.
func walkStrings(v any, f func(string) bool) bool {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for _, e := range t {
			if !walkStrings(e, f) {
				return false
			}
		}
	case map[string]any:
		for k, e := range t {
			if !f(k) || !walkStrings(e, f) {
				return false
			}
		}
	}
	return true
}

// Colour literals.

var (
	colourFunc = regexp.MustCompile(`(?i)\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\s*\(`)
	colourHex  = regexp.MustCompile(`(?i)(^|[^0-9a-z&])#([0-9a-f]{3,8})\b`)
)

// colourLiteral reports whether s holds a CSS colour: a colour function,
// or a hex colour of 3, 4, 6 or 8 digits with at least one letter (so
// "#123", an issue or a rank, stays text).
func colourLiteral(s string) bool {
	if colourFunc.MatchString(s) {
		return true
	}
	for _, m := range colourHex.FindAllStringSubmatch(s, -1) {
		hex := m[2]
		if n := len(hex); (n == 3 || n == 4 || n == 6 || n == 8) && strings.ContainsAny(strings.ToLower(hex), "abcdef") {
			return true
		}
	}
	return false
}

// assertionTexts lists every text a check asserts on: expected values,
// headers and body text (http), expected text and URLs (browser), expected
// stdout (exec).
func assertionTexts(c Check) []string {
	var out []string
	addValues := func(m map[string]acceptance.Value) {
		for _, v := range m {
			walkStrings(v.Decoded(), func(s string) bool { out = append(out, s); return true })
		}
	}
	switch {
	case c.HTTP != nil:
		for _, st := range c.HTTP.Steps {
			addValues(st.Expect.JSON)
			addValues(st.Expect.Headers)
			out = append(out, st.Expect.BodyContains)
		}
	case c.Browser != nil:
		for _, st := range c.Browser.Steps {
			if a := st.Expect; a != nil {
				out = append(out, a.URL)
				if a.Text != nil {
					out = append(out, a.Text.Equals, a.Text.Contains, a.Text.Matches)
				}
			}
		}
	case c.Exec != nil:
		addValues(c.Exec.Expect.StdoutJSON)
		out = append(out, c.Exec.Expect.StdoutContains)
	}
	return out
}

// Shared text rules.

// checkText holds a text field to checkLine and to one placeholder:
// {{run}}.
func checkText(field, s string) *Refusal {
	if r := checkLine(field, s); r != nil {
		return r
	}
	if _, err := acceptance.Substitute(s, "x", nil); err != nil {
		if errors.Is(err, acceptance.ErrCaptureMissing) {
			return refuse(RuleGrammar, "%s: captures exist only between http steps; use {{run}}", field)
		}
		return refuse(RuleGrammar, "%s: %v", field, err)
	}
	return nil
}

// checkLine holds a text field to one line of at most MaxTextChars
// characters, with no control or format character.
func checkLine(field, s string) *Refusal {
	if !utf8.ValidString(s) {
		return refuse(RuleGrammar, "%s is not UTF-8", field)
	}
	if n := utf8.RuneCountInString(s); n > MaxTextChars {
		return refuse(RuleGrammar, "%s is %d characters, over %d", field, n, MaxTextChars)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return refuse(RuleGrammar, "%s holds the control character %U", field, r)
		}
	}
	return nil
}

func hasRun(s string) bool { return strings.Contains(s, RunPlaceholder) }

// checkNavigation holds a goto, waitForURL or url pattern to a path under
// the preview: never a URL with a scheme or authority, never a dot
// segment, always under one of p.Paths. A "*" in a pattern stands for any
// text.
func checkNavigation(field, s string, p Preview) *Refusal {
	if r := checkText(field, s); r != nil {
		return r
	}
	sample := strings.ReplaceAll(strings.ReplaceAll(s, RunPlaceholder, "x"), "*", "x")
	if !strings.HasPrefix(sample, "/") || strings.HasPrefix(sample, "//") {
		return refuse(RuleOffPreview, "%s %q must be a path on the preview, starting with one \"/\"", field, s)
	}
	if err := acceptance.CheckPath(sample); err != nil {
		return refuse(RuleOffPreview, "%s %q: %v", field, s, err)
	}
	path, _, _ := strings.Cut(sample, "?")
	paths := p.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	if !slices.ContainsFunc(paths, func(prefix string) bool { return underPath(path, prefix) }) {
		return refuse(RuleOffPreview, "%s %q is outside the preview's paths %v", field, s, paths)
	}
	return nil
}

// underPath reports whether path is prefix or below it, on a segment
// boundary.
func underPath(path, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

// runScoped refuses an $order or $includes expectation looking for a
// value that carries no {{run}} text.
func runScoped(field string, m map[string]acceptance.Value) *Refusal {
	for ptr, v := range m {
		obj, ok := v.Decoded().(map[string]any)
		if !ok {
			continue
		}
		var items []any
		switch {
		case obj["$order"] != nil:
			items, _ = obj["$order"].([]any)
		case obj["$includes"] != nil:
			items = []any{obj["$includes"]}
		default:
			continue
		}
		for _, it := range items {
			if !walkStrings(it, func(s string) bool { return !hasRun(s) }) {
				continue
			}
			return refuse(RuleRunToken, "%s %q: every value an $order or $includes looks for must carry "+
				"{{run}} text, so the check asserts only on data its own run created", field, ptr)
		}
	}
	return nil
}

// contentJSON reports whether an expected-JSON map asserts on content:
// any expectation but the bare presence or absence of a whole document.
func contentJSON(m map[string]acceptance.Value) bool {
	for ptr, v := range m {
		if ptr != "" {
			return true
		}
		obj, ok := v.Decoded().(map[string]any)
		if !ok || (obj["$present"] == nil && obj["$absent"] == nil) {
			return true
		}
	}
	return false
}
