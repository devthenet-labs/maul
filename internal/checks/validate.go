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

// Workspace placeholders (SPEC-S2 Q23): the tools container substitutes
// them in a workspace check's argv, args and program files, the same at
// head and at base, so one check text runs at both sides.
const (
	// BinPlaceholder is the cli surface's built binary, a path under
	// /work/bin; only a cli workspace has one.
	BinPlaceholder = "{{bin}}"
	// TreePlaceholder is the writable tree the build ran in, /work/tree.
	TreePlaceholder = "{{tree}}"
)

// Preview is what a check may reach: the preview's path prefixes (a goto
// must stay under one; none means "/") and its component keys (an http
// target must be one; none means unchecked).
type Preview struct {
	Paths      []string
	Components []string
}

// Workspace is a surface with no preview (SPEC-S2): the tool pod's built
// trees. Exactly one of its fields is set: Binary for a cli surface (exec
// of BinPlaceholder, or a run program invoking it), Language for a library
// surface (run only).
type Workspace struct {
	// Binary is the cli surface's binary path, as the Project declares it.
	Binary string
	// Language is the library surface's language (LanguageGo).
	Language string
}

// Context is the surface a check is validated for: exactly one of a
// preview slot and a workspace. A slot validates http, browser and exec
// checks as S1 did; a workspace refuses http and browser (RuleOffSurface)
// and admits exec and run checks of its surface.
type Context struct {
	Preview   *Preview
	Workspace *Workspace
}

var (
	// idPattern is an outcome's id, as acceptance criteria are named.
	idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// checkIDPattern is an outcome-style id and an optional ".<name>".
	checkIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}(\.[a-z0-9][a-z0-9-]{0,31})?$`)
)

// Validate holds c to the grammar and to DESIGN 4.3's static rules, on
// the surface ctx names. A refused check is a *Refusal naming the rule it
// broke; the checks run in a fixed order (context, shape, surface, size,
// steps, snapshot refs, colours, the form's grammar, substance, run-token
// scoping), so a check breaking several rules is refused for the first. A
// context naming no surface, or both, refuses every check (RuleGrammar).
func Validate(c Check, ctx Context) error {
	if r := validate(c, ctx); r != nil {
		r.Check = c.ID
		return r
	}
	return nil
}

func validate(c Check, ctx Context) *Refusal {
	if r := validateContext(ctx); r != nil {
		return r
	}
	if r := validateShape(c); r != nil {
		return r
	}
	ws := ctx.Workspace
	if r := onSurface(c.Form, ws); r != nil {
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
		return validateHTTP(*c.HTTP, *ctx.Preview)
	case FormBrowser:
		return validateBrowser(*c.Browser, *ctx.Preview)
	case FormRun:
		return validateRun(*c.Run, ws)
	default:
		return validateExec(*c.Exec, ws)
	}
}

// validateContext refuses a context naming no surface or both, and a
// workspace that is not exactly one of a cli and a library of a language
// maul builds programs in.
func validateContext(ctx Context) *Refusal {
	if (ctx.Preview == nil) == (ctx.Workspace == nil) {
		return refuse(RuleGrammar, "a check is validated for exactly one surface: a preview or a workspace")
	}
	ws := ctx.Workspace
	switch {
	case ws == nil:
		return nil
	case (ws.Binary == "") == (ws.Language == ""):
		return refuse(RuleGrammar, "a workspace is exactly one of a cli (its binary) and a library (its language)")
	case ws.Language != "" && !runLanguages[ws.Language]:
		return refuse(RuleGrammar, "library language %q is not one maul builds a program in: %s", ws.Language,
			LanguageGo)
	}
	return nil
}

// onSurface refuses a form the surface has no tool for: run in a preview
// slot; http and browser in a workspace; exec in a library, which has no
// binary.
func onSurface(f Form, ws *Workspace) *Refusal {
	switch {
	case ws == nil && f == FormRun:
		return refuse(RuleOffSurface, "a preview has no tree to build a program against; use http, browser or exec")
	case ws != nil && (f == FormHTTP || f == FormBrowser):
		return refuse(RuleOffSurface, "a workspace has no preview to send requests to or browse; use exec or run")
	case ws != nil && ws.Language != "" && f == FormExec:
		return refuse(RuleOffSurface, "a library has no binary to run; use a run check's consumer program")
	}
	return nil
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
	for _, set := range []bool{c.HTTP != nil, c.Browser != nil, c.Exec != nil, c.Run != nil} {
		if set {
			bodies++
		}
	}
	ok := bodies == 1 && (c.Form == FormHTTP && c.HTTP != nil || c.Form == FormBrowser && c.Browser != nil ||
		c.Form == FormExec && c.Exec != nil || c.Form == FormRun && c.Run != nil)
	if !ok {
		return refuse(RuleGrammar, "form must be one of http, browser, exec, run, with exactly that body")
	}
	return nil
}

// validateSize bounds a check's canonical JSON: MaxCanonicalBytes, plus
// MaxProgramBytes for an exec check's program files. A run check's program
// counts toward MaxCanonicalBytes with the rest of the check: the saved
// check bound stays (SPEC-S2 S2-04).
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
// included. An exec or run check's program files are left out: they run
// in the sandbox, never against the browser, and source text such as 1e5
// would read as a ref.
func findSnapshotRef(c Check) *Refusal {
	if c.Exec != nil {
		e := *c.Exec
		e.Files = nil
		c.Exec = &e
	}
	if c.Run != nil {
		r := *c.Run
		r.Files = nil
		c.Run = &r
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
// stdout (exec, run).
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
	case c.Run != nil:
		addValues(c.Run.Expect.StdoutJSON)
		out = append(out, c.Run.Expect.StdoutContains)
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

// checkArg holds an argument to checkText, admitting a workspace's
// placeholders in a workspace: TreePlaceholder in both, BinPlaceholder in
// a cli only. Outside a workspace either is an unknown placeholder.
func checkArg(field, s string, ws *Workspace) *Refusal {
	if ws != nil {
		if ws.Binary == "" && strings.Contains(s, BinPlaceholder) {
			return refuse(RuleGrammar, "%s: %s names a cli's binary; a library has none", field, BinPlaceholder)
		}
		s = strings.NewReplacer(BinPlaceholder, "bin", TreePlaceholder, "tree").Replace(s)
	}
	return checkText(field, s)
}

// checkProgramPlaceholders refuses BinPlaceholder in a library's program
// file, where nothing would substitute it.
func checkProgramPlaceholders(f File, ws *Workspace) *Refusal {
	if ws != nil && ws.Binary == "" && strings.Contains(f.Content, BinPlaceholder) {
		return refuse(RuleGrammar, "program file %q: %s names a cli's binary; a library has none", f.Path,
			BinPlaceholder)
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
