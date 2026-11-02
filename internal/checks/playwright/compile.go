// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
)

// The compiled spec's contract with the runner that writes it out (S1-12).
const (
	// FixtureModule is what a compiled spec imports test and expect from:
	// maul's fixture, never @playwright/test.
	FixtureModule = "./maul-fixture"
	// SupportModule is what a compiled spec imports maul's helpers from;
	// SupportFile, its file, holds Support().
	SupportModule = "./maul-checks"
	SupportFile   = "maul-checks.ts"
	// ActionTimeoutMS bounds every action and every retried judgement.
	ActionTimeoutMS = 5000
	// TestTimeoutMS bounds the whole check.
	TestTimeoutMS = 30000
	// PatientExpectTimeoutMS bounds every retried judgement and expectation
	// of a patient check (Options.Patient), PatientTestTimeoutMS the whole of
	// it.
	PatientExpectTimeoutMS = 15000
	PatientTestTimeoutMS   = 120000
	// AnnotationIndeterminate is the test annotation an unsure judgement
	// adds before it throws: the runner reports such a test as a check
	// error, never a fail.
	AnnotationIndeterminate = "maul-indeterminate"
	// MaxViewportSide bounds the viewport's width and height in px.
	MaxViewportSide = 4096
)

// Viewport is the fixed browser viewport a check runs at, recorded in the
// compiled spec: a layout defect may exist at one width only.
type Viewport struct {
	Width, Height int
}

// DefaultViewport is the viewport a check runs at unless Options names one.
var DefaultViewport = Viewport{Width: 1280, Height: 800}

// Options are the per-run inputs of a compiled check.
type Options struct {
	// RunToken replaces every {{run}}; required when the check uses it.
	RunToken string
	// Patient compiles a check run to show a defect (third review, F6):
	// every retried judgement and Playwright expectation waits
	// PatientExpectTimeoutMS, under PatientTestTimeoutMS, so a slow but
	// correct page is not read as the defect.
	Patient bool
	// Viewport is the fixed viewport; zero means DefaultViewport.
	Viewport Viewport
}

func (o Options) withDefaults() Options {
	if o.Viewport == (Viewport{}) {
		o.Viewport = DefaultViewport
	}
	return o
}

// runTokenPattern is a run token: maul's own (v<round>a<attempt>t<try>-<hex>),
// safe in a path, a selector and a text.
var runTokenPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// errPlaceholder marks text whose placeholders do not substitute.
var errPlaceholder = errors.New("placeholder")

// Compile compiles a browser check to a Playwright test (.spec.ts) a human
// can read and run. It compiles only a check the grammar accepts
// (checks.Validate; its refusal is returned as is) and that it can emit
// exactly: every role an ARIA role Playwright knows, no CSS locator that
// Playwright would read as a chain of selector engines. Agent text reaches
// the output only as JSON string literals.
//
// The spec imports test and expect from FixtureModule and maul's helpers
// from SupportModule; paths are resolved against the runner's baseURL (the
// preview). After every goto, click and key press it waits for the page
// at its current URL to load; a navigation started later by script (after
// a request) is the check's own waitForURL step. visible, legible and
// clickable run maul's page functions (pagefns.js); text, count use
// Playwright's retrying expectations.
func Compile(b checks.BrowserCheck, o Options) ([]byte, error) {
	o = o.withDefaults()
	v := o.Viewport
	if v.Width <= 0 || v.Height <= 0 || v.Width > MaxViewportSide || v.Height > MaxViewportSide {
		return nil, fmt.Errorf("viewport %dx%d is not within 1..%d px a side", v.Width, v.Height, MaxViewportSide)
	}
	c := checks.Check{ID: "compile", Outcome: "compile", Form: checks.FormBrowser, Browser: &b}
	if err := checks.Validate(c, checks.Preview{}); err != nil {
		return nil, err
	}
	if err := emittable(&b, o.RunToken); err != nil {
		return nil, err
	}
	return emit(b, o)
}

// emittable refuses what the grammar admits but Playwright would read
// otherwise, and a {{run}} with no valid token to put there.
func emittable(b *checks.BrowserCheck, token string) error {
	var err error
	usesRun := false
	walkLocators(b, func(l *checks.Locator) {
		switch {
		case err != nil:
		case l.Role != "" && !ariaRoles[l.Role]:
			err = fmt.Errorf("role %q is not an ARIA role Playwright locates by", l.Role)
		case strings.Contains(l.CSS, ">>"):
			err = fmt.Errorf("css %q chains selector engines (>>); write one CSS selector", l.CSS)
		}
	})
	walkStrings(b, func(s *string) {
		usesRun = usesRun || strings.Contains(*s, checks.RunPlaceholder)
	})
	if err == nil && usesRun && !runTokenPattern.MatchString(token) {
		err = fmt.Errorf("the check uses {{run}} and the run token %q is not one maul draws", token)
	}
	return err
}

// emit writes the spec of b without validating it: every string goes
// through lit.
func emit(b checks.BrowserCheck, o Options) ([]byte, error) {
	w := &writer{run: o.RunToken}
	if o.Patient {
		w.expectOpts = ", { timeout: " + strconv.Itoa(PatientExpectTimeoutMS) + " }"
	}
	v := o.Viewport
	size := "{ width: " + strconv.Itoa(v.Width) + ", height: " + strconv.Itoa(v.Height) + " }"
	usesExpect := false
	for _, st := range b.Steps {
		if a := st.Expect; a != nil && (a.Assert == checks.AssertText || a.Assert == checks.AssertCount) {
			usesExpect = true
		}
	}
	imports := "test"
	if usesExpect {
		imports = "test, expect"
	}
	w.line("// Compiled by maul from a saved browser check (internal/checks/playwright).")
	w.line("// Do not edit: maul runs only what it compiles. Agent text appears only in")
	w.line("// string literals. Viewport " + strconv.Itoa(v.Width) + "x" + strconv.Itoa(v.Height) +
		", action timeout " + strconv.Itoa(ActionTimeoutMS) + " ms.")
	w.line("import { " + imports + " } from " + strconv.Quote(FixtureModule) + ";")
	w.line("import { maul } from " + strconv.Quote(SupportModule) + ";")
	w.line("")
	w.line("test.use({ viewport: " + size + ", actionTimeout: " + strconv.Itoa(ActionTimeoutMS) + " });")
	w.line("")
	w.line(`test("maul check", async ({ page }) => {`)
	if o.Patient {
		w.line("  test.setTimeout(" + strconv.Itoa(PatientTestTimeoutMS) + ");")
		w.line("  maul.patience(" + strconv.Itoa(PatientExpectTimeoutMS) + ");")
	} else {
		w.line("  test.setTimeout(" + strconv.Itoa(TestTimeoutMS) + ");")
	}
	w.line("  await page.setViewportSize(" + size + ");")
	for i, st := range b.Steps {
		w.line("")
		w.step(i+1, st)
	}
	w.line("});")
	if w.err != nil {
		return nil, w.err
	}
	return []byte(w.b.String()), nil
}

// writer accumulates a spec and its first error.
type writer struct {
	b   strings.Builder
	run string
	err error
	// expectOpts follows a Playwright expectation's argument: a patient
	// check's timeout, or nothing.
	expectOpts string
}

func (w *writer) line(s string) {
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

// do writes one awaited statement inside the test.
func (w *writer) do(s string) { w.line("  await " + s + ";") }

// lit is s, {{run}} substituted, as a JSON string literal: the only way
// agent text enters a spec.
func (w *writer) lit(s string) string {
	sub, err := substitute(s, w.run)
	if err != nil && w.err == nil {
		w.err = err
	}
	out, err := json.Marshal(sub)
	if err != nil && w.err == nil {
		w.err = err
	}
	return string(out)
}

// substitute replaces {{run}} with run; any other placeholder is an error.
func substitute(s, run string) (string, error) {
	out, err := acceptance.Substitute(s, run, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", errPlaceholder, s, err)
	}
	return out, nil
}

func num(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "0"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func (w *writer) step(n int, st checks.BrowserStep) {
	label := "// step " + strconv.Itoa(n) + ": "
	switch {
	case st.Goto != "":
		w.line("  " + label + "goto")
		w.do("page.goto(" + w.lit(st.Goto) + ")")
		w.do("maul.settled(page)")
	case st.WaitForURL != "":
		w.line("  " + label + "waitForURL")
		w.do("maul.waitForPath(page, " + w.lit(st.WaitForURL) + ")")
	case st.Click != nil:
		w.line("  " + label + "click")
		w.do(w.locator(*st.Click) + ".click()")
		w.do("maul.settled(page)")
	case st.Fill != nil:
		w.line("  " + label + "fill")
		w.do(w.locator(st.Fill.Locator) + ".fill(" + w.lit(st.Fill.Text) + ")")
	case st.Select != nil:
		w.line("  " + label + "select")
		w.do("page.getByLabel(" + w.lit(st.Select.Label) + ").selectOption({ label: " + w.lit(st.Select.Option) +
			" })")
	case st.Press != nil:
		w.line("  " + label + "press")
		if st.Press.Locator != nil {
			w.do(w.locator(*st.Press.Locator) + ".press(" + w.lit(st.Press.Key) + ")")
		} else {
			w.do("page.keyboard.press(" + w.lit(st.Press.Key) + ")")
		}
		w.do("maul.settled(page)")
	case st.Scroll != nil:
		w.line("  " + label + "scroll")
		w.do("maul.scroll(" + w.locator(st.Scroll.Locator) + ", " + strconv.Itoa(st.Scroll.DX) + ", " +
			strconv.Itoa(st.Scroll.DY) + ")")
	case st.Expect != nil:
		w.line("  " + label + "expect " + string(assertName(st.Expect.Assert)))
		w.assertion(*st.Expect)
	default:
		if w.err == nil {
			w.err = fmt.Errorf("step %d is empty", n)
		}
	}
}

// assertName is a as written in a comment or an error: a known kind,
// never agent text.
func assertName(a checks.AssertKind) checks.AssertKind {
	switch a {
	case checks.AssertVisible, checks.AssertText, checks.AssertCount, checks.AssertURL, checks.AssertLeftOf,
		checks.AssertAbove, checks.AssertNoOverlap, checks.AssertEqualSize, checks.AssertAligned, checks.AssertSize,
		checks.AssertClickable, checks.AssertLegible:
		return a
	}
	return "unknown"
}

// helpers are the maul helper of each assertion on one or two elements.
var helpers = map[checks.AssertKind]string{
	checks.AssertVisible:   "visible",
	checks.AssertLegible:   "legible",
	checks.AssertClickable: "clickable",
	checks.AssertLeftOf:    "leftOf",
	checks.AssertAbove:     "above",
	checks.AssertNoOverlap: "noOverlap",
	checks.AssertEqualSize: "equalSize",
}

func (w *writer) assertion(a checks.Assertion) {
	loc := func(l *checks.Locator) string {
		if l == nil {
			if w.err == nil {
				w.err = fmt.Errorf("assert %s lacks a locator", assertName(a.Assert))
			}
			return "page"
		}
		return w.locator(*l)
	}
	tol := num(a.Tolerance)
	switch a.Assert {
	case checks.AssertVisible, checks.AssertLegible, checks.AssertClickable:
		w.do("maul." + helpers[a.Assert] + "(" + loc(a.Locator) + ")")
	case checks.AssertLeftOf, checks.AssertAbove, checks.AssertNoOverlap, checks.AssertEqualSize:
		w.do("maul." + helpers[a.Assert] + "(" + loc(a.Locator) + ", " + loc(a.Other) + ", " + tol + ")")
	case checks.AssertAligned:
		w.do("maul.aligned(" + loc(a.Locator) + ", " + loc(a.Other) + ", " + w.lit(a.Axis) + ", " + tol + ")")
	case checks.AssertSize:
		var dims []string
		if a.Width != nil {
			dims = append(dims, "width: "+num(*a.Width))
		}
		if a.Height != nil {
			dims = append(dims, "height: "+num(*a.Height))
		}
		w.do("maul.size(" + loc(a.Locator) + ", { " + strings.Join(dims, ", ") + " }, " + tol + ")")
	case checks.AssertURL:
		w.do("maul.expectPath(page, " + w.lit(a.URL) + ")")
	case checks.AssertCount:
		n := 0
		if a.Count != nil {
			n = *a.Count
		}
		w.do("expect(" + loc(a.Locator) + ").toHaveCount(" + strconv.Itoa(n) + w.expectOpts + ")")
	case checks.AssertText:
		w.text(loc(a.Locator), a.Text)
	default:
		if w.err == nil {
			w.err = errors.New("an assertion is not one maul compiles")
		}
	}
}

func (w *writer) text(loc string, t *checks.TextMatch) {
	switch {
	case t == nil:
		if w.err == nil {
			w.err = errors.New("assert text lacks its text")
		}
	case t.Equals != "":
		w.do("expect(" + loc + ").toHaveText(" + w.lit(t.Equals) + w.expectOpts + ")")
	case t.Contains != "":
		w.do("expect(" + loc + ").toContainText(" + w.lit(t.Contains) + w.expectOpts + ")")
	default:
		w.do("expect(" + loc + ").toHaveText(maul.regex(" + w.lit(t.Matches) + ")" + w.expectOpts + ")")
	}
}

// locator is l as a Playwright locator expression, its within parents
// first.
func (w *writer) locator(l checks.Locator) string {
	scope := "page"
	if l.Within != nil {
		scope = w.locator(*l.Within)
	}
	var e string
	switch {
	case l.Role != "":
		e = scope + ".getByRole(" + w.lit(l.Role)
		if l.Name != "" {
			e += ", { name: " + w.lit(l.Name) + " }"
		}
		e += ")"
	case l.Label != "":
		e = scope + ".getByLabel(" + w.lit(l.Label) + ")"
	case l.Text != "":
		e = scope + ".getByText(" + w.lit(l.Text) + ")"
	case l.TestID != "":
		e = scope + ".getByTestId(" + w.lit(l.TestID) + ")"
	default:
		// The css engine prefix keeps a selector from being read as any
		// other engine's (text=, xpath=, internal:...).
		e = scope + ".locator(" + w.lit("css="+l.CSS) + ")"
	}
	if l.HasText != "" {
		e += ".filter({ hasText: " + w.lit(l.HasText) + " })"
	}
	if l.Nth != nil {
		if *l.Nth == -1 {
			e += ".last()"
		} else {
			e += ".nth(" + strconv.Itoa(*l.Nth) + ")"
		}
	}
	return e
}

// walkLocators calls f on every locator of b, within parents included.
func walkLocators(b *checks.BrowserCheck, f func(*checks.Locator)) {
	var walk func(l *checks.Locator)
	walk = func(l *checks.Locator) {
		if l == nil {
			return
		}
		f(l)
		walk(l.Within)
	}
	for i := range b.Steps {
		st := &b.Steps[i]
		walk(st.Click)
		if st.Fill != nil {
			walk(&st.Fill.Locator)
		}
		if st.Press != nil {
			walk(st.Press.Locator)
		}
		if st.Scroll != nil {
			walk(&st.Scroll.Locator)
		}
		if st.Expect != nil {
			walk(st.Expect.Locator)
			walk(st.Expect.Other)
		}
	}
}

// walkStrings calls f on every non-empty string field of b, in place.
func walkStrings(b *checks.BrowserCheck, f func(*string)) {
	str := func(s *string) {
		if *s != "" {
			f(s)
		}
	}
	walkLocators(b, func(l *checks.Locator) {
		for _, s := range []*string{&l.Role, &l.Name, &l.Label, &l.Text, &l.TestID, &l.CSS, &l.HasText} {
			str(s)
		}
	})
	for i := range b.Steps {
		st := &b.Steps[i]
		str(&st.Goto)
		str(&st.WaitForURL)
		if st.Fill != nil {
			str(&st.Fill.Text)
		}
		if st.Select != nil {
			str(&st.Select.Label)
			str(&st.Select.Option)
		}
		if st.Press != nil {
			str(&st.Press.Key)
		}
		if a := st.Expect; a != nil {
			str(&a.URL)
			str(&a.Axis)
			if a.Text != nil {
				str(&a.Text.Equals)
				str(&a.Text.Contains)
				str(&a.Text.Matches)
			}
		}
	}
}

// ariaRoles are the roles Playwright's getByRole takes (its AriaRole type,
// @playwright/test 1.64.1).
var ariaRoles = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range strings.Fields(`alert alertdialog application article banner blockquote button caption cell
		checkbox code columnheader combobox complementary contentinfo definition deletion dialog directory document
		emphasis feed figure form generic grid gridcell group heading img insertion link list listbox listitem log
		main marquee math meter menu menubar menuitem menuitemcheckbox menuitemradio navigation none note option
		paragraph presentation progressbar radio radiogroup region row rowgroup rowheader scrollbar search searchbox
		separator slider spinbutton status strong subscript superscript switch tab table tablist tabpanel term
		textbox time timer toolbar tooltip tree treegrid treeitem`) {
		m[r] = true
	}
	return m
}()
