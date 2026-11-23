// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"slices"
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
// (#60). The green rule names each, and the reviewer judges by it; the
// observed evaluator writes no check, so its prompt states no green rule.
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
	cr, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleExecCheck)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flat(cr), wants[0]) {
		t.Error("the checkreview prompt is not shown the rule")
	}
}

// TestEvaluatePromptLooksWhereAWrongBuildDiffers (S1 demo #57-#60, as the
// observed evaluator states it): every evaluate prompt asks the evaluator
// to try an outcome where a careless build would answer otherwise: a
// left-out item created and looked for beside a kept one, both sides of a
// rule and of a bound, inputs other than the defaults, and the wrong builds
// named.
func TestEvaluatePromptLooksWhereAWrongBuildDiffers(t *testing.T) {
	shared := []string{
		"Try the outcome where a careless build would get it wrong: inputs other than the defaults, both sides of " +
			"a rule",
		"each side of a bound",
		"The wrong builds to look for: the value fixed or left at its default",
		"the rule right only on the example, the bound off by one",
	}
	for name, tt := range map[string]struct {
		p    EvaluatePrompt
		want []string
	}{
		"slot": {testEvaluatePrompt(), []string{"create the item that must be left out and look for it beside one " +
			"that is there, so an empty answer does not pass", "for a change, the state before and after it"}},
		"cli": {testWorkspacePrompt(testCLIWorkspace), []string{"the input that must be left out beside one that " +
			"is kept"}},
		"library": {testWorkspacePrompt(testLibraryWorkspace), []string{"the input that must be left out beside " +
			"one that is kept"}},
	} {
		got, err := RenderEvaluatePrompt(tt.p)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range slices.Concat(shared, tt.want) {
			if !strings.Contains(flat(got), want) {
				t.Errorf("%s: prompt lacks %q", name, want)
			}
		}
	}
}

// TestExecMarkerFailsTheCheckIsStated (S1 demo #57; PR #320 F1, F2, F3):
// maul fails an exec check whose output has a line starting with what
// {{marker}} stands for, a secret drawn for every run, whatever its exit
// code, and never passes an output cut at its bound. The evaluator is told
// to print it only for an assertion's final judgement, never inside a retry
// or poll or on an error, and to assert on what the program observed; the
// reviewer of an exec check is told how maul runs it. The observed
// evaluator writes no check, so its prompt names no marker
// (TestEvaluatePromptHasNoCheckSections).
func TestExecMarkerFailsTheCheckIsStated(t *testing.T) {
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
// or narrows the left-out item away. (The observed evaluator writes no
// check; only the reviewer's half stays, until the reviewer goes.)
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
	cr, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flat(cr), rubric[0]) {
		t.Error("the checkreview prompt is not shown the narrowing rule")
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
