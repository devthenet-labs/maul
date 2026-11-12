// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The green question, and the sufficiency rule it rests on: a pass shows
// the outcome holds.
const (
	greenQuestion = "if this check passes, does that show the outcome holds?"
	sufficiency   = "if it passed, that would show the outcome holds"
)

// TestCheckReviewRedJudgesTheFailure (gate 2, G-B-BR-3): a red check is
// judged by what its failure proves, never by whether a pass would prove
// the whole outcome. A visibility check on an outcome with a clause the
// browser grammar cannot assert (G10: "inside the 1280x800 viewport") was
// refused four times for that alone. The necessity rules that keep a red
// check from failing a working build stay, every step held to them; the
// green question is unchanged.
func TestCheckReviewRedJudgesTheFailure(t *testing.T) {
	g10 := testCheckReviewPrompt(mustCheck(t, `{"id":"clear","outcome":"clear","form":"browser","browser":{"steps":[`+
		`{"goto":"/"},{"expect":{"assert":"visible","locator":{"css":"#clear"}}},`+
		`{"expect":{"assert":"legible","locator":{"css":"#clear"}}}]}}`))
	g10.Outcome = "At 1280 by 800 the Clear button lies wholly inside the viewport, its label Clear is readable " +
		"against its background and it has a non-zero size."
	g10.ExpectFail, g10.FailedStep, g10.FailedAssertion = true, 3, "expect"

	http := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
	http.ExpectFail, http.FailedStep, http.FailedAssertion = true, 4, "json /players"

	w := testReviewLibrary
	w.BaseUnbuildable, w.Base = true, ReviewRun{}
	w.Head = ReviewRun{Result: string(v1alpha1.CheckRunFail), Assertion: "marker too-long"}
	ws := testWorkspaceReview(t, ExampleLibraryRunCheck, w)
	ws.ExpectFail, ws.FailedStep, ws.FailedAssertion = true, 1, "marker too-long"

	scoped := testSwingReview(mustCheck(t, swingCheck(`{"css":"li","hasText":"{{run}}-beat1"}`, "swing 20%")))

	red := map[string]CheckReviewPrompt{"layout (G10)": g10, "http": http, "workspace": ws, "run-scoped text": scoped}
	for name, p := range red {
		got, err := RenderCheckReviewPrompt(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got = flat(got)
		for _, gone := range []string{greenQuestion, sufficiency} {
			if strings.Contains(got, gone) {
				t.Errorf("%s: the red question still asks %q", name, gone)
			}
		}
		for _, want := range []string{
			"does this check's failure show that the outcome does not hold?",
			"It need not show every part of the outcome",
			"never refuse it only because a pass would leave part of the outcome unshown",
			// The necessity rules, every step held to them (G10's other
			// three refusals: steps the outcome does not promise).
			"It would pass on a build where the outcome holds",
			"at any step, before or after the one that failed,",
			// Setup stays sound: the exemption covers what the outcome
			// promises, never the condition it is stated under.
			"That never excuses a condition the outcome is stated under",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the red question lacks %q", name, want)
			}
		}
	}

	// The layout rubric holds its setup to the outcome too: a check that
	// clicks or scrolls to another state than the outcome names could fail
	// a working build however true its assertion is.
	got, err := RenderCheckReviewPrompt(g10)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"measures something the outcome promises", // the layout branch
		"the steps before it set up the situation the outcome is stated for",
	} {
		if !strings.Contains(flat(got), want) {
			t.Errorf("layout (G10): the red question lacks %q", want)
		}
	}

	for name, p := range map[string]CheckReviewPrompt{
		"http":   testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)),
		"layout": testCheckReviewPrompt(mustCheck(t, ExampleBrowserCheck)),
	} {
		got, err := RenderCheckReviewPrompt(p)
		if err != nil {
			t.Fatalf("green %s: %v", name, err)
		}
		if got = flat(got); !strings.Contains(got, greenQuestion) || strings.Contains(got, "need not show every part") {
			t.Errorf("green %s: the green question changed", name)
		}
	}
}
