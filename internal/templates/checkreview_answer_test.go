// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestCheckReviewAnswerIsTheFile (L6-B-LIB-1, defect gate 5): reviewers
// ended 9 of 11 sessions with their answer as reply text and no report file,
// costing a repair turn per check. Every review says, before the outcome and
// as its last line, that only the file counts and the reply text is ignored.
func TestCheckReviewAnswerIsTheFile(t *testing.T) {
	red := testCheckReviewPrompt(mustCheck(t, ExampleBrowserCheck))
	red.ExpectFail, red.FailedStep, red.FailedAssertion = true, 11, "expect"
	for name, p := range map[string]CheckReviewPrompt{
		"green http":  testCheckReviewPrompt(mustCheck(t, ExampleHTTPCheck)),
		"red browser": red,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RenderCheckReviewPrompt(p)
			if err != nil {
				t.Fatal(err)
			}
			head, _, ok := strings.Cut(got, "## The outcome and the check")
			if !ok {
				t.Fatal("no outcome section")
			}
			for _, want := range []string{"`" + p.ReportPath + "`", "with the Write tool",
				"maul reads only that file", "the text of your reply is ignored"} {
				if !strings.Contains(flat(head), want) {
					t.Errorf("the opening does not say %q:\n%s", want, head)
				}
			}
			lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
			last := lines[len(lines)-1]
			for _, want := range []string{"only as that file", "the text of your reply is ignored", "Write tool"} {
				if !strings.Contains(last, want) {
					t.Errorf("the last line %q does not say %q", last, want)
				}
			}
		})
	}
}
