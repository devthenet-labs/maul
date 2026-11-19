// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"math/rand"
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
		{"the needle half on stdout and half on stderr", streams(0, run, "-item\n"), containsExit,
			toolwire.ResultError},
		{"the needle on stderr, cut in two by stdout as the shim read them", toolexec.Result{Exit: 0,
			Output: run + "ok\n-item\n", Stdout: ptr("ok\n"), Stderr: ptr(run + "-item\n")}, containsExit,
			toolwire.ResultError},
		{"the needle nowhere, exit 0 expected", streams(0, "nothing\n", "warning\n"), containsExit,
			toolwire.ResultFail},
		{"the needle nowhere, exited 1, no exit expected", streams(1, "", "boom\n"), contains, toolwire.ResultFail},
		{"a shim that keeps no streams apart", old, containsExit, toolwire.ResultError},
		// Code review 2026-11-18 (relay): the shim's own words for a
		// program it could not start are on stderr, never stdout.
		{"the shim could not start the program, the needle in its words", streams(127, "",
			"[maul: could not start /work/bin/"+run+"-item: fork/exec /work/bin/"+run+
				"-item: no such file or directory]"), contains, toolwire.ResultError},
		{"the same from a shim that keeps no streams apart", toolexec.Result{Exit: 127,
			Output: "[maul: could not start /work/bin/" + run + "-item: no such file or directory]"}, contains,
			toolwire.ResultError},
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

func ptr(s string) *string { return &s }

func mustValue(t *testing.T, v any) httpstep.Value {
	t.Helper()
	val, err := httpstep.ValueOf(v)
	if err != nil {
		t.Fatal(err)
	}
	return val
}

// stdoutSeed is TestStdoutJudgedOnStdoutOrNotAtAll's seed, so the gate is
// deterministic.
const stdoutSeed = 20261119

// TestStdoutJudgedOnStdoutOrNotAtAll is a seeded property over both judges
// (code review 2026-11-18, checks 6): for any split of a program's text
// across stdout and stderr, any exit and any expect,
//
//   - a pass means expect's stdout assertions hold on stdout alone, and the
//     exit was the one expect names, or 0 when it names none;
//   - the verdict is the one the same text all on one stream (stdout, then
//     stderr, as a single pipe would hold it) gets, or an error: keeping
//     the streams apart never makes a pass or a fail of its own;
//   - the two judges agree.
func TestStdoutJudgedOnStdoutOrNotAtAll(t *testing.T) {
	const run = "v1a1t1-abc"
	marker := checks.ExecMarker(strings.Repeat("cd", 16))
	needle := run + "-item"
	frags := []string{"", "\n", "x\n", "warn", needle, needle + "\n", "  assert \"" + needle + "\" in body\n",
		`{"n":"` + needle + `"}`, `{"n":"` + needle + `"}` + "\n", `{"n":"other"}`, run, "-item\n", marker + " a: no\n",
		"count=2", "Traceback\n"}
	zero, one := int32(0), int32(1)
	expects := []checks.ExecExpect{
		{StdoutContains: "{{run}}-item"},
		{Exit: &zero, StdoutContains: "{{run}}-item"},
		{Exit: &one, StdoutContains: "{{run}}-item"},
		{StdoutJSON: map[string]httpstep.Value{"/n": mustValue(t, "{{run}}-item")}},
		{Exit: &zero, StdoutJSON: map[string]httpstep.Value{"/n": mustValue(t, "{{run}}-item")}},
	}
	r := rand.New(rand.NewSource(stdoutSeed))
	side := func() string {
		var b strings.Builder
		for range r.Intn(3) {
			b.WriteString(frags[r.Intn(len(frags))])
		}
		return b.String()
	}
	tally := map[toolwire.AttemptResult]int{}
	for i := range 4000 {
		stdout, stderr, exit := side(), side(), r.Intn(3)
		x := expects[r.Intn(len(expects))]
		res := streams(exit, stdout, stderr)
		single := streams(exit, stdout+stderr, "")
		slot, ws := judgeExec(res, x, marker, run)[0], judgeWorkspace(res, nil, x, marker, run, places{})[0]
		was := judgeExec(single, x, marker, run)[0]
		tally[slot.Result]++
		switch {
		case slot.Result != ws.Result:
			t.Fatalf("#%d stdout %q stderr %q exit %d %+v: slot %s, workspace %s", i, stdout, stderr, exit, x,
				slot.Result, ws.Result)
		case slot.Result != was.Result && slot.Result != toolwire.ResultError:
			t.Fatalf("#%d stdout %q stderr %q exit %d %+v: %s, on one stream %s", i, stdout, stderr, exit, x,
				slot.Result, was.Result)
		case slot.Result == toolwire.ResultPass && (!stdoutHolds(stdout, x, needle, run) ||
			x.Exit == nil && exit != 0 || x.Exit != nil && int32(exit) != *x.Exit):
			t.Fatalf("#%d stdout %q stderr %q exit %d %+v: a pass", i, stdout, stderr, exit, x)
		}
	}
	for _, res := range []toolwire.AttemptResult{toolwire.ResultPass, toolwire.ResultFail, toolwire.ResultError} {
		if tally[res] < 200 {
			t.Fatalf("%v: the property is near vacuous", tally)
		}
	}
}
