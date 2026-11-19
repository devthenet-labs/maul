// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestSeveralLinesAsOneJSONString pins what the evaluate prompt teaches for
// several lines of plain output (defect gate 5): a run check's program
// prints them as one JSON string, and stdoutJSON holds it to the whole
// text. The same output passes; other lines after the expected exit fail by
// the check's assertion, as a change outcome's check must fail at the base.
func TestSeveralLinesAsOneJSONString(t *testing.T) {
	c, err := checks.Parse([]byte(`{"id":"solo-lines","outcome":"solo","form":"run","run":{"language":"go",` +
		`"files":[{"path":"main.go","content":"package main\n// {{bin}} {{marker}}\n"}],"args":["{{run}}"],` +
		`"timeout":"30s","expect":{"exit":0,"stdoutJSON":{"/stdout":"kick 4\nsolo hat 4\n"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := checks.Validate(c, checks.Context{Workspace: &checks.Workspace{Binary: "/work/bin/bbcount"}}); err != nil {
		t.Fatalf("the taught shape is refused: %v", err)
	}
	for _, tc := range []struct {
		name, output string
		want         toolwire.AttemptResult
		asserted     bool
	}{
		{"the same lines", `{"stdout":"kick 4\nsolo hat 4\n"}`, toolwire.ResultPass, false},
		{"other lines", `{"stdout":"kick 4\n"}`, toolwire.ResultFail, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := judgeOutput(onStdout(tc.output), c.Run.Expect, "MARK-x", "r1", places{})
			if steps[0].Result != tc.want || steps[0].Assertion != tc.asserted {
				t.Errorf("judged %+v, want %s with assertion %v", steps[0], tc.want, tc.asserted)
			}
		})
	}
}
