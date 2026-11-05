// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestGreenRubricRefusesACheckThatCannotGoRed (S1 demo #57-#60): every
// outcome left unconfirmed on a correct build had a check the reviewer
// rightly refused because a wrong build would still pass it: a range filter
// checked only by what it kept (#59), a "same array" checked on three items
// (#58), an exec program that printed the marker and went on to exit 0
// (#57), a level checked on an empty beat that reads "sparse" by default
// (#60). The shared green rule names each, so the evaluator writes to it and
// the reviewer judges by it.
func TestGreenRubricRefusesACheckThatCannotGoRed(t *testing.T) {
	wants := []string{
		"Answer `does_not_prove` too for a check that a plausible wrong build would still pass",
		"shows something is left out, refused or absent only by asserting what is there",
		"it must create the item to be left out and assert that item is not there",
		"compares only some of what the outcome says must match",
		"whose inputs make the expected result the one a fixed or default answer gives",
		"whose program can print what its `expect` looks for and exit as it expects whatever it observed",
	}
	for _, want := range wants {
		if !strings.Contains(GreenRubric, want) {
			t.Errorf("the green rule lacks %q", want)
		}
	}
	ev, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	cr, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleExecCheck)))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"evaluate": ev, "checkreview": cr} {
		if !strings.Contains(flat(got), wants[0]) {
			t.Errorf("the %s prompt is not shown the rule", name)
		}
	}
}

// TestEvaluatePromptAsksForChecksThatCanGoRed (S1 demo #57-#60): the
// evaluator's list says how to make each kind of check able to fail: the
// left-out item created and asserted absent beside a kept one, a whole
// compared whole, a value input away from its default, both sides of a
// bound, and last the wrong builds named and each matched to an assertion.
func TestEvaluatePromptAsksForChecksThatCanGoRed(t *testing.T) {
	got, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"create the item that must be left out yourself and assert that it is not there, beside an assertion of " +
			"an item that is there, so an empty answer does not pass",
		"assert that the index after the last is `{\"$absent\": true}`",
		"assert a `count` of 0 for a locator naming the left-out item's `{{run}}` text",
		"compare all of it, not a sample",
		"the value the outcome expects is not the one a new, empty item shows",
		"create an item on each side of the bound and assert both",
		"name the wrong builds a careless change could ship",
		"find the assertion of your check that would go red on it",
		"where no check you can write would go red, say so in the verdict's note instead of proposing a weaker one",
	} {
		if !strings.Contains(flat(got), want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	p := testEvaluatePrompt()
	p.Tools.Browser = false
	noBrowser, err := RenderEvaluatePrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noBrowser, "a `count` of 0") {
		t.Error("a preview with no browser is told to write a browser count")
	}
}

// TestExecMarkerFailsTheCheckIsStated (S1 demo #57): maul fails an exec
// check whose output has a marker line whatever its exit code, so the
// evaluator is told to print it for every failed assertion and to assert on
// what the program observed, and the reviewer of an exec check is told how
// maul runs it.
func TestExecMarkerFailsTheCheckIsStated(t *testing.T) {
	ev, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"maul fails an exec check whose output has a line that starts with `" + checks.ExecAssertionMarker +
			"`, whatever its exit code and its `expect`",
		"never only on a line it prints whatever happened (`DONE`)",
	} {
		if !strings.Contains(flat(ev), want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
	cr, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleExecCheck)))
	if err != nil {
		t.Fatal(err)
	}
	want := "It also fails whenever a line of its output starts with `" + checks.ExecAssertionMarker +
		"`, whatever its exit code and whatever else it prints."
	if !strings.Contains(flat(cr), want) {
		t.Errorf("exec review prompt lacks %q", want)
	}
	http, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(http, checks.ExecAssertionMarker) {
		t.Error("an http check's review names the exec marker")
	}
}
