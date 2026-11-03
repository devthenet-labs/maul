// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/report"
)

// TestEvaluatePromptJudgesAsWorded (S1 demo bug 13): the evaluator saw a
// keep outcome fail as worded (the exact GET body posted back was a 400),
// called it pre-existing, saved a check that left the failing field out and
// reported pass. The prompt now says an outcome is judged as worded, that a
// failure from before the change is still a failure, that nothing the
// outcome names is dropped for a green run, and that the seqs where it
// failed go in the verdict's contrary, which withholds a pass; a dispute is
// never for a failure the change did not cause.
func TestEvaluatePromptJudgesAsWorded(t *testing.T) {
	got, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**Judge each outcome as it is worded.**",
		"it does not hold, whether or not this change caused it",
		"a failure that was there before the change is still a failure of the outcome",
		"for a keep outcome it is exactly what the outcome is there to catch",
		"Such an outcome is never `pass`, and is never re-checked on a narrower claim",
		"report it `fail` with a check that shows the defect if one meets the rules under **A check that shows a " +
			"defect**, else `unsure`, and say in its note what failed",
		"Never drop, replace or default an input, a field, a step or a value the outcome names",
		"A call that failed by your own mistake",
		"List in the verdict's `contrary` the ledger seqs where you saw the outcome as worded not hold",
		"maul passes no outcome whose verdict lists any",
		"`contrary`, the seqs where you saw the outcome as worded not hold",
		"A failure that was there before this change is still a defect: never dispute a check for that.",
	} {
		if !strings.Contains(flat(got), want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(flat(got), "not a defect of the change") {
		t.Error("the prompt still lets a dispute call a failure not a defect of the change")
	}
	e, err := report.ParseEvaluation([]byte(reportExampleOf(t, got)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(report.FrontmatterKeyPaths([]byte(ExampleEvaluationReport)), " "),
		"verdicts[].contrary") || !hasContrary(e) {
		t.Errorf("the report example shows no contrary seqs:\n%s", ExampleEvaluationReport)
	}
}

func hasContrary(e *report.Evaluation) bool {
	for _, v := range e.Verdicts {
		if len(v.Contrary) > 0 && v.Verdict != report.VerdictPass {
			return true
		}
	}
	return false
}

// TestGreenRubricRefusesANarrowedCheck (S1 demo bug 13): the reviewer
// answered proves for a check that posted a subset of the body the outcome
// said to post back exactly. The green rule now names that: a check leaving
// out, replacing or altering anything the outcome's words name tests less
// than the outcome, even on a reasonable reading.
func TestGreenRubricRefusesANarrowedCheck(t *testing.T) {
	for _, want := range []string{
		"A check that leaves out, replaces or alters anything the outcome's words name",
		"a field of a body it says to send, a step, a value, a page or a surface",
		"tests less than the outcome: answer `does_not_prove`, even if what it does test is a reasonable reading " +
			"of the outcome",
	} {
		if !strings.Contains(GreenRubric, want) {
			t.Errorf("the green rule lacks %q", want)
		}
	}
	got, err := RenderCheckReviewPrompt(testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flat(got), "even if what it does test is a reasonable reading of the outcome") {
		t.Error("a green review is not shown the narrowed-check rule")
	}
}
