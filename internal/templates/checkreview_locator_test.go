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
const locatorRule = "How the check finds an element (a role and its accessible name, a label, a test id or CSS) " +
	"is not what it proves"

// TestCheckReviewPromptJudgesAssertionsNotLocators: a green browser review
// states the rule, keeping both of its limits: the element found must be
// the one the outcome is about, and what the outcome's own words promise
// (a label a person sees) must still be asserted. A red review, which
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
	for _, want := range []string{locatorRule, "so long as it finds the element the outcome is about",
		"A check proves only what it asserts", "only when the outcome's own words promise that thing"} {
		if !strings.Contains(green, want) {
			t.Errorf("the green browser review does not say %q", want)
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
