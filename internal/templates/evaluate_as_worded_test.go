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
// outcome names is dropped to see it hold, and that any call showing it not
// hold makes its verdict fail or unsure, never pass (the observed
// evaluator has no contrary list: the rule is the evaluator's, and the
// cited calls are shown to the reviewers).
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
		"Never drop, replace or default an input, a field, a step or a value the outcome names",
		"A call that failed by your own mistake",
		"If any call you made showed the outcome as worded not hold, its verdict is `fail`, citing that call, or " +
			"`unsure`: never `pass`, whatever your other calls showed and whatever you believe caused it.",
	} {
		if !strings.Contains(flat(got), want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(flat(got), "not a defect of the change") {
		t.Error("the prompt still lets a dispute call a failure not a defect of the change")
	}
	if _, err := report.ParseEvaluation([]byte(reportExampleOf(t, got))); err != nil {
		t.Fatal(err)
	}
}
