// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package previewtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devthenet-labs/maul/internal/checks/playwright"
)

// The e2e suite's scripted browser (SPEC S1-24). envtest runs no Chromium,
// so the e2e build's relay puts a browser in miniature behind the same
// BrowserCLI and TestRunner seams the image's Playwright CLI and test
// runner fill, and everything around them (the allowlist, navigation
// confinement, the preview front, snapshot inlining, artifact keeping,
// generate-locator, the compiled checks, the report judge, the ledger)
// runs as released.
//
// Every page it opens it fetches through the relay's own preview front, so
// the front's path routing reaches the component, and reads the answer as
// a ScriptedPage: elements with a role, a name, a test id, a text and a
// box, each optionally hidden, covered or illegible. The CLI answers the
// allowlisted verbs in the Playwright CLI's output format, writing its
// snapshots and screenshots (a PNG drawn from the boxes) where the CLI
// writes them. The runner reads a compiled check back statement by
// statement, measuring each against the page model with the support
// module's rules and words, and answers in the JSON reporter's shape. A
// statement it cannot read is a check error (the indeterminate
// annotation), never a pass. Never in a released binary.

// ScriptedPage is a page as the scripted browser renders it: the JSON a
// component answers in a page's place.
type ScriptedPage struct {
	Title    string            `json:"title,omitempty"`
	Elements []ScriptedElement `json:"elements,omitempty"`
}

// ScriptedElement is one element of a ScriptedPage, its box in CSS px at
// the fixed viewport.
type ScriptedElement struct {
	Role   string  `json:"role"`
	Name   string  `json:"name,omitempty"`
	TestID string  `json:"testid,omitempty"`
	Label  string  `json:"label,omitempty"`
	Text   string  `json:"text,omitempty"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	// Hidden: no box, not visible (display: none).
	Hidden bool `json:"hidden,omitempty"`
	// Covered: another element receives a click at its centre.
	Covered bool `json:"covered,omitempty"`
	// Illegible: its text's contrast is under WCAG AA.
	Illegible bool `json:"illegible,omitempty"`
}

// ScriptedBrowser is the scripted browser working under dir, and its
// preview front's loopback listener (a port of its own, so suites run in
// parallel).
func ScriptedBrowser(dir string) (*BrowserOptions, ArtifactKeeper, net.Listener, error) {
	session := filepath.Join(dir, "session")
	if err := os.MkdirAll(session, 0o700); err != nil {
		return nil, nil, nil, err
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	base := "http://" + front.Addr().String()
	f := scriptedFetcher{base: base, client: &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{Proxy: nil}}}
	return &BrowserOptions{
		CLI:        &scriptedCLI{dir: session, fetch: f},
		Dir:        session,
		Runner:     scriptedRunner{fetch: f},
		Base:       base,
		ConfigFile: filepath.Join(dir, "cli.json"),
	}, DirArtifacts{Dir: filepath.Join(dir, "artifacts")}, front, nil
}

// scriptedFetcher loads pages through the preview front.
type scriptedFetcher struct {
	base   string
	client *http.Client
}

// load fetches target (a URL on the front, or a path) and reads its page;
// an answer that is not a ScriptedPage is an empty page.
func (f scriptedFetcher) load(ctx context.Context, target string) (string, int, ScriptedPage, error) {
	u := target
	if strings.HasPrefix(target, "/") {
		u = f.base + target
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return u, 0, ScriptedPage{}, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		return u, 0, ScriptedPage{}, err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes))
	if err != nil {
		return u, res.StatusCode, ScriptedPage{}, err
	}
	var p ScriptedPage
	if json.Unmarshal(body, &p) != nil {
		p = ScriptedPage{}
	}
	return res.Request.URL.String(), res.StatusCode, p, nil
}

// ---- the CLI -----------------------------------------------------------

// scriptedCLI is the Playwright CLI's session in miniature.
type scriptedCLI struct {
	mu       sync.Mutex
	dir      string
	fetch    scriptedFetcher
	open     bool
	url      string
	page     ScriptedPage
	n        int
	requests []string
}

// Run implements BrowserCLI.
func (c *scriptedCLI) Run(ctx context.Context, args []string) (CLIResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(args) == 0 {
		return CLIResult{Output: "### Error\nError: no command\n", Exit: 1}, nil
	}
	verb, rest := args[0], args[1:]
	if verb == "open" {
		c.open, c.url, c.page = true, "about:blank", ScriptedPage{}
		return c.pageResult("### Browser `"+SessionName+"` opened with pid 1.\n", "await page.goto('about:blank');")
	}
	if !c.open {
		return CLIResult{Output: "The browser '" + SessionName + "' " + lostSession + ", please run open first\n",
			Exit: 1}, nil
	}
	switch verb {
	case "resize":
		return CLIResult{Output: code("await page.setViewportSize({ width: " + arg(rest, 0) + ", height: " +
			arg(rest, 1) + " });")}, nil
	case "goto":
		return c.navigate(ctx, arg(rest, 0), "await page.goto("+jsString(arg(rest, 0))+");")
	case "reload":
		return c.navigate(ctx, c.url, "await page.reload();")
	case "snapshot":
		return c.pageResult("", "")
	case "click", "hover", "fill", "select":
		i, res, ok := c.ref(arg(rest, 0))
		if !ok {
			return res, nil
		}
		return c.pageResult("", "await page."+locatorOf(c.page.Elements[i])+"."+verb+"();")
	case "type", "press":
		return c.pageResult("", "await page.keyboard."+verb+"("+jsString(arg(rest, 0))+");")
	case "screenshot":
		return c.screenshot()
	case "generate-locator":
		i, res, ok := c.ref(arg(rest, 0))
		if !ok {
			return res, nil
		}
		return CLIResult{Output: "### Result\n" + locatorOf(c.page.Elements[i]) + "\n"}, nil
	case "console":
		return CLIResult{Output: "### Result\nTotal messages: 0 (Errors: 0, Warnings: 0)\n"}, nil
	case "requests":
		return CLIResult{Output: "### Result\n" + strings.Join(c.requests, "\n") + "\n"}, nil
	case "eval":
		return CLIResult{Output: "### Error\nError: the scripted browser evaluates no script\n", Exit: 1}, nil
	}
	return CLIResult{Output: "### Error\nError: unknown command " + strconv.Quote(verb) + "\n", Exit: 1}, nil
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// navigate loads target as the page.
func (c *scriptedCLI) navigate(ctx context.Context, target, js string) (CLIResult, error) {
	u, status, p, err := c.fetch.load(ctx, target)
	if err != nil {
		return CLIResult{Output: "### Error\nError: page.goto: net::ERR_CONNECTION_REFUSED at " + target + "\n",
			Exit: 1}, nil
	}
	c.url, c.page = u, p
	c.requests = append(c.requests, fmt.Sprintf("[GET] %s => [%d]", u, status))
	return c.pageResult("", js)
}

// ref is the element a snapshot ref names, or the CLI's error.
func (c *scriptedCLI) ref(ref string) (int, CLIResult, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "e"))
	if err != nil || !strings.HasPrefix(ref, "e") || n < 2 || n-2 >= len(c.page.Elements) {
		return 0, CLIResult{Output: "### Error\nError: Ref " + ref + " not found in the current page snapshot. " +
			"Try capturing new snapshot.\n", Exit: 1}, false
	}
	return n - 2, CLIResult{}, true
}

// pageResult is an action's answer: the code it ran, the page and a link
// to its snapshot file, as the CLI writes them.
func (c *scriptedCLI) pageResult(head, js string) (CLIResult, error) {
	c.n++
	name := fmt.Sprintf("page-%d.yml", c.n)
	if err := c.write(name, []byte(snapshotOf(c.page))); err != nil {
		return CLIResult{}, err
	}
	var b strings.Builder
	b.WriteString(head)
	if js != "" {
		b.WriteString(code(js))
	}
	fmt.Fprintf(&b, "### Page\n- Page URL: %s\n- Page Title: %s\n### Snapshot\n- [Snapshot](%s/%s)\n", c.url,
		c.page.Title, cliArtifactDir, name)
	return CLIResult{Output: b.String()}, nil
}

// screenshot draws the page's boxes as a PNG at the viewport's size.
func (c *scriptedCLI) screenshot() (CLIResult, error) {
	v := playwright.DefaultViewport
	img := image.NewNRGBA(image.Rect(0, 0, v.Width, v.Height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for i, el := range c.page.Elements {
		if el.Hidden {
			continue
		}
		shade := uint8(40 + (i*50)%160)
		r := image.Rect(int(el.X), int(el.Y), int(el.X+el.Width), int(el.Y+el.Height))
		draw.Draw(img, r.Intersect(img.Bounds()), image.NewUniform(color.NRGBA{R: shade, G: shade, B: 200, A: 255}),
			image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return CLIResult{}, err
	}
	c.n++
	name := fmt.Sprintf("page-%d.png", c.n)
	if err := c.write(name, buf.Bytes()); err != nil {
		return CLIResult{}, err
	}
	link := cliArtifactDir + "/" + name
	return CLIResult{Output: "### Result\n- [Screenshot of viewport](" + link + ")\n" +
		code("await page.screenshot({ path: '"+link+"', scale: 'css', type: 'png' });")}, nil
}

func (c *scriptedCLI) write(name string, data []byte) error {
	dir := filepath.Join(c.dir, cliArtifactDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func code(js string) string { return "### Ran Playwright code\n```js\n" + js + "\n```\n" }

func jsString(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// snapshotOf is the page's accessibility snapshot: e1 the root, each
// element from e2 in order.
func snapshotOf(p ScriptedPage) string {
	var b strings.Builder
	b.WriteString("- generic [ref=e1]:\n")
	for i, el := range p.Elements {
		fmt.Fprintf(&b, "  - %s", el.Role)
		if el.Name != "" {
			fmt.Fprintf(&b, " %s", strconv.Quote(el.Name))
		}
		fmt.Fprintf(&b, " [ref=e%d]", i+2)
		if el.Text != "" {
			fmt.Fprintf(&b, ": %s", strconv.Quote(el.Text))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// locatorOf is the locator generate-locator gives el.
func locatorOf(el ScriptedElement) string {
	if el.TestID != "" {
		return "getByTestId(" + jsString(el.TestID) + ")"
	}
	if el.Name != "" {
		return "getByRole(" + jsString(el.Role) + ", { name: " + jsString(el.Name) + " })"
	}
	return "getByRole(" + jsString(el.Role) + ")"
}

// ---- the test runner ----------------------------------------------------

// scriptedRunner runs a compiled check against the page model.
type scriptedRunner struct {
	fetch scriptedFetcher
}

// scriptFailure is why a statement did not pass, in the words the real
// runner reports; indeterminate carries the indeterminate annotation.
type scriptFailure struct {
	msg           string
	indeterminate bool
}

// RunSpec implements TestRunner: each awaited statement, in order, until
// one does not pass.
func (r scriptedRunner) RunSpec(ctx context.Context, spec []byte) ([]byte, error) {
	s := &specRun{ctx: ctx, fetch: r.fetch, url: "about:blank"}
	for i, line := range strings.Split(string(spec), "\n") {
		stmt, ok := strings.CutPrefix(strings.TrimSpace(line), "await ")
		if !ok {
			continue
		}
		if f := s.exec(strings.TrimSuffix(stmt, ";")); f != nil {
			return scriptedReport(f, i+1)
		}
	}
	return scriptedReport(nil, 0)
}

// scriptedReport is the JSON reporter's report of one test.
func scriptedReport(f *scriptFailure, line int) ([]byte, error) {
	res := map[string]any{"status": "passed"}
	annotations := []map[string]string{}
	if f != nil {
		res["status"] = "failed"
		res["errors"] = []any{map[string]any{"message": "Error: " + f.msg, "location": map[string]int{"line": line}}}
		if f.indeterminate {
			annotations = append(annotations, map[string]string{"type": playwright.AnnotationIndeterminate})
		}
	}
	res["annotations"] = annotations
	return json.Marshal(map[string]any{"suites": []any{map[string]any{"specs": []any{map[string]any{
		"tests": []any{map[string]any{"annotations": []any{}, "results": []any{res}}}}}}}})
}

// specRun is one check's fresh browser.
type specRun struct {
	ctx   context.Context
	fetch scriptedFetcher
	url   string
	page  ScriptedPage
}

func failf(format string, a ...any) *scriptFailure {
	return &scriptFailure{msg: fmt.Sprintf(format, a...)}
}

func unreadable(stmt string) *scriptFailure {
	return &scriptFailure{msg: "maul: indeterminate the scripted browser cannot run " + strconv.Quote(stmt),
		indeterminate: true}
}

// exec runs one statement.
func (s *specRun) exec(stmt string) *scriptFailure {
	p := &exprParser{s: stmt}
	switch {
	case p.eat("page.setViewportSize("), p.eat("page.keyboard.press("):
		return nil
	case p.eat("page.goto("):
		path, ok := p.str()
		if !ok || !p.eat(")") || !p.done() {
			return unreadable(stmt)
		}
		u, _, page, err := s.fetch.load(s.ctx, path)
		if err != nil {
			return failf("page.goto: net::ERR_CONNECTION_REFUSED at %s", path)
		}
		s.url, s.page = u, page
		return nil
	case p.eat("maul."):
		return s.helper(p, stmt)
	case p.eat("expect("):
		return s.expectation(p, stmt)
	}
	l, err := p.locator()
	if err != nil || l.by == "" || !p.eat(".") {
		return unreadable(stmt)
	}
	switch op := p.ident(); op {
	case "click", "fill", "press", "selectOption":
		_, f := s.one(l, "locator."+op)
		return f
	}
	return unreadable(stmt)
}

// helper runs a maul.<helper>(…) statement of the support module.
func (s *specRun) helper(p *exprParser, stmt string) *scriptFailure {
	name := p.ident()
	args, ok := p.args()
	if !ok || !p.done() {
		return unreadable(stmt)
	}
	switch name {
	case "settled":
		return nil
	case "waitForPath", "expectPath":
		pattern, ok := argAt[string](args, 1)
		if !ok {
			return unreadable(stmt)
		}
		if pathMatches(pattern, s.url) {
			return nil
		}
		if name == "waitForPath" {
			return failf("page.waitForURL: Timeout %dms exceeded.", playwright.ActionTimeoutMS)
		}
		return failf("maul: url %s failed: the page is at %s", pattern, pathOf(s.url))
	case "visible", "legible", "clickable", "scroll":
		return s.single(name, args, stmt)
	case "size":
		return s.size(args, stmt)
	case "leftOf", "above", "noOverlap", "equalSize", "aligned":
		return s.relation(name, args, stmt)
	}
	return unreadable(stmt)
}

// single runs a helper on one element.
func (s *specRun) single(name string, args []any, stmt string) *scriptFailure {
	l, ok := argAt[*locator](args, 0)
	if !ok {
		return unreadable(stmt)
	}
	el, f := s.one(l, "locator.scrollIntoViewIfNeeded")
	if f != nil {
		return f
	}
	switch {
	case name == "visible" && el.Hidden:
		return failf("maul: visible failed: the element is hidden")
	case name == "legible" && el.Illegible:
		return failf("maul: legible failed: contrast 1.4:1 is under 4.5:1")
	case name == "clickable" && (el.Hidden || el.Covered):
		return failf("maul: clickable failed: another element receives a click at its centre")
	}
	return nil
}

// size is maul.size(loc, { width, height }, tolerance).
func (s *specRun) size(args []any, stmt string) *scriptFailure {
	l, ok1 := argAt[*locator](args, 0)
	want, ok2 := argAt[map[string]float64](args, 1)
	tol, ok3 := argAt[float64](args, 2)
	if !ok1 || !ok2 || !ok3 || len(args) != 3 {
		return unreadable(stmt)
	}
	b, f := s.box(l, "size")
	if f != nil {
		return f
	}
	var why []string
	if w, ok := want["width"]; ok {
		why = append(why, within("width", b.Width, w, tol)...)
	}
	if h, ok := want["height"]; ok {
		why = append(why, within("height", b.Height, h, tol)...)
	}
	if len(why) > 0 {
		return failf("maul: size failed: %s", strings.Join(why, "; "))
	}
	return nil
}

// relation is a box relation between two elements, as the support module
// judges it.
func (s *specRun) relation(name string, args []any, stmt string) *scriptFailure {
	a, ok1 := argAt[*locator](args, 0)
	b, ok2 := argAt[*locator](args, 1)
	axis, _ := argAt[string](args, 2)
	tol, ok3 := argAt[float64](args, len(args)-1)
	if !ok1 || !ok2 || !ok3 {
		return unreadable(stmt)
	}
	what := map[string]string{"leftOf": "left-of", "above": "above", "noOverlap": "no-overlap",
		"equalSize": "equal-size", "aligned": "aligned " + axis}[name]
	p, f := s.box(a, what)
	if f != nil {
		return f
	}
	q, f := s.box(b, what)
	if f != nil {
		return f
	}
	var why []string
	switch name {
	case "leftOf":
		if p.X+p.Width > q.X+tol {
			why = []string{"right edge " + px(p.X+p.Width) + " is past " + px(q.X)}
		}
	case "above":
		if p.Y+p.Height > q.Y+tol {
			why = []string{"bottom edge " + px(p.Y+p.Height) + " is below " + px(q.Y)}
		}
	case "noOverlap":
		w := math.Min(p.X+p.Width, q.X+q.Width) - math.Max(p.X, q.X)
		h := math.Min(p.Y+p.Height, q.Y+q.Height) - math.Max(p.Y, q.Y)
		if w > tol && h > tol {
			why = []string{"the boxes overlap by " + px(w) + "x" + px(h)}
		}
	case "equalSize":
		why = append(within("width", p.Width, q.Width, tol), within("height", p.Height, q.Height, tol)...)
	case "aligned":
		if axis == "x" {
			why = within("x", p.X, q.X, tol)
		} else {
			why = within("y", p.Y, q.Y, tol)
		}
	}
	if len(why) > 0 {
		return failf("maul: %s failed: %s", what, strings.Join(why, "; "))
	}
	return nil
}

// expectation runs expect(loc).toHaveText/toContainText/toHaveCount(…).
func (s *specRun) expectation(p *exprParser, stmt string) *scriptFailure {
	l, err := p.locator()
	if err != nil || l.by == "" || !p.eat(").") {
		return unreadable(stmt)
	}
	matcher := p.ident()
	if !p.eat("(") {
		return unreadable(stmt)
	}
	if matcher == "toHaveCount" {
		n, ok := p.num()
		if !ok || !p.eat(")") || !p.done() {
			return unreadable(stmt)
		}
		if got := len(s.matches(l)); float64(got) != n {
			return failf("expect(locator).toHaveCount(expected) failed: expected %v, received %d", n, got)
		}
		return nil
	}
	match, ok := p.textMatcher(matcher)
	if !ok || !p.eat(")") || !p.done() {
		return unreadable(stmt)
	}
	el, f := s.one(l, "expect."+matcher)
	if f != nil {
		return f
	}
	if !match(strings.TrimSpace(el.Text)) {
		return failf("expect(locator).%s(expected) failed", matcher)
	}
	return nil
}

// textMatcher reads toHaveText's or toContainText's argument, a string or
// maul.regex(string), as the match it asks for.
func (p *exprParser) textMatcher(matcher string) (func(string) bool, bool) {
	if matcher != "toHaveText" && matcher != "toContainText" {
		return nil, false
	}
	if want, ok := p.str(); ok {
		if matcher == "toHaveText" {
			return func(s string) bool { return s == want }, true
		}
		return func(s string) bool { return strings.Contains(s, want) }, true
	}
	pattern, ok := p.strIn("maul.regex(", ")")
	if !ok {
		return nil, false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false
	}
	return re.MatchString, true
}

// box is el's box for a box assertion; a hidden element has none.
func (s *specRun) box(l *locator, what string) (ScriptedElement, *scriptFailure) {
	el, f := s.one(l, "locator.boundingBox")
	if f != nil {
		return el, f
	}
	if el.Hidden {
		return el, failf("maul: %s failed: the element has no box (not visible)", what)
	}
	return el, nil
}

// one is the one element l resolves to, as Playwright's strict mode
// demands, or op's failure.
func (s *specRun) one(l *locator, op string) (ScriptedElement, *scriptFailure) {
	m := s.matches(l)
	switch {
	case len(m) == 0:
		return ScriptedElement{}, failf("%s: Timeout %dms exceeded.", op, playwright.ActionTimeoutMS)
	case len(m) > 1:
		return ScriptedElement{}, failf("%s: Error: strict mode violation: locator resolved to %d elements", op,
			len(m))
	}
	return m[0], nil
}

// matches are the page's elements l finds, nth applied.
func (s *specRun) matches(l *locator) []ScriptedElement {
	var out []ScriptedElement
	for _, el := range s.page.Elements {
		if l.finds(el) {
			out = append(out, el)
		}
	}
	switch {
	case l.nth == nthLast && len(out) > 0:
		return out[len(out)-1:]
	case l.nth >= 0 && l.nth < len(out):
		return out[l.nth : l.nth+1]
	case l.nth >= 0:
		return nil
	}
	return out
}

func within(name string, got, want, tolerance float64) []string {
	if math.Abs(got-want) <= tolerance {
		return nil
	}
	return []string{name + " " + px(got) + " not " + px(want) + " +/- " + px(tolerance)}
}

func px(n float64) string { return strconv.FormatFloat(math.Round(n*10)/10, 'f', -1, 64) }

func pathOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.Path
}

// pathMatches is the support module's pathPattern.
func pathMatches(pattern, u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	got := p.Path
	if strings.Contains(pattern, "?") && p.RawQuery != "" {
		got += "?" + p.RawQuery
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(got)
}

// ---- reading a compiled statement -----------------------------------------

// nthNone and nthLast are a locator without nth, and .last().
const (
	nthNone = -2
	nthLast = -1
)

// locator is a compiled locator expression: one way to find an element,
// optionally filtered and picked.
type locator struct {
	by, value, name, hasText string
	nth                      int
}

// finds reports el matched as Playwright's locators match: a role and an
// accessible name containing name (case-insensitive), a label or a text
// containing value, a test id exactly.
func (l *locator) finds(el ScriptedElement) bool {
	in := func(have, want string) bool { return strings.Contains(strings.ToLower(have), strings.ToLower(want)) }
	var ok bool
	switch l.by {
	case "role":
		ok = el.Role == l.value && (l.name == "" || in(el.Name, l.name))
	case "label":
		ok = el.Label != "" && in(el.Label, l.value)
	case "text":
		ok = in(el.Text, l.value) || (el.Text == "" && in(el.Name, l.value))
	case "testid":
		ok = el.TestID == l.value
	}
	return ok && (l.hasText == "" || in(el.Text+" "+el.Name, l.hasText))
}

// exprParser reads a compiled statement: maul's compiler writes every
// string as a JSON literal, so the grammar is small.
type exprParser struct {
	s string
	i int
}

func (p *exprParser) eat(lit string) bool {
	if strings.HasPrefix(p.s[p.i:], lit) {
		p.i += len(lit)
		return true
	}
	return false
}

func (p *exprParser) done() bool { return p.i == len(p.s) }

func (p *exprParser) ident() string {
	j := p.i
	for j < len(p.s) && (p.s[j] == '_' || p.s[j] >= 'a' && p.s[j] <= 'z' || p.s[j] >= 'A' && p.s[j] <= 'Z' ||
		j > p.i && p.s[j] >= '0' && p.s[j] <= '9') {
		j++
	}
	id := p.s[p.i:j]
	p.i = j
	return id
}

// str reads a JSON string literal.
func (p *exprParser) str() (string, bool) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", false
	}
	j := p.i + 1
	for j < len(p.s) && p.s[j] != '"' {
		if p.s[j] == '\\' {
			j++
		}
		j++
	}
	if j >= len(p.s) {
		return "", false
	}
	var out string
	if json.Unmarshal([]byte(p.s[p.i:j+1]), &out) != nil {
		return "", false
	}
	p.i = j + 1
	return out, true
}

func (p *exprParser) num() (float64, bool) {
	j := p.i
	for j < len(p.s) && strings.ContainsRune("-+.0123456789eE", rune(p.s[j])) {
		j++
	}
	f, err := strconv.ParseFloat(p.s[p.i:j], 64)
	if err != nil {
		return 0, false
	}
	p.i = j
	return f, true
}

var errLocator = errors.New("not a locator maul compiles")

// locator reads page[.getBy…(…)][.filter(…)][.nth(…)|.last()]; a bare page
// has no by. A second way to find (a within parent) is not read.
func (p *exprParser) locator() (*locator, error) {
	if !p.eat("page") {
		return nil, errLocator
	}
	l := &locator{nth: nthNone}
	for {
		save := p.i
		if !p.eat(".") {
			return l, nil
		}
		m := p.ident()
		switch m {
		case "getByRole", "getByLabel", "getByText", "getByTestId", "locator":
			if l.by != "" {
				return nil, errLocator
			}
			if err := p.finder(l, m); err != nil {
				return nil, err
			}
		case "filter":
			v, ok := p.strIn("({ hasText: ", " })")
			if !ok {
				return nil, errLocator
			}
			l.hasText = v
		case "nth":
			if !p.eat("(") {
				return nil, errLocator
			}
			n, ok := p.num()
			if !ok || !p.eat(")") {
				return nil, errLocator
			}
			l.nth = int(n)
		case "last":
			if !p.eat("()") {
				return nil, errLocator
			}
			l.nth = nthLast
		default:
			p.i = save
			return l, nil
		}
	}
}

// finder reads one getBy…(…) or locator(…) call's arguments.
func (p *exprParser) finder(l *locator, m string) error {
	if !p.eat("(") {
		return errLocator
	}
	v, ok := p.str()
	if !ok {
		return errLocator
	}
	l.by, l.value = map[string]string{"getByRole": "role", "getByLabel": "label", "getByText": "text",
		"getByTestId": "testid", "locator": "css"}[m], v
	if m == "getByRole" && p.eat(", { name: ") {
		if l.name, ok = p.str(); !ok || !p.eat(" }") {
			return errLocator
		}
	}
	if !p.eat(")") || l.by == "css" {
		// A CSS selector is not something the page model can match.
		return errLocator
	}
	return nil
}

// strIn reads open, a string, close.
func (p *exprParser) strIn(open, closing string) (string, bool) {
	if !p.eat(open) {
		return "", false
	}
	v, ok := p.str()
	return v, ok && p.eat(closing)
}

// args reads a helper's parenthesised arguments: locators, strings,
// numbers and { key: number } objects.
func (p *exprParser) args() ([]any, bool) {
	if !p.eat("(") {
		return nil, false
	}
	var out []any
	for !p.eat(")") {
		if len(out) > 0 && !p.eat(", ") {
			return nil, false
		}
		a, ok := p.arg()
		if !ok {
			return nil, false
		}
		out = append(out, a)
	}
	return out, true
}

func (p *exprParser) arg() (any, bool) {
	switch {
	case strings.HasPrefix(p.s[p.i:], "page"):
		l, err := p.locator()
		return l, err == nil
	case strings.HasPrefix(p.s[p.i:], `"`):
		return p.str()
	case p.eat("{ "):
		obj := map[string]float64{}
		for !p.eat("}") {
			if len(obj) > 0 && !p.eat(", ") {
				return nil, false
			}
			k := p.ident()
			if k == "" || !p.eat(": ") {
				return nil, false
			}
			v, ok := p.num()
			if !ok {
				return nil, false
			}
			obj[k] = v
			p.eat(" ")
		}
		return obj, true
	}
	return p.num()
}

// argAt is args[i] as a T.
func argAt[T any](args []any, i int) (T, bool) {
	var zero T
	if i < 0 || i >= len(args) {
		return zero, false
	}
	v, ok := args[i].(T)
	return v, ok
}
