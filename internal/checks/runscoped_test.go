// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import "testing"

// TestFailsRunScopedText (D-B4): a red browser text check is one whose
// asserted element holds the run's data only when it writes data of its run
// and the failing text expect's own locator names {{run}}; {{run}} only in
// a parent it is within (whose first row may be another run's), only in the
// expected text (a whole list's text), a count, a layout read, another step
// or another form is not one.
func TestFailsRunScopedText(t *testing.T) {
	fill := BrowserStep{Fill: &Fill{Locator: Locator{Label: "Name"}, Text: "{{run}}-beat"}}
	text := func(l Locator, contains string) BrowserStep {
		return BrowserStep{Expect: &Assertion{Assert: AssertText, Locator: &l, Text: &TextMatch{Contains: contains}}}
	}
	browser := func(steps ...BrowserStep) Check {
		return Check{Form: FormBrowser, Browser: &BrowserCheck{Steps: steps}}
	}
	row := Locator{CSS: "li", HasText: "{{run}}-beat"}
	for _, tc := range []struct {
		name    string
		check   Check
		failing int
		want    bool
	}{
		{"the run's row by hasText", browser(BrowserStep{Goto: "/"}, fill, text(row, "swing 20%")), 2, true},
		{"the run's row by hasText, nth", browser(BrowserStep{Goto: "/"}, fill,
			text(Locator{CSS: "li", HasText: "{{run}}-beat1", Nth: new(int)}, "swing 20%")), 2, true},
		{"only within the run's row", browser(BrowserStep{Goto: "/"}, fill,
			text(Locator{CSS: ".meta", Within: &row}, "swing 20%")), 2, false},
		{"the first row of a list found by the run", browser(BrowserStep{Goto: "/"}, fill,
			text(Locator{CSS: "li", Nth: new(int), Within: &Locator{CSS: "ul", HasText: "{{run}}-beat1"}},
				"{{run}}-beat1 · swing 20%")), 2, false},
		{"by its text", browser(BrowserStep{Goto: "/"}, fill,
			text(Locator{Text: "{{run}}-beat"}, "swing 20%")), 2, true},
		{"a list, the run only in the expected text", browser(BrowserStep{Goto: "/"}, fill,
			text(Locator{CSS: "#leaderboard"}, "{{run}}-beat · swing 20%")), 2, false},
		{"writes nothing of its run", browser(BrowserStep{Goto: "/"}, text(row, "swing 20%")), 1, false},
		{"another step failing", browser(BrowserStep{Goto: "/"}, fill, text(row, "swing 20%")), 1, false},
		{"out of range", browser(BrowserStep{Goto: "/"}, fill, text(row, "swing 20%")), 3, false},
		{"a count", browser(BrowserStep{Goto: "/"}, fill, BrowserStep{Expect: &Assertion{Assert: AssertCount,
			Locator: &row, Count: new(int)}}), 2, false},
		{"a layout read", browser(BrowserStep{Goto: "/"}, fill, BrowserStep{Expect: &Assertion{Assert: AssertVisible,
			Locator: &row}}), 2, false},
		{"not a browser check", Check{Form: FormHTTP, HTTP: &HTTPCheck{}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailsRunScopedText(tc.check, tc.failing); got != tc.want {
				t.Errorf("FailsRunScopedText = %v, want %v", got, tc.want)
			}
		})
	}
}
