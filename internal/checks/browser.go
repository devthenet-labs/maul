// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// BrowserCheck is browser steps run in order in a fresh browser, compiled
// by maul to a Playwright test. Every navigation, and every click that
// navigates, is followed by a URL wait the compiler inserts.
type BrowserCheck struct {
	Steps []BrowserStep `json:"steps"`
}

// BrowserStep is exactly one action or one assertion.
type BrowserStep struct {
	// Goto is a path on the preview, resolved against it.
	Goto string `json:"goto,omitempty"`
	// WaitForURL is a path pattern ("*" any text) to wait for.
	WaitForURL string     `json:"waitForURL,omitempty"`
	Click      *Locator   `json:"click,omitempty"`
	Fill       *Fill      `json:"fill,omitempty"`
	Select     *Select    `json:"select,omitempty"`
	Press      *Press     `json:"press,omitempty"`
	Scroll     *Scroll    `json:"scroll,omitempty"`
	Expect     *Assertion `json:"expect,omitempty"`
}

// Locator finds an element, never by a snapshot ref: exactly one of Role
// (with an optional Name), Label, Text, TestID or CSS (last resort),
// optionally inside Within, filtered by HasText, and picked by Nth (-1 the
// last). Without Nth it must match exactly one element at run time.
type Locator struct {
	Role    string   `json:"role,omitempty"`
	Name    string   `json:"name,omitempty"`
	Label   string   `json:"label,omitempty"`
	Text    string   `json:"text,omitempty"`
	TestID  string   `json:"testid,omitempty"`
	CSS     string   `json:"css,omitempty"`
	Within  *Locator `json:"within,omitempty"`
	HasText string   `json:"hasText,omitempty"`
	Nth     *int     `json:"nth,omitempty"`
}

// Fill types Text into the control Locator finds.
type Fill struct {
	Locator Locator `json:"locator"`
	Text    string  `json:"text"`
}

// Select picks the option labelled Option in the control labelled Label.
type Select struct {
	Label  string `json:"label"`
	Option string `json:"option"`
}

// Press presses Key, on Locator when given, else on the page.
type Press struct {
	Locator *Locator `json:"locator,omitempty"`
	Key     string   `json:"key"`
}

// Scroll scrolls the element Locator finds by DX, DY px.
type Scroll struct {
	Locator Locator `json:"locator"`
	DX      int     `json:"dx"`
	DY      int     `json:"dy"`
}

// AssertKind is what an assertion checks.
type AssertKind string

// The assertions (DESIGN 2.7, SPEC Q9, Q10).
const (
	// AssertVisible: human visible (Q9): in the viewport after scrolling,
	// a non-zero box, effective opacity above 0.1 and, for an element with
	// its own text, contrast of at least 1.5:1 against its background.
	AssertVisible AssertKind = "visible"
	// AssertText: the element's text equals, contains or matches Text.
	AssertText AssertKind = "text"
	// AssertCount: Locator matches exactly Count elements.
	AssertCount AssertKind = "count"
	// AssertURL: the page's path matches URL.
	AssertURL AssertKind = "url"
	// AssertLeftOf: Locator's box ends left of Other's, within Tolerance.
	AssertLeftOf AssertKind = "left-of"
	// AssertAbove: Locator's box ends above Other's, within Tolerance.
	AssertAbove AssertKind = "above"
	// AssertNoOverlap: the two boxes do not overlap by more than Tolerance.
	AssertNoOverlap AssertKind = "no-overlap"
	// AssertEqualSize: the two boxes' sizes differ by at most Tolerance.
	AssertEqualSize AssertKind = "equal-size"
	// AssertAligned: the two boxes share an x (or y) within Tolerance.
	AssertAligned AssertKind = "aligned"
	// AssertSize: the box's Width and/or Height, within Tolerance.
	AssertSize AssertKind = "size"
	// AssertClickable: the element receives a click at its centre.
	AssertClickable AssertKind = "clickable"
	// AssertLegible: WCAG AA contrast of its text against its background
	// (4.5:1, 3:1 large); a background image or gradient is indeterminate.
	AssertLegible AssertKind = "legible"
)

// Assertion is one browser assertion; which fields it takes depends on
// Assert.
type Assertion struct {
	Assert  AssertKind `json:"assert"`
	Locator *Locator   `json:"locator,omitempty"`
	// Other is a box relation's second element.
	Other *Locator   `json:"other,omitempty"`
	Text  *TextMatch `json:"text,omitempty"`
	Count *int       `json:"count,omitempty"`
	// URL is a path pattern ("*" any text).
	URL string `json:"url,omitempty"`
	// Axis is aligned's: x or y.
	Axis   string   `json:"axis,omitempty"`
	Width  *float64 `json:"width,omitempty"`
	Height *float64 `json:"height,omitempty"`
	// Tolerance is a box assertion's slack in px.
	Tolerance float64 `json:"tolerance,omitempty"`
}

// TextMatch is exactly one of an exact text, a substring or a regular
// expression.
type TextMatch struct {
	Equals   string `json:"equals,omitempty"`
	Contains string `json:"contains,omitempty"`
	Matches  string `json:"matches,omitempty"`
}

// Bounds of the browser grammar.
const (
	// MaxWithin bounds a locator's chain of within parents.
	MaxWithin = 3
	// MaxNth bounds a locator's nth.
	MaxNth = 100
	// MaxScroll bounds a scroll in px.
	MaxScroll = 10000
	// MaxCount bounds a count assertion.
	MaxCount = 1000
	// MaxTolerance bounds a box assertion's tolerance in px.
	MaxTolerance = 100
	// MaxBox bounds a size assertion's width or height in px.
	MaxBox = 10000
)

var (
	rolePattern = regexp.MustCompile(`^[a-z]{2,24}$`)
	keyPattern  = regexp.MustCompile(`^((Control|Shift|Alt|Meta)\+){0,3}([A-Za-z0-9]|F([1-9]|1[0-2])|Enter|Tab|` +
		`Escape|Backspace|Delete|Space|ArrowUp|ArrowDown|ArrowLeft|ArrowRight|Home|End|PageUp|PageDown)$`)
	// listRoles are the roles of list entries: an assertion on one must be
	// filtered to run-token text.
	listRoles = map[string]bool{"row": true, "listitem": true, "treeitem": true, "option": true, "gridcell": true,
		"cell": true}
)

func validateBrowser(b BrowserCheck, p Preview) *Refusal {
	if len(b.Steps) == 0 {
		return refuse(RuleGrammar, "a browser check has at least one step")
	}
	acts, content := false, false
	for i, st := range b.Steps {
		r := validateBrowserStep(st, p)
		if r != nil {
			r.Reason = "step " + strconv.Itoa(i+1) + ": " + r.Reason
			return r
		}
		if st.Expect == nil {
			acts = acts || st.WaitForURL == ""
		} else {
			content = content || st.Expect.Assert != AssertURL
		}
	}
	if !acts {
		return refuse(RuleNotSubstantive, "no step acts on the page (goto, click, fill, select, press, scroll)")
	}
	if !content {
		return refuse(RuleNotSubstantive, "no assertion is about content; a url alone only says navigation worked")
	}
	for i, st := range b.Steps {
		if st.Expect == nil {
			continue
		}
		if r := assertionRunScoped(*st.Expect); r != nil {
			r.Reason = "step " + strconv.Itoa(i+1) + ": " + r.Reason
			return r
		}
	}
	return nil
}

func validateBrowserStep(st BrowserStep, p Preview) *Refusal {
	set := 0
	for _, on := range []bool{st.Goto != "", st.WaitForURL != "", st.Click != nil, st.Fill != nil, st.Select != nil,
		st.Press != nil, st.Scroll != nil, st.Expect != nil} {
		if on {
			set++
		}
	}
	if set != 1 {
		return refuse(RuleGrammar, "a step is exactly one of goto, waitForURL, click, fill, select, press, scroll, "+
			"expect")
	}
	switch {
	case st.Goto != "":
		return checkNavigation("goto", st.Goto, p)
	case st.WaitForURL != "":
		return checkNavigation("waitForURL", st.WaitForURL, p)
	case st.Click != nil:
		return validateLocator("click", *st.Click, 0)
	case st.Fill != nil:
		return firstRefusal(validateLocator("fill", st.Fill.Locator, 0), checkText("fill text", st.Fill.Text))
	case st.Select != nil:
		return validateSelect(*st.Select)
	case st.Press != nil:
		return validatePress(*st.Press)
	case st.Scroll != nil:
		return validateScroll(*st.Scroll)
	default:
		return validateAssertion(*st.Expect, p)
	}
}

func firstRefusal(rs ...*Refusal) *Refusal {
	for _, r := range rs {
		if r != nil {
			return r
		}
	}
	return nil
}

func validateSelect(s Select) *Refusal {
	if s.Label == "" || s.Option == "" {
		return refuse(RuleGrammar, "select takes the control's label and the option's label")
	}
	return firstRefusal(checkText("select label", s.Label), checkText("select option", s.Option))
}

func validatePress(p Press) *Refusal {
	if !keyPattern.MatchString(p.Key) {
		return refuse(RuleGrammar, "press key %q is not a key name (e.g. Enter, ArrowRight, Control+a)", p.Key)
	}
	if p.Locator != nil {
		return validateLocator("press", *p.Locator, 0)
	}
	return nil
}

func validateScroll(s Scroll) *Refusal {
	if s.DX == 0 && s.DY == 0 || abs(s.DX) > MaxScroll || abs(s.DY) > MaxScroll {
		return refuse(RuleGrammar, "scroll takes dx and dy within %d px, not both 0", MaxScroll)
	}
	return validateLocator("scroll", s.Locator, 0)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// validateLocator checks one locator and its within chain; depth is how
// many within parents lie below it.
func validateLocator(field string, l Locator, depth int) *Refusal {
	if depth > MaxWithin {
		return refuse(RuleGrammar, "%s: more than %d within parents", field, MaxWithin)
	}
	kinds := 0
	for _, s := range []string{l.Role, l.Label, l.Text, l.TestID, l.CSS} {
		if s != "" {
			kinds++
		}
	}
	switch {
	case kinds != 1:
		return refuse(RuleGrammar, "%s: a locator is exactly one of role, label, text, testid, css", field)
	case l.Name != "" && l.Role == "":
		return refuse(RuleGrammar, "%s: name goes with role", field)
	case l.Role != "" && !rolePattern.MatchString(l.Role):
		return refuse(RuleGrammar, "%s: role %q is not an ARIA role", field, l.Role)
	case l.Nth != nil && (*l.Nth < -1 || *l.Nth > MaxNth):
		return refuse(RuleGrammar, "%s: nth is -1 (the last) to %d", field, MaxNth)
	}
	for _, f := range [][2]string{{"name", l.Name}, {"label", l.Label}, {"text", l.Text}, {"testid", l.TestID},
		{"hasText", l.HasText}} {
		if r := checkText(field+" "+f[0], f[1]); r != nil {
			return r
		}
	}
	if l.CSS != "" {
		if r := checkCSS(field, l.CSS); r != nil {
			return r
		}
	}
	if l.Within != nil {
		return validateLocator(field+" within", *l.Within, depth+1)
	}
	return nil
}

// checkCSS holds a CSS locator to one selector (no list) of at most
// MaxCSSCompounds compound selectors.
func checkCSS(field, css string) *Refusal {
	if r := checkLine(field+" css", css); r != nil {
		return r
	}
	compounds, list, ok := cssCompounds(css)
	switch {
	case !ok:
		return refuse(RuleGrammar, "%s: css %q is not balanced", field, css)
	case list:
		return refuse(RuleGrammar, "%s: css %q is a selector list; write one selector", field, css)
	case compounds > MaxCSSCompounds:
		return refuse(RuleCSSDepth, "%s: css %q has %d compound selectors, over %d; locate by role, label, text or "+
			"testid", field, css, compounds, MaxCSSCompounds)
	}
	return nil
}

// cssCompounds counts css's compound selectors, separated by whitespace or
// a combinator (>, +, ~) outside brackets, parens and quotes, and reports
// a top-level selector list and whether the brackets and quotes balance.
func cssCompounds(css string) (compounds int, list, ok bool) {
	inCompound := false
	var n cssNesting
	for _, c := range css {
		separator := false
		if !n.step(c) {
			switch c {
			case ',':
				list, separator = true, true
			case ' ', '>', '+', '~':
				separator = true
			}
		}
		if separator {
			inCompound = false
		} else if !inCompound {
			compounds, inCompound = compounds+1, true
		}
	}
	return compounds, list, n.balanced()
}

// cssNesting tracks quotes, brackets and parens in a CSS selector.
type cssNesting struct {
	quote rune
	depth int
}

// step takes the next character and reports whether it is a quote, a
// bracket, a paren or inside one.
func (n *cssNesting) step(c rune) bool {
	switch {
	case n.quote != 0:
		if c == n.quote {
			n.quote = 0
		}
	case c == '"' || c == '\'':
		n.quote = c
	case c == '[' || c == '(':
		n.depth++
	case c == ']' || c == ')':
		n.depth--
	default:
		return n.depth > 0
	}
	return true
}

func (n cssNesting) balanced() bool { return n.quote == 0 && n.depth == 0 }

// assertion field use, per kind.
const (
	useLocator = 1 << iota
	useOther
	useText
	useCount
	useURL
	useAxis
	useSize
	useTolerance
)

var assertionUses = map[AssertKind]int{
	AssertVisible:   useLocator,
	AssertClickable: useLocator,
	AssertLegible:   useLocator,
	AssertText:      useLocator | useText,
	AssertCount:     useLocator | useCount,
	AssertURL:       useURL,
	AssertLeftOf:    useLocator | useOther | useTolerance,
	AssertAbove:     useLocator | useOther | useTolerance,
	AssertNoOverlap: useLocator | useOther | useTolerance,
	AssertEqualSize: useLocator | useOther | useTolerance,
	AssertAligned:   useLocator | useOther | useAxis | useTolerance,
	AssertSize:      useLocator | useSize | useTolerance,
}

func validateAssertion(a Assertion, p Preview) *Refusal {
	uses, known := assertionUses[a.Assert]
	if !known {
		return refuse(RuleGrammar, "assert %q is not one of visible, text, count, url, left-of, above, no-overlap, "+
			"equal-size, aligned, size, clickable, legible", a.Assert)
	}
	for _, f := range []struct {
		bit int
		set bool
	}{
		{useLocator, a.Locator != nil}, {useOther, a.Other != nil}, {useText, a.Text != nil},
		{useCount, a.Count != nil}, {useURL, a.URL != ""}, {useAxis, a.Axis != ""},
		{useSize, a.Width != nil || a.Height != nil}, {useTolerance, a.Tolerance != 0},
	} {
		if f.set != (uses&f.bit != 0) && (f.set || f.bit != useTolerance) {
			return refuse(RuleGrammar, "assert %s takes %s", a.Assert, describeUses(uses))
		}
	}
	if a.Locator != nil {
		if r := validateLocator(string(a.Assert), *a.Locator, 0); r != nil {
			return r
		}
	}
	if a.Other != nil {
		if r := validateLocator(string(a.Assert)+" other", *a.Other, 0); r != nil {
			return r
		}
	}
	return validateAssertionValues(a, p)
}

func validateAssertionValues(a Assertion, p Preview) *Refusal {
	switch {
	case a.Text != nil:
		return validateTextMatch(*a.Text)
	case a.Count != nil && (*a.Count < 0 || *a.Count > MaxCount):
		return refuse(RuleGrammar, "count is 0 to %d", MaxCount)
	case a.URL != "":
		return checkNavigation("url", a.URL, p)
	case a.Assert == AssertAligned && a.Axis != "x" && a.Axis != "y":
		return refuse(RuleGrammar, "aligned takes axis x or y")
	case a.Tolerance < 0 || a.Tolerance > MaxTolerance || math.IsNaN(a.Tolerance):
		return refuse(RuleGrammar, "tolerance is 0 to %d px", MaxTolerance)
	}
	for _, d := range []*float64{a.Width, a.Height} {
		if d != nil && (*d <= 0 || *d > MaxBox) {
			return refuse(RuleGrammar, "width and height are over 0 and at most %d px", MaxBox)
		}
	}
	return nil
}

func validateTextMatch(t TextMatch) *Refusal {
	set := 0
	for _, s := range []string{t.Equals, t.Contains, t.Matches} {
		if s != "" {
			set++
		}
	}
	if set != 1 {
		return refuse(RuleGrammar, "text takes exactly one of equals, contains, matches")
	}
	if r := checkText("text", t.Equals+t.Contains+t.Matches); r != nil {
		return r
	}
	if t.Matches != "" {
		if _, err := regexp.Compile(t.Matches); err != nil {
			return refuse(RuleGrammar, "text matches %q is not a regular expression: %v", t.Matches, err)
		}
	}
	return nil
}

func describeUses(uses int) string {
	var out []string
	for _, f := range []struct {
		bit  int
		name string
	}{
		{useLocator, "locator"}, {useOther, "other"}, {useText, "text"}, {useCount, "count"}, {useURL, "url"},
		{useAxis, "axis"}, {useSize, "width or height"}, {useTolerance, "tolerance (optional)"},
	} {
		if uses&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return strings.Join(out, ", ")
}

// assertionRunScoped refuses a count assertion, or an assertion on a list
// entry (a row, list item, option or cell), whose locator chain carries no
// {{run}} text: it would assert on data other runs created (DESIGN 4.3
// rule 2, the #44 stale-data false fail).
func assertionRunScoped(a Assertion) *Refusal {
	for _, l := range []*Locator{a.Locator, a.Other} {
		if l == nil || runFiltered(*l) {
			continue
		}
		if a.Assert == AssertCount {
			return refuse(RuleRunToken, "count must be over elements filtered by {{run}} text (name, text, label "+
				"or hasText, here or in a within parent)")
		}
		if listEntry(*l) {
			return refuse(RuleRunToken, "an assertion on a %s must filter it by {{run}} text (name, text, label "+
				"or hasText, here or in a within parent)", l.Role)
		}
	}
	return nil
}

func runFiltered(l Locator) bool {
	for p := &l; p != nil; p = p.Within {
		if hasRun(p.Name) || hasRun(p.Text) || hasRun(p.Label) || hasRun(p.HasText) {
			return true
		}
	}
	return false
}

func listEntry(l Locator) bool {
	for p := &l; p != nil; p = p.Within {
		if listRoles[p.Role] {
			return true
		}
	}
	return false
}
