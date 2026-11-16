// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// locatorRule is the green browser review's word on how a check finds an
// element (G5R2-B-BR-1): in one review the reviewer refused three checks for
// clicking `Double tempo`, the button's accessible name, where the outcome
// says Double, and passed six that found the same buttons the same way.
const locatorRule = "How the check finds an element it clicks or fills, or whose text or state it asserts (a role " +
	"and its accessible name, a label, a test id or CSS), is not what it proves"

// TestCheckReviewPromptJudgesAssertionsNotLocators: a green browser review
// states the rule, keeping both of its limits: the element found must be
// the one the outcome is about, and a label a person sees that the outcome's
// own words promise must still be asserted. It is scoped to that label, not
// to anything a check leaves unasserted, and it never claims a locator that
// matches nothing only fails a check: a count of 0 over it passes. A red review, which
// already never refuses for a locator that found its element, and an http
// check, which has no element, are not told it.
func TestCheckReviewPromptJudgesAssertionsNotLocators(t *testing.T) {
	render := func(p CheckReviewPrompt) string {
		t.Helper()
		got, err := RenderCheckReviewPrompt(p)
		if err != nil {
			t.Fatal(err)
		}
		return flat(got)
	}
	green := render(testCheckReviewPrompt(mustCheck(t, ExampleBrowserCheck)))
	for _, want := range []string{locatorRule, "so long as that element is the one the outcome is about",
		"for not asserting the label a person sees on that element unless the outcome's own words promise that label"} {
		if !strings.Contains(green, want) {
			t.Errorf("the green browser review does not say %q", want)
		}
	}
	for _, broad := range []string{"matches nothing only makes the check fail", "A check proves only what it asserts",
		"only when the outcome's own words promise that thing"} {
		if strings.Contains(green, broad) {
			t.Errorf("the green browser review says %q", broad)
		}
	}
	red := testCheckReviewPrompt(mustCheck(t, ExampleBrowserCheck))
	red.ExpectFail, red.FailedStep, red.FailedAssertion = true, 11, "expect"
	for name, got := range map[string]string{"red browser": render(red),
		"green http": render(testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)))} {
		if strings.Contains(got, locatorRule) {
			t.Errorf("the %s review states the locator rule", name)
		}
	}
}
