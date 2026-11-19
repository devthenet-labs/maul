// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
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
		"create the item that must be left out yourself and assert that it is not there, after an assertion of " +
			"an item that is there in the same answer (the same step's `json`), so an empty answer does not pass",
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

// TestExecAssertionIDRuleIsStated (gate D-B2): both evaluate prompts state
// the marker id's whole grammar, its bound checks.MaxExecAssertionIDLen
// included, so an evaluator never names an assertion maul cannot count.
func TestExecAssertionIDRuleIsStated(t *testing.T) {
	want := fmt.Sprintf("the id naming that assertion (lowercase letters, digits and hyphens, starting with a "+
		"letter, at most %d characters)", checks.MaxExecAssertionIDLen)
	for name, p := range map[string]EvaluatePrompt{"slot": testEvaluatePrompt(),
		"cli": testWorkspacePrompt(testCLIWorkspace), "library": testWorkspacePrompt(testLibraryWorkspace)} {
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(flat(got), want) {
			t.Errorf("%s evaluate prompt lacks %q", name, want)
		}
	}
}

// TestExecMarkerFailsTheCheckIsStated (S1 demo #57; PR #320 F1, F2, F3):
// maul fails an exec check whose output has a line starting with what
// {{marker}} stands for, a secret drawn for every run, whatever its exit
// code, and never passes an output cut at its bound. The evaluator is told
// to print it only for an assertion's final judgement, never inside a retry
// or poll or on an error, and to assert on what the program observed; the
// reviewer of an exec check is told how maul runs it.
func TestExecMarkerFailsTheCheckIsStated(t *testing.T) {
	ev, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"maul fails an exec check whose output has a line that starts with what `" +
			checks.ExecMarkerPlaceholder + "` in its program files stands for (a secret maul draws for every run), " +
			"whatever its exit code and its `expect`",
		"when its final judgement of an assertion finds the outcome does not hold, never inside a retry or poll, " +
			"on a connection error, a status code or an exception",
		"never only on a line it prints whatever happened (`DONE`)",
		"A run whose output is too long to keep whole (over 64 KiB) never passes",
	} {
		if !strings.Contains(flat(ev), want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
	if strings.Contains(ev, "MAUL-ASSERTION-FAILED") {
		t.Error("the evaluate prompt names a marker the program could print without {{marker}}")
	}
	cr, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleExecCheck)))
	if err != nil {
		t.Fatal(err)
	}
	want := "It also fails whenever a line of its output starts with what `" + checks.ExecMarkerPlaceholder +
		"` in its program files stands for, a secret maul draws afresh for every run and gives only the program, " +
		"whatever its exit code and whatever else it prints; a run whose output is too long to keep whole never passes."
	if !strings.Contains(flat(cr), want) {
		t.Errorf("exec review prompt lacks %q", want)
	}
	http, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(http, checks.ExecMarkerPlaceholder) {
		t.Error("an http check's review names the exec marker")
	}
}

// TestExcludesOnASharedListSendsTheNamedRequest (O7-B-1, beatbattle-59): an
// excludes outcome on a shared list could not be proved, because rule 8 told
// the evaluator to narrow the outcome's request to its run's data and the
// green rule refused any check altering that request. Both now say the same
// thing: the named request is sent unchanged in a step of its own, asserting
// only what holds whatever other runs saved, and the narrowed request is a
// further step keeping every named parameter, with a kept item of the run
// and a control showing the narrowing alone lists the left-out item. The
// green rule still refuses a check that drops or changes a named parameter
// or narrows the left-out item away.
func TestExcludesOnASharedListSendsTheNamedRequest(t *testing.T) {
	rubric := []string{
		"A step adding to a request the outcome names only a narrowing to the check's own run (`{{run}}` text), " +
			"every named parameter kept as named, alters nothing if the check also sends that request unchanged, " +
			"asserts a kept item of its run in the narrowed answer, and lists the left-out item by the narrowing " +
			"alone.",
		// The exception sits between the rule it narrows and the
		// wrong-build rule, which still refuses a check whose unchanged
		// step a build ignoring the named parameters would pass.
		"tests less than the outcome: answer `does_not_prove`, even if what it does test is a reasonable reading " +
			"of the outcome. A step adding to a request",
		"lists the left-out item by the narrowing alone. Answer `does_not_prove` too for a check that a plausible " +
			"wrong build would still pass",
	}
	for _, want := range rubric {
		if !strings.Contains(GreenRubric, want) {
			t.Errorf("the green rule lacks %q", want)
		}
	}
	ev, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	cr, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"evaluate": ev, "checkreview": cr} {
		if !strings.Contains(flat(got), rubric[0]) {
			t.Errorf("the %s prompt is not shown the narrowing rule", name)
		}
	}
	// The reviewer of an http check is also told what the unchanged step
	// must hold: reads of rows it did not create only for what holds of any
	// data, and one assertion a build ignoring the named parameters without
	// the narrowing fails; a dropped or changed parameter still refused.
	shared := "For such a narrowing, the step of the unchanged request may read rows the check did not create, but " +
		"only for what holds of any data, and one of its assertions must fail on a build that ignores the " +
		"outcome's parameters where nothing narrows the list (the first item it lists meeting the outcome's rule, " +
		"with the left-out item created last on a newest-first list). Without such an assertion, or with a " +
		"parameter the outcome names dropped or changed, the check tests less than the outcome: answer " +
		"`does_not_prove`."
	if !strings.Contains(flat(cr), shared) {
		t.Errorf("http review prompt lacks %q", shared)
	}
	for name, src := range map[string]string{"browser": ExampleBrowserCheck, "exec": ExampleExecCheck} {
		got, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, src)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "For such a narrowing") {
			t.Errorf("a %s check's review is told of a request narrowing", name)
		}
	}
	for _, want := range []string{
		"On a list other runs share, narrow to your run's data as the rule above admits",
		"in the narrowed answer assert your kept items by index and assert that the index after the last is " +
			"`{\"$absent\": true}`, so the list ends there",
		"on the unchanged request assert only what holds of any data, its status and the first item " +
			"meeting the outcome's rule (`{\"$range\": [lo, hi]}` at its field), never your items' places",
		"on a newest-first list create the left-out item last",
	} {
		if !strings.Contains(flat(ev), want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
}

// TestExcludesRecipeIsAdmitted (O7-B-1): the check rule 8 now asks for is
// one maul stores: the named request unchanged with a range assertion on its
// first item (no {{run}}, which only $order and $includes need), the
// narrowed request with the kept item and the end of the list, and the
// narrowing alone listing the left-out item.
func TestExcludesRecipeIsAdmitted(t *testing.T) {
	beat := func(name string, bpm int, capture string) string {
		return fmt.Sprintf(`{"request":{"method":"POST","path":"/api/patterns","json":{"name":"{{run}}-%s",`+
			`"bpm":%d}},"expect":{"status":[201],"json":{"/bpm":%d}},"capture":{"%s":"/id"}}`, name, bpm, bpm, capture)
	}
	src := `{"id":"bpm-range-excludes.shared","outcome":"bpm-range-excludes","form":"http","http":{"target":"api",` +
		`"steps":[` + beat("in150", 150, "inid") + `,` + beat("out70", 70, "outid") + `,` +
		`{"request":{"method":"GET","path":"/api/patterns?minBpm=100&maxBpm=200"},` +
		`"expect":{"status":[200],"json":{"/0/bpm":{"$range":[100,200]}}}},` +
		`{"request":{"method":"GET","path":"/api/patterns?minBpm=100&maxBpm=200&q={{run}}"},` +
		`"expect":{"status":[200],"json":{"/0/id":"{{capture.inid}}","/1":{"$absent":true}}}},` +
		`{"request":{"method":"GET","path":"/api/patterns?q={{run}}"},` +
		`"expect":{"status":[200],"json":{"/0/id":"{{capture.outid}}","/1/id":"{{capture.inid}}"}}}]}}`
	c, err := checks.ParseProposal([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := checks.Context{Preview: &checks.Preview{Paths: []string{"/", "/api"}, Components: []string{"api", "web"}}}
	if err := checks.Validate(c, ctx); err != nil {
		t.Errorf("the recipe is refused: %v", err)
	}
}
