// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package playwright

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
)

// liveEnv names a directory holding node_modules with @playwright/test
// 1.64.1 (its Chromium installed) and typescript: the live tests run only
// when it is set.
const liveEnv = "MAUL_LIVE_PLAYWRIGHT"

// standInFixture replaces maul's keyed CDP fixture (S1-12) with a local
// launch: what is under test here is the compiled spec and the page
// functions, not the attach.
const standInFixture = `export { test, expect } from "@playwright/test";
`

// liveCase is one compiled check over the fixture page and the verdict it
// must come to: pass, fail or indeterminate.
type liveCase struct {
	name, want string
	expect     checks.Assertion
}

func css(sel string) *checks.Locator { return &checks.Locator{CSS: sel} }

// liveCases are the SP-5 fixtures' verdicts (spike-SP-5.md, fixtures2.out)
// plus clickable and box cases.
func liveCases() []liveCase {
	verdicts := []struct{ id, visible, legible string }{
		{"t-hero", "pass", "pass"},
		{"t-img", "indeterminate", "indeterminate"},
		{"t-clip", "indeterminate", "indeterminate"},
		{"t-grey", "pass", "fail"},
		{"t-faint", "fail", "fail"},
		{"t-transparent", "fail", "fail"},
		{"t-glow", "indeterminate", "indeterminate"},
		{"t-large", "pass", "pass"},
		{"t-translucent", "pass", "pass"},
		{"t-gradok", "pass", "pass"},
		{"t-gradmix", "pass", "indeterminate"},
		{"t-thin", "pass", "fail"},
		{"t-underlay", "pass", "pass"},
		{"t-emoji", "pass", "indeterminate"},
		{"t-blend", "indeterminate", "indeterminate"},
		// The known miss: brightness(0) renders black on black.
		{"t-filter", "pass", "pass"},
		{"t-offscreen", "fail", "pass"},
	}
	var out []liveCase
	for _, v := range verdicts {
		out = append(out,
			liveCase{v.id + "-visible", v.visible, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + v.id)}},
			liveCase{v.id + "-legible", v.legible, checks.Assertion{Assert: checks.AssertLegible, Locator: css("#" + v.id)}})
	}
	width, atLeast, atMost := 200.0, 48.0, 150.0
	out = append(out,
		liveCase{"c-free", "pass", checks.Assertion{Assert: checks.AssertClickable, Locator: css("#c-free")}},
		liveCase{"c-covered", "fail", checks.Assertion{Assert: checks.AssertClickable, Locator: css("#c-covered")}},
		liveCase{"c-veiled", "pass", checks.Assertion{Assert: checks.AssertClickable, Locator: css("#c-veiled")}},
		liveCase{"b-left-of", "pass", checks.Assertion{Assert: checks.AssertLeftOf, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-left-of-overlap", "fail", checks.Assertion{Assert: checks.AssertLeftOf, Locator: css("#b-first"),
			Other: css("#b-over")}},
		liveCase{"b-no-overlap", "pass", checks.Assertion{Assert: checks.AssertNoOverlap, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-overlap", "fail", checks.Assertion{Assert: checks.AssertNoOverlap, Locator: css("#b-first"),
			Other: css("#b-over")}},
		liveCase{"b-equal-size", "pass", checks.Assertion{Assert: checks.AssertEqualSize, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-aligned", "pass", checks.Assertion{Assert: checks.AssertAligned, Locator: css("#b-left"),
			Other: css("#b-right"), Axis: "y"}},
		liveCase{"b-above", "fail", checks.Assertion{Assert: checks.AssertAbove, Locator: css("#b-left"),
			Other: css("#b-right")}},
		liveCase{"b-size", "pass", checks.Assertion{Assert: checks.AssertSize, Locator: css("#b-left"),
			Width: &width}},
		liveCase{"b-size-at-least", "pass", checks.Assertion{Assert: checks.AssertSize, Locator: css("#b-left"),
			MinWidth: &atLeast, MinHeight: &atLeast}},
		liveCase{"b-size-at-most", "fail", checks.Assertion{Assert: checks.AssertSize, Locator: css("#b-left"),
			MaxWidth: &atMost}},
	)
	out = append(out, nameCases()...)
	out = append(out, seenCases()...)
	out = append(out, unmeasuredCases()...)
	out = append(out, ordinaryCases()...)
	out = append(out, clippedCases()...)
	return append(out, textCases()...)
}

// textCases are G7-B-1's and G7-B-2's (defect gate 7): a text assertion
// holds only on text a person can see. A line clipped to nothing by its
// own overflow, the same line asserted through its container, a
// screen-reader span, a closed details, display none and a faded-out
// line fail, in maul's words; text whose showing maul cannot settle (a
// carousel's other slide, a clip under a scale) is indeterminate. Text
// that shows judges as before: the rest of a container, a zero-height
// wrapper whose text overflows it, text below the fold, a container
// taller than the window, a list a person scrolls, a truncated title, a
// select's options, an empty element. And visible on a zero box says what
// hides its content, or is unsure when the content shows all the same.
func textCases() []liveCase {
	text := func(name, want, id string, m checks.TextMatch) liveCase {
		return liveCase{name, want, checks.Assertion{Assert: checks.AssertText, Locator: css("#" + id), Text: &m}}
	}
	see := func(id, want string) liveCase {
		return liveCase{id + "-visible", want, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + id)}}
	}
	return []liveCase{
		text("x-clipped-text", "fail", "x-clipped", checks.TextMatch{Equals: "Busiest: none"}),
		text("x-clipped-matches", "fail", "x-clipped", checks.TextMatch{Matches: "^Busiest: "}),
		text("x-panel-hidden", "fail", "x-panel", checks.TextMatch{Contains: "Busiest: kick (3)"}),
		text("x-panel-shown", "pass", "x-panel", checks.TextMatch{Contains: "Loud"}),
		text("x-sr-whole", "fail", "x-sr", checks.TextMatch{Equals: "Delete item 3"}),
		text("x-sr-shown", "pass", "x-sr", checks.TextMatch{Contains: "Delete"}),
		text("x-details-closed", "fail", "x-details", checks.TextMatch{Contains: "Hidden body"}),
		text("x-details-summary", "pass", "x-details", checks.TextMatch{Contains: "More"}),
		text("x-none-hidden", "fail", "x-none", checks.TextMatch{Contains: "Secret"}),
		text("x-none-shown", "pass", "x-none", checks.TextMatch{Matches: "^Shown"}),
		text("x-faded-text", "fail", "x-faded", checks.TextMatch{Equals: "Faded out"}),
		text("x-zero-text", "pass", "x-zero", checks.TextMatch{Equals: "Overflowing"}),
		text("x-zero-own-text", "pass", "x-zero-own", checks.TextMatch{Equals: "Own overflowing"}),
		text("x-tall-text", "pass", "x-tall", checks.TextMatch{Contains: "Top of a tall panel"}),
		text("x-list-scrolled", "pass", "x-list", checks.TextMatch{Contains: "Track 9"}),
		text("x-ellipsis-text", "pass", "x-ellipsis", checks.TextMatch{Contains: "at the end"}),
		text("x-select-text", "pass", "x-select", checks.TextMatch{Contains: "Snare"}),
		text("x-empty-text", "pass", "x-empty", checks.TextMatch{Matches: "^$"}),
		text("x-below-text", "pass", "x-below", checks.TextMatch{Equals: "Far below the fold"}),
		text("x-carousel-other", "indeterminate", "x-carousel", checks.TextMatch{Contains: "Slide three"}),
		text("x-carousel-first", "pass", "x-carousel", checks.TextMatch{Contains: "Slide one"}),
		text("x-warped-text", "indeterminate", "x-warped", checks.TextMatch{Equals: "Under a scale"}),
		text("x-wrong-text", "fail", "x-below", checks.TextMatch{Equals: "Another line"}),
		see("x-clipped", "fail"),
		see("x-zero", "indeterminate"),
		see("x-zero-own", "indeterminate"),
	}
}

// textUnseen are the text cases whose text is there and matches but is
// not shown: each fails in maul's own words, apart from a text that does
// not match (the runner's expectation, x-wrong-text).
var textUnseen = []string{"x-clipped-text", "x-clipped-matches", "x-panel-hidden", "x-sr-whole", "x-details-closed",
	"x-none-hidden", "x-faded-text"}

// seenCases are review finding 2's: visible means a person can see it. An
// element clipped to nothing (a collapsed panel, a 1px screen-reader span,
// clip-path inset(50%)) fails; one covered at its centre by something
// opaque is indeterminate, never a pass; text only in its descendants is
// judged as its own text is. The controls judge as before: a transparent
// or pointer-events:none layer, a cover that leaves the centre clear, a
// sticky header the element is scrolled clear of, readable label spans,
// an icon button with a screen-reader or faded-out label, an underlined
// nav link, a link wrapped over two lines.
func seenCases() []liveCase {
	see := func(id, want string) liveCase {
		return liveCase{id + "-visible", want, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + id)}}
	}
	return []liveCase{
		see("v-occluded", "indeterminate"),
		see("v-collapsed", "fail"),
		see("v-sronly", "fail"),
		see("v-clippath", "fail"),
		see("v-childwhite", "fail"),
		see("v-under-sticky", "indeterminate"),
		see("c-covered", "indeterminate"),
		see("v-plain", "pass"),
		see("v-glass", "pass"),
		see("c-veiled", "pass"),
		see("v-partly", "pass"),
		see("v-childdark", "pass"),
		see("v-below-sticky", "pass"),
		see("v-navlink", "pass"),
		see("v-wrapped", "pass"),
		see("v-card", "pass"),
		see("v-play", "pass"),
		see("v-icon", "pass"),
		see("v-tipped", "pass"),
		{"v-play-role-visible", "pass", checks.Assertion{Assert: checks.AssertVisible,
			Locator: &checks.Locator{Role: "button", Name: "Listen"}}},
		{"v-childwhite-role-visible", "fail", checks.Assertion{Assert: checks.AssertVisible,
			Locator: &checks.Locator{Role: "button", Name: "Save"}}},
		{"v-navlink-legible", "pass", checks.Assertion{Assert: checks.AssertLegible, Locator: css("#v-navlink")}},
	}
}

// unmeasuredCases are review finding 10's: text over an <img>, a <canvas>
// or a positioned ::before scrim is indeterminate for visible and legible
// whichever way it reads (maul does not know what they paint), while a
// flow ::before beside the text judges as before; a web component's
// button in an open shadow root is hit-tested and its background resolved
// through the root, so it passes clickable, visible and legible, a covered
// one still fails clickable, and text slotted over the component's own
// shadow background is judged against it.
func unmeasuredCases() []liveCase {
	both := func(id, want string) []liveCase {
		return []liveCase{
			{id + "-visible", want, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + id)}},
			{id + "-legible", want, checks.Assertion{Assert: checks.AssertLegible, Locator: css("#" + id)}},
		}
	}
	yes := true
	shadow := &checks.Locator{Role: "button", Name: "Shadow button", Exact: &yes}
	covered := &checks.Locator{Role: "button", Name: "Covered shadow button", Exact: &yes}
	var out []liveCase
	for _, id := range []string{"i-dark-img", "i-white-img", "i-dark-canvas", "i-white-canvas", "i-dark-before",
		"i-white-before"} {
		out = append(out, both(id, "indeterminate")...)
	}
	out = append(out, both("i-dot", "pass")...)
	out = append(out, both("sh-slotted", "pass")...)
	return append(out,
		liveCase{"sh-button-clickable", "pass", checks.Assertion{Assert: checks.AssertClickable, Locator: shadow}},
		liveCase{"sh-button-visible", "pass", checks.Assertion{Assert: checks.AssertVisible, Locator: shadow}},
		liveCase{"sh-button-legible", "pass", checks.Assertion{Assert: checks.AssertLegible, Locator: shadow}},
		liveCase{"sh-covered-clickable", "fail", checks.Assertion{Assert: checks.AssertClickable, Locator: covered}},
		liveCase{"sh-covered-visible", "indeterminate", checks.Assertion{Assert: checks.AssertVisible, Locator: covered}},
		liveCase{"sh-text-legible", "pass", checks.Assertion{Assert: checks.AssertLegible,
			Locator: &checks.Locator{Text: "Shadow text, pointer-events none"}}},
		liveCase{"sh-host-visible", "pass", checks.Assertion{Assert: checks.AssertVisible, Locator: css("#sh-light-host")}},
	)
}

// ordinaryCases are the review of the two findings' fix: pages that must
// judge as before it (a nav link whose underline is scaled away until
// hover, a bullet drawn by a negative-margin ::before, a ::before bar
// placed by a positioned ancestor or at the right edge, text in a
// light-DOM custom element, a heading in a slanted hero, a round avatar,
// a table row found by its role, a panel inside an overflow clip margin),
// and paint maul must not guess at, in either direction: a ::before an
// ancestor places under the text, text positioned over a flow ::before or
// sharing its grid cell, text slotted into a closed shadow root, a cover
// painted by its flow ::before, a clip-path shape that leaves nothing, and
// a clip maul's arithmetic gets wrong while the element is found at its
// centre, and a screen-reader checkbox behind its styled label (never a
// fail; a field in a collapsed panel still fails). A component's button
// is judged by the label slotted into it.
func ordinaryCases() []liveCase {
	both := func(id, want string) []liveCase {
		return []liveCase{
			{id + "-visible", want, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + id)}},
			{id + "-legible", want, checks.Assertion{Assert: checks.AssertLegible, Locator: css("#" + id)}},
		}
	}
	see := func(id, want string) liveCase {
		return liveCase{id + "-visible", want, checks.Assertion{Assert: checks.AssertVisible, Locator: css("#" + id)}}
	}
	var out []liveCase
	for _, id := range []string{"r-underline", "r-bullet", "r-topbar", "r-rightbar", "r-frame", "r-slant", "r-avatar"} {
		out = append(out, both(id, "pass")...)
	}
	for _, id := range []string{"r-veil", "r-closed", "r-band-white", "r-band-dark", "r-stack-white"} {
		out = append(out, both(id, "indeterminate")...)
	}
	return append(out,
		see("r-gone", "indeterminate"),
		see("r-row", "pass"),
		see("r-margin", "pass"),
		see("r-margin-box", "indeterminate"),
		see("r-flowlid", "indeterminate"),
		see("r-peer", "indeterminate"),
		see("r-peer-box", "pass"),
		see("r-collapsed-input", "fail"),
		// A component's button found by its role: its slotted label is its
		// text. Bare text is not measured (never a pass on the box alone),
		// a slotted element is judged inside the component.
		liveCase{"r-pill-white-visible", "indeterminate", checks.Assertion{Assert: checks.AssertVisible,
			Locator: &checks.Locator{Role: "button", Name: "White slotted label"}}},
		liveCase{"r-pill-span-visible", "fail", checks.Assertion{Assert: checks.AssertVisible,
			Locator: &checks.Locator{Role: "button", Name: "White slotted span"}}},
		liveCase{"r-pill-read-visible", "pass", checks.Assertion{Assert: checks.AssertVisible,
			Locator: &checks.Locator{Role: "button", Name: "Readable slotted span"}}},
	)
}

// notFailed are the cases whose verdict is not a fail: none may carry
// maul's "<what> failed:" words, which previewtools reads as a failed
// assertion, so an element maul cannot judge is never red by assertion.
func notFailed(cases []liveCase) []string {
	var out []string
	for _, c := range cases {
		if c.want != "fail" {
			out = append(out, c.name)
		}
	}
	return out
}

// nameCases are G4-B-BR-1's: an exact label or role name is Playwright's
// exact, the whole name, case-sensitive, whitespace runs one space; without
// exact a part of it in any case, so "Beat name" also finds the button
// labelled after a run's beat and matches two elements.
func nameCases() []liveCase {
	yes := true
	see := func(name, want string, l checks.Locator) liveCase {
		return liveCase{name, want, checks.Assertion{Assert: checks.AssertVisible, Locator: &l}}
	}
	icon := checks.Locator{Role: "button", Name: "Play", Exact: &yes}
	return []liveCase{
		see("n-label-exact", "pass", checks.Locator{Label: "Beat name", Exact: &yes}),
		see("n-label-part", "fail", checks.Locator{Label: "Beat name"}),
		see("n-label-exact-case", "fail", checks.Locator{Label: "beat name", Exact: &yes}),
		see("n-label-exact-space", "pass", checks.Locator{Label: " Beat  name ", Exact: &yes}),
		see("n-role-exact-icon", "fail", icon),
		see("n-role-exact", "pass", checks.Locator{Role: "button", Name: "▶ Play", Exact: &yes}),
		see("n-role-part", "pass", checks.Locator{Role: "button", Name: "Play"}),
		{"n-miss-legible", "fail", checks.Assertion{Assert: checks.AssertLegible, Locator: &icon}},
		{"n-miss-clickable", "fail", checks.Assertion{Assert: checks.AssertClickable, Locator: &icon}},
		{"n-miss-no-overlap", "fail", checks.Assertion{Assert: checks.AssertNoOverlap, Locator: css("#n-beat"),
			Other: &icon}},
	}
}

// missesNoElement are the name cases whose exact locator finds nothing:
// each must fail as Playwright's own timeout, never as maul's "<what>
// failed:" words, which previewtools reads as a failed assertion; so an
// exact name that misses can never make a check red by assertion.
var missesNoElement = []string{"n-label-exact-case", "n-role-exact-icon", "n-miss-legible", "n-miss-clickable",
	"n-miss-no-overlap"}

// clippedCases are bug 12's: box relations measure what is drawn, each box
// cut by its clipping ancestors, and an element clipped away entirely is
// not drawn, so a relation on it is indeterminate, never a fail.
func clippedCases() []liveCase {
	rel := func(name, want string, assert checks.AssertKind, a, b string) liveCase {
		return liveCase{name, want, checks.Assertion{Assert: assert, Locator: css(a), Other: css(b)}}
	}
	step, wide, narrow := 48.0, 100.0, 40.0
	return []liveCase{
		// The S1 demo's false alarm: step 1 scrolled out of view under KICK.
		rel("k-clipped-no-overlap", "indeterminate", checks.AssertNoOverlap, "#k-step1", "#k-label"),
		rel("k-clipped-left-of", "indeterminate", checks.AssertLeftOf, "#k-label", "#k-step1"),
		rel("k-clipped-equal-size", "indeterminate", checks.AssertEqualSize, "#k-step1", "#k-step2"),
		{"k-clipped-size", "indeterminate", checks.Assertion{Assert: checks.AssertSize, Locator: css("#k-step1"),
			Width: &step}},
		rel("k-drawn-no-overlap", "pass", checks.AssertNoOverlap, "#k-step2", "#k-label"),
		// Partly clipped: the drawn part is beside the label; its layout
		// box still measures 48 wide, and a fail is not a defect's.
		rel("p-cut-no-overlap", "pass", checks.AssertNoOverlap, "#p-step1", "#p-label"),
		rel("p-cut-left-of", "pass", checks.AssertLeftOf, "#p-label", "#p-step1"),
		{"p-cut-size", "pass", checks.Assertion{Assert: checks.AssertSize, Locator: css("#p-step1"), Width: &step}},
		{"p-cut-size-wrong", "indeterminate", checks.Assertion{Assert: checks.AssertSize, Locator: css("#p-step1"),
			Width: &wide}},
		// Bounds take the same rule: a bound that fails on a cut box is
		// indeterminate, one that holds passes, none judges a box not drawn.
		{"p-cut-size-at-least", "pass", checks.Assertion{Assert: checks.AssertSize, Locator: css("#p-step1"),
			MinWidth: &narrow}},
		{"p-cut-size-at-least-wrong", "indeterminate", checks.Assertion{Assert: checks.AssertSize,
			Locator: css("#p-step1"), MinWidth: &wide}},
		{"p-cut-size-at-most-wrong", "indeterminate", checks.Assertion{Assert: checks.AssertSize,
			Locator: css("#p-step1"), MaxWidth: &narrow}},
		{"k-clipped-size-at-least", "indeterminate", checks.Assertion{Assert: checks.AssertSize,
			Locator: css("#k-step1"), MinWidth: &narrow}},
		// A drawn step under a sticky label still overlaps it.
		rel("s-under-sticky", "fail", checks.AssertNoOverlap, "#s-stepa", "#s-label"),
		rel("s-beside-sticky", "pass", checks.AssertNoOverlap, "#s-stepc", "#s-label"),
	}
}

// TestLivePageFunctions compiles one check per fixture and assertion, runs
// them with the real Playwright test runner against the fixture page and
// holds each to its verdict; it also typechecks every golden spec and the
// support module with tsc --strict.
func TestLivePageFunctions(t *testing.T) {
	root := os.Getenv(liveEnv)
	if root == "" {
		t.Skipf("set %s to a directory with node_modules holding @playwright/test and typescript", liveEnv)
	}
	dir, err := os.MkdirTemp(root, "maul-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	page, err := os.ReadFile(filepath.Join("testdata", "pagefns-fixtures.html"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}))
	t.Cleanup(srv.Close)

	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(SupportFile, Support())
	write("maul-fixture.ts", []byte(standInFixture))
	write("playwright.config.ts", []byte(`import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: ".", testMatch: "*.spec.ts", workers: 4, retries: 0,
  reporter: [["json", { outputFile: "results.json" }]],
  use: { baseURL: process.env.MAUL_BASE },
});
`))
	cases := liveCases()
	for _, c := range cases {
		a := c.expect
		b := checks.BrowserCheck{Steps: []checks.BrowserStep{{Goto: "/fixtures.html"}, {Expect: &a}}}
		spec, err := Compile(b, Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		write(c.name+".spec.ts", spec)
	}
	t.Run("typecheck", func(t *testing.T) { typecheck(t, root, dir) })

	cmd := exec.Command(filepath.Join(root, "node_modules", ".bin", "playwright"), "test")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MAUL_BASE="+srv.URL)
	start := time.Now()
	out, _ := cmd.CombinedOutput()
	t.Logf("playwright ran %d checks in %s", len(cases), time.Since(start).Round(time.Millisecond))
	got := readVerdicts(t, filepath.Join(dir, "results.json"), out)
	for _, c := range cases {
		if v := got[c.name+".spec.ts"]; v.verdict != c.want {
			t.Errorf("%s: verdict %s, want %s (%s)", c.name, v.verdict, c.want, v.why)
		}
	}
	for _, name := range notFailed(cases) {
		if why := stripEscapes(got[name+".spec.ts"].why); strings.Contains(why, " failed: ") {
			t.Errorf("%s: judged %q, which reads as a failed assertion", name, why)
		}
	}
	for _, name := range textUnseen {
		why := strings.TrimPrefix(stripEscapes(got[name+".spec.ts"].why), "Error: ")
		if !strings.HasPrefix(why, "maul: text failed: ") {
			t.Errorf("%s: text that is not shown failed as %q, want maul's text failure", name, why)
		}
	}
	if why := stripEscapes(got["x-wrong-text.spec.ts"].why); !strings.Contains(why, "toHaveText") {
		t.Errorf("x-wrong-text: a text that does not match failed as %q, want the runner's expectation", why)
	}
	if why := stripEscapes(got["x-clipped-visible.spec.ts"].why); !strings.Contains(why, "zero box") ||
		!strings.Contains(why, "overflow") {
		t.Errorf("x-clipped-visible: failed as %q, want the zero box and the overflow that hides its content", why)
	}
	for _, name := range missesNoElement {
		why := strings.TrimPrefix(stripEscapes(got[name+".spec.ts"].why), "Error: ")
		if !strings.Contains(why, "Timeout") || strings.HasPrefix(why, "maul: ") {
			t.Errorf("%s: an exact name that finds nothing failed as %q, want Playwright's timeout", name, why)
		}
	}
}

// colourEscape is a terminal's colour escape in a runner message.
var colourEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripEscapes(s string) string { return colourEscape.ReplaceAllString(s, "") }

// typecheck runs tsc --noEmit --strict over the live specs, the goldens and
// the support module.
func typecheck(t *testing.T, root, dir string) {
	goldens, err := filepath.Glob(filepath.Join("testdata", "*.spec.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range goldens {
		b, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "golden-"+filepath.Base(g)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.spec.ts"))
	files = append(files, filepath.Join(dir, SupportFile), filepath.Join(dir, "maul-fixture.ts"))
	sort.Strings(files)
	args := append([]string{"--noEmit", "--strict", "--target", "es2022", "--module", "nodenext",
		"--moduleResolution", "nodenext", "--lib", "es2022,dom", "--skipLibCheck"}, files...)
	cmd := exec.Command(filepath.Join(root, "node_modules", ".bin", "tsc"), args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("tsc: %v\n%s", err, out)
	}
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "golden-") {
			_ = os.Remove(f)
		}
	}
}

type liveVerdict struct{ verdict, why string }

// readVerdicts maps each spec file to its verdict: passed is pass, failed
// with the indeterminate annotation is indeterminate, failed otherwise is
// fail.
func readVerdicts(t *testing.T, path string, output []byte) map[string]liveVerdict {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no JSON report (%v); playwright said:\n%s", err, output)
	}
	var report struct {
		Suites []struct {
			File  string `json:"file"`
			Specs []struct {
				Tests []struct {
					Annotations []struct{ Type string } `json:"annotations"`
					Results     []struct {
						Status      string                     `json:"status"`
						Annotations []struct{ Type string }    `json:"annotations"`
						Errors      []struct{ Message string } `json:"errors"`
					} `json:"results"`
				} `json:"tests"`
			} `json:"specs"`
		} `json:"suites"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	got := map[string]liveVerdict{}
	for _, s := range report.Suites {
		for _, sp := range s.Specs {
			for _, ts := range sp.Tests {
				for _, r := range ts.Results {
					v := liveVerdict{verdict: "fail"}
					if len(r.Errors) > 0 {
						v.why = firstLine(r.Errors[0].Message)
					}
					indeterminate := false
					for _, a := range append(ts.Annotations, r.Annotations...) {
						indeterminate = indeterminate || a.Type == AnnotationIndeterminate
					}
					switch {
					case r.Status == "passed":
						v.verdict = "pass"
					case indeterminate:
						v.verdict = "indeterminate"
					}
					got[filepath.Base(s.File)] = v
				}
			}
		}
	}
	if len(got) == 0 {
		t.Fatalf("the report holds no results; playwright said:\n%s", output)
	}
	return got
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return fmt.Sprintf("%.200s", s)
}
