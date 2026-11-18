// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// streams is a program's result as a shim that keeps its streams apart
// answers it: Output the two together, stdout first.
func streams(exit int, stdout, stderr string) toolexec.Result {
	return toolexec.Result{Exit: exit, Output: stdout + stderr, Stdout: &stdout, Stderr: &stderr}
}

// onStdout is a program's result that printed out on stdout alone.
func onStdout(out string) toolexec.Result { return streams(0, out, "") }

// TestStdoutAssertionsReadStdout (code review 2026-11-18, checks 6):
// stdoutContains and stdoutJSON are judged on the program's stdout, never
// on text it printed on stderr (a traceback echoing its source line, a
// usage error echoing an argument), and with no exit expected a run that
// exited non-zero never passes. Each run judged otherwise than it was
// before (on stdout and stderr together, any exit) is an error, never a
// pass nor a fail: a check saved under the old judgement leads only to
// unsure. Both judges, the slot's and the workspace's. %M is the attempt's
// marker.
func TestStdoutAssertionsReadStdout(t *testing.T) {
	const run = "v1a1t1-abc"
	marker := checks.ExecMarker(strings.Repeat("ab", 16))
	zero := int32(0)
	contains := checks.ExecExpect{StdoutContains: "{{run}}-item"}
	containsExit := checks.ExecExpect{Exit: &zero, StdoutContains: "{{run}}-item"}
	jsonExit := checks.ExecExpect{Exit: &zero, StdoutJSON: map[string]httpstep.Value{"/n": mustValue(t, "{{run}}-item")}}
	traceback := "Traceback (most recent call last):\n  File \"c.py\", line 3\n    assert \"" + run +
		"-item\" in body\nAssertionError\n"
	old := toolexec.Result{Exit: 0, Output: run + "-item\n"} // a shim that keeps no streams apart
	tests := []struct {
		name   string
		res    toolexec.Result
		x      checks.ExecExpect
		result toolwire.AttemptResult
	}{
		{"a traceback echoes the needle on stderr, no exit expected", streams(1, "", traceback), contains,
			toolwire.ResultError},
		{"a usage error echoes it on stderr, exit 0 expected", streams(0, "", "unknown command \""+run+"-item\"\n"),
			containsExit, toolwire.ResultError},
		{"the needle on stdout, exited 1, no exit expected", streams(1, "created "+run+"-item\n", ""), contains,
			toolwire.ResultError},
		{"the needle on stdout, exited 0, no exit expected", streams(0, "created "+run+"-item\n", ""), contains,
			toolwire.ResultPass},
		{"the needle on stdout, a warning on stderr", streams(0, run+"-item\n", "warning: slow\n"), containsExit,
			toolwire.ResultPass},
		{"json on stdout, a warning on stderr", streams(0, `{"n":"`+run+`-item"}`, "warning: slow\n"), jsonExit,
			toolwire.ResultError},
		{"json on stdout alone", streams(0, `{"n":"`+run+`-item"}`+"\n", ""), jsonExit, toolwire.ResultPass},
		{"the needle nowhere, exit 0 expected", streams(0, "nothing\n", "warning\n"), containsExit,
			toolwire.ResultFail},
		{"the needle nowhere, exited 1, no exit expected", streams(1, "", "boom\n"), contains, toolwire.ResultFail},
		{"a shim that keeps no streams apart", old, containsExit, toolwire.ResultError},
		{"the marker starts a stderr line after an open stdout line", streams(0, "count=2", "%M a: no\n"),
			containsExit, toolwire.ResultError},
		{"the marker starts a stdout line, stderr left open", streams(0, "%M a: no\n", "warn"), containsExit,
			toolwire.ResultError},
		{"the marker starts a stderr line, both streams closed", streams(0, run+"-item\n", "%M a: no\n"),
			containsExit, toolwire.ResultFail},
		{"the marker on stdout, stderr empty", streams(1, "%M a: no\n", ""), containsExit, toolwire.ResultFail},
		{"the marker mid-line in its own stream", streams(0, run+"-item\n", "warn %M a: no\n"), containsExit,
			toolwire.ResultError},
	}
	for _, tt := range tests {
		res := tt.res
		withMarker := func(s *string) *string {
			if s == nil {
				return nil
			}
			v := strings.ReplaceAll(*s, "%M", marker)
			return &v
		}
		res.Output = strings.ReplaceAll(res.Output, "%M", marker)
		res.Stdout, res.Stderr = withMarker(res.Stdout), withMarker(res.Stderr)
		for judge, steps := range map[string][]toolwire.StepRecord{
			"slot":      judgeExec(res, tt.x, marker, run),
			"workspace": judgeWorkspace(res, nil, tt.x, marker, run, places{}),
		} {
			if got := steps[0]; got.Result != tt.result {
				t.Errorf("%s, %s: %+v, want %s", judge, tt.name, got, tt.result)
			} else if got.Result == toolwire.ResultError && (got.Assertion || strings.Contains(got.Detail, marker)) {
				t.Errorf("%s, %s: an error by assertion or showing the marker: %+v", judge, tt.name, got)
			}
		}
	}
}

func mustValue(t *testing.T, v any) httpstep.Value {
	t.Helper()
	val, err := httpstep.ValueOf(v)
	if err != nil {
		t.Fatal(err)
	}
	return val
}
