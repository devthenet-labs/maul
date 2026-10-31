// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"regexp"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/report"
)

// reportExampleOf is the report example a rendered evaluate prompt shows:
// the fenced markdown block under "## Your report".
func reportExampleOf(t *testing.T, prompt string) string {
	t.Helper()
	_, section, ok := strings.Cut(prompt, "## Your report")
	if !ok {
		t.Fatal("the prompt has no report section")
	}
	_, block, ok := strings.Cut(section, "```markdown\n")
	if !ok {
		t.Fatal("the report section shows no markdown example")
	}
	example, _, ok := strings.Cut(block, "\n```\n")
	if !ok {
		t.Fatal("the report example is not closed")
	}
	return example + "\n"
}

// TestEvaluatePromptReportExampleDecodes (S1 bug 5): the example the
// rendered prompt shows decodes with the real strict decoder, names each
// verdict's outcome as outcome (the key proposed uses too) and each
// proposal by its file, so the prompt teaches exactly the schema.
func TestEvaluatePromptReportExampleDecodes(t *testing.T) {
	got, err := RenderEvaluatePrompt(testEvaluatePrompt())
	if err != nil {
		t.Fatal(err)
	}
	example := reportExampleOf(t, got)
	if example != ExampleEvaluationReport {
		t.Errorf("the prompt's example is not ExampleEvaluationReport:\n%s", example)
	}
	e, err := report.ParseEvaluation([]byte(example))
	if err != nil {
		t.Fatalf("the prompt's report example is refused: %v", err)
	}
	if len(e.Verdicts) == 0 || len(e.Proposed) == 0 || e.Proposed[0].File == "" {
		t.Errorf("example = %+v, want verdicts and a proposal by its file", e)
	}
	if regexp.MustCompile(`(?m)^\s*-?\s*id:\s*"` + e.Verdicts[0].ID + `"`).MatchString(example) {
		t.Errorf("the example names a verdict's outcome as id, not outcome:\n%s", example)
	}
	for _, key := range []string{"verdicts[].outcome", "verdicts[].verdict", "verdicts[].ledger",
		"proposed[].outcome", "proposed[].file"} {
		if !strings.Contains(strings.Join(report.FrontmatterKeyPaths([]byte(example)), " "), key) {
			t.Errorf("the example lacks %s", key)
		}
	}
}

// TestCheckReviewPromptReportExampleDecodes (S1 bug 8): the example the
// rendered reviewer prompt shows, red question or not, is
// ExampleCheckReviewReport, closed by its second fence, and decodes with the
// real decoder; the prompt says both fence lines are required.
func TestCheckReviewPromptReportExampleDecodes(t *testing.T) {
	for _, red := range []bool{false, true} {
		p := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
		if red {
			p.ExpectFail, p.FailedStep, p.FailedAssertion = true, 4, "json /players"
		}
		got, err := RenderCheckReviewPrompt(p)
		if err != nil {
			t.Fatal(err)
		}
		example := reportExampleOf(t, got)
		if example != ExampleCheckReviewReport {
			t.Errorf("red %v: the prompt's example is not ExampleCheckReviewReport:\n%s", red, example)
		}
		c, err := report.ParseCheckReview([]byte(example))
		if err != nil || c.Answer != report.AnswerProves || c.Reason == "" {
			t.Errorf("red %v: the prompt's report example decodes to %+v, %v", red, c, err)
		}
		if strings.Count(example, "---\n") != 2 || !strings.Contains(got, "both lines are required") {
			t.Errorf("red %v: the example or the prompt does not close the frontmatter:\n%s", red, got)
		}
	}
}

// TestCheckReviewPromptRedQuestion: a check proposed to show a defect is
// asked the red question, naming the step that failed; a green one never
// is.
func TestCheckReviewPromptRedQuestion(t *testing.T) {
	p := testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck))
	green, err := RenderCheckReviewPrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	p.ExpectFail, p.FailedStep, p.FailedAssertion = true, 4, "json /players"
	red, err := RenderCheckReviewPrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"This check failed on the build under review",
		"The application's own state was not fresh",
		"at step 4, on its assertion `json /players`",
		"It would pass on a build where the outcome holds",
		"That assertion, `json /players` of step 4, asserts the outcome itself",
		"about data the check itself created"} {
		if !strings.Contains(red, want) {
			t.Errorf("the red prompt lacks %q", want)
		}
		if strings.Contains(green, want) {
			t.Errorf("the green prompt carries %q", want)
		}
	}
}

// reportKeyLine is a line that reads as a key of the report's frontmatter.
var reportKeyLine = regexp.MustCompile(`^\s*(- )?(id|outcome|kind|manual|verdict|ledger|note|result|why|check|` +
	`digest|file|from the request|why a person judges it):`)

// TestEvaluatePromptDataIsNotReportSyntax (S1 bug 5): the outcomes and the
// previous round are listed in prose, never as YAML whose keys a model
// carries into its report (the live evaluators wrote `outcome:` from the
// outcome list into every verdict); every outcome's id, text, kind and
// request line is still there.
func TestEvaluatePromptDataIsNotReportSyntax(t *testing.T) {
	p := testEvaluatePrompt()
	p.Round, p.PriorRound = 2, 1
	p.PriorVerdicts = []PriorVerdict{{Outcome: "winner-rises", Result: "failed", Reason: "the order did not change"}}
	p.PriorReplay = []PriorReplay{{Check: "winner-rises.api", Outcome: "winner-rises", Result: "fail"}}
	got, err := RenderEvaluatePrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"### The approved outcomes", "### The previous round"} {
		_, rest, _ := strings.Cut(got, section)
		block, _, _ := strings.Cut(rest, "\n##")
		for _, line := range strings.Split(block, "\n") {
			if reportKeyLine.MatchString(line) {
				t.Errorf("%s: line %q reads as report syntax", section, line)
			}
		}
	}
	for _, want := range []string{"1. winner-rises (a change)",
		"Rating a battle moves the winner above the loser on the leaderboard.",
		"From the request, quoted: ratings should update after every battle",
		"2. leaderboard-still-loads (a keep)", "3. sounds-good (a change, judged by a person)",
		"Why a person judges it, quoted: only a person can judge how it sounds",
		"- winner-rises was judged failed. Why, quoted: the order did not change",
		"- check winner-rises.api, of outcome winner-rises, replayed fail"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

// TestCheckReviewPromptExecMarker (second review, 1a): an exec check's
// failure is its program's own word (the marker), so the reviewer is shown
// which marker it printed and asked whether the program prints it only when
// the approved outcome is violated, never on a transport or status error.
func TestCheckReviewPromptExecMarker(t *testing.T) {
	p := testCheckReviewPrompt(mustCheck(t, ExampleExecCheck))
	p.ExpectFail, p.FailedStep, p.FailedAssertion = true, 1, "marker live-score"
	got, err := RenderCheckReviewPrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(got), " ")
	for _, want := range []string{"`MAUL-ASSERTION-FAILED: live-score:`",
		"prints it only when the approved outcome is violated",
		"never on a connection or transport error, a status code, a timeout, an exception or bad input"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the exec red prompt lacks %q:\n%s", want, got)
		}
	}
}
