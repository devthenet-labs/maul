// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestEvaluatePromptTeachesTheGrammarItTripsOn (defect gate 5): the
// evaluator's first drafts were refused in every round for the same few
// shapes: a list or a newline in stdoutContains, a run program that asserts
// nothing on stdout beside its marker, and a browser text matcher written
// beside the assertion instead of under `text`. Each prompt states the shape
// that holds, and the JSON it shows for it validates.
func TestEvaluatePromptTeachesTheGrammarItTripsOn(t *testing.T) {
	render := func(p EvaluatePrompt) string {
		t.Helper()
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatal(err)
		}
		return flat(got)
	}
	slot := testEvaluatePrompt()
	slot.Tools.Browser, slot.Tools.Sandbox = true, true
	for name, tc := range map[string]struct {
		prompt string
		want   []string
	}{
		"cli": {render(testWorkspacePrompt(testCLIWorkspace)), []string{
			"`stdoutContains` is one string on one line, never a list and never with a newline",
			"prints what it printed as one JSON string",
			"`\"expect\": {\"exit\": 0, \"stdoutJSON\": {\"/stdout\": \"kick 4\\nsnare 2\\n\"}}`",
			"a program that prints the marker on a mismatch included",
			"Run your first check file before you write the others",
		}},
		"library": {render(testWorkspacePrompt(testLibraryWorkspace)), []string{
			"`stdoutContains` is one string on one line, never a list and never with a newline",
			"print them as one JSON string and let `stdoutJSON` compare it",
			"a program that prints the marker on a mismatch included",
		}},
		"slot": {render(slot), []string{
			"`stdoutContains` (one string, on one line)",
			"its matcher nested under `text`",
			"never the value of an input or slider",
		}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.prompt, w) {
				t.Errorf("%s prompt does not say %q", name, w)
			}
		}
	}

	// The text step the slot prompt shows, in a check: it validates.
	text := `{"assert": "text", "locator": {"css": "#bpm-out"}, "text": {"equals": "90"}}`
	if !strings.Contains(render(slot), text) {
		t.Fatalf("slot prompt does not show %s", text)
	}
	c := mustCheck(t, `{"id":"bpm-shown","outcome":"bpm","form":"browser","browser":{"steps":[{"goto":"/"},`+
		`{"fill":{"locator":{"label":"Beat name"},"text":"{{run}}-b"}},{"expect":`+text+`}]}}`)
	if err := checks.Validate(c, checks.Context{Preview: &checks.Preview{Paths: []string{"/"}}}); err != nil {
		t.Errorf("the text step the prompt shows is refused: %v", err)
	}
	// The several-lines expect the cli prompt shows validates in a run check.
	c = mustCheck(t, `{"id":"solo-lines","outcome":"solo","form":"run","run":{"language":"go",`+
		`"files":[{"path":"main.go","content":"package main\n// {{bin}} {{marker}}\n"}],"args":["{{run}}"],`+
		`"timeout":"30s","expect":{"exit":0,"stdoutJSON":{"/stdout": "kick 4\nsnare 2\n"}}}}`)
	ws := checks.Context{Workspace: &checks.Workspace{Binary: testCLIWorkspace.Binary}}
	if err := checks.Validate(c, ws); err != nil {
		t.Errorf("the several-lines expect the prompt shows is refused: %v", err)
	}
}
