// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestMarkerUnplacedIsAnError (code review 2026-11-18, relay 1): a run
// whose output carries its attempt's marker with no line starting with it
// (after output with no trailing newline, a space, a carriage return, inside
// a JSON value, a source line echoed) errs in both judges, whatever its exit
// and its expect: the program may have said the outcome does not hold, so it
// is never a pass, and maul cannot tell, so it is never a fail. The detail
// says how to fix the program and never holds the marker. %M is the
// attempt's marker.
func TestMarkerUnplacedIsAnError(t *testing.T) {
	const jsonCheck = `{"id":"m.json","outcome":"m","form":"exec","exec":{"argv":["{{bin}}","m.txt"],
 "files":[{"path":"m.txt","content":"{{marker}}\n"}],"expect":{"exit":0,"stdoutJSON":{"/m":"ok"}}}}`
	tests := []struct {
		name, check, out string
		exit             int
	}{
		{"after output with no newline, the expected text there", cliRunCheck, "ok%M a: no\n", 0},
		{"after output with no newline, the expected text missing", cliRunCheck, "count=2 %M\n", 0},
		{"after a space", cliRunCheck, "ok\n %M a: no\n", 0},
		{"after a carriage return", cliRunCheck, "ok\r%M a: no\n", 0},
		{"inside a json value", jsonCheck, `{"m":"%M"}` + "\n", 0},
		{"a source line echoed, exit 1", cliRunCheck, "  print('%M a: no')\nTraceback\n", 1},
	}
	for _, tt := range tests {
		t.Run("workspace "+tt.name, func(t *testing.T) {
			var markers []string
			m := ""
			head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				for _, f := range req.Files {
					if found := attemptMarkerPattern.FindString(string(f.Content)); found != "" {
						m, markers = found, append(markers, found)
					}
				}
				// The build, and a run check's go work init and go build.
				if req.Kind == toolexec.KindBuild || req.Argv[0] == "go" {
					return toolexec.Result{}, nil
				}
				if m == "" {
					t.Fatalf("no marker in the program's files")
				}
				return toolexec.Result{Output: strings.ReplaceAll(tt.out, "%M", m), Exit: tt.exit}, nil
			}}
			h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
			_, resp := h.callKeyed(toolwire.VerbCheckRun, mustJSON(t, toolwire.CheckRunRequest{Spec: json.RawMessage(tt.check)}))
			if resp.Refused != "" {
				t.Fatalf("dry run refused: %+v", resp)
			}
			rec := resp.Observed.CheckRun
			if rec.Result != toolwire.RunError {
				t.Fatalf("result %s, want error (%+v)", rec.Result, rec)
			}
			for _, a := range rec.Attempts {
				s := a.Steps[len(a.Steps)-1]
				if a.Result != toolwire.ResultError || s.Assertion || s.Cause != "" || s.Detail != markerUnplaced {
					t.Fatalf("attempt %+v, want an error with no assertion, no cause and the unplaced detail", a)
				}
				heldToTheReviewersBounds(t, s.Detail, markers)
			}
		})
		t.Run("slot "+tt.name, func(t *testing.T) {
			ex := &fakeExec{run: func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
				return toolexec.Result{Exit: tt.exit, Output: strings.ReplaceAll(tt.out, "%M", attemptMarker(t, req))}, nil
			}}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunError {
				t.Fatalf("result %s, want error (%+v)", rec.Result, rec)
			}
			for _, a := range rec.Attempts {
				if s := a.Steps[0]; s.Assertion || s.Detail != markerUnplaced {
					t.Fatalf("step %+v, want an error with no assertion and the unplaced detail", s)
				}
			}
		})
	}
}

// TestMarkerUnplacedThenCleanIsNoPass: an attempt that errs on an
// unplaced marker holds the run off a pass even when the retry is clean.
func TestMarkerUnplacedThenCleanIsNoPass(t *testing.T) {
	ex := &fakeExec{run: func(req *toolwire.ExecRequest, n int) (toolexec.Result, error) {
		if n == 0 {
			return toolexec.Result{Output: "x" + attemptMarker(t, req) + " a: no\nDONE\n"}, nil
		}
		return toolexec.Result{Output: "DONE\n"}, nil
	}}
	h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
	rec := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
	if rec.Result == toolwire.RunPass || len(rec.Attempts) != 2 {
		t.Fatalf("result %s after %d attempts, want no pass after two (%+v)", rec.Result, len(rec.Attempts), rec)
	}
}

// markerSeed is TestMarkerAnywhereIsNeverAPass's seed, so the gate is
// deterministic.
const markerSeed = 20261118

// TestMarkerAnywhereIsNeverAPass is a seeded property over both judges:
// for any output, a marker that starts a line fails the run (red by
// assertion exactly when ExecAssertion names an id), a marker anywhere else
// errs it, and only an output without the marker can pass. The expect is
// met by every output (exit 0, "DONE" in it), so nothing but the marker
// stands between the run and a pass.
func TestMarkerAnywhereIsNeverAPass(t *testing.T) {
	marker := checks.ExecMarker("0123456789abcdef0123456789abcdef")
	frags := []string{"DONE", "\n", "\r", " ", "\t", `{"bpm":150}`, "count=3", "a: x", " a: no", ":",
		"MAUL-ASSERTION-FAILED: a: x", checks.ExecMarker("ffffffffffffffffffffffffffffffff"), marker, marker}
	exit := int32(0)
	x := checks.ExecExpect{Exit: &exit, StdoutContains: "DONE"}

	var out string
	ex := &fakeExec{run: func(*toolwire.ExecRequest, int) (toolexec.Result, error) {
		return toolexec.Result{Output: out}, nil
	}}
	h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
	r := rand.New(rand.NewSource(markerSeed))
	for i := range 400 {
		var b strings.Builder
		for range 1 + r.Intn(8) {
			b.WriteString(frags[r.Intn(len(frags))])
		}
		b.WriteString("\nDONE\n")
		out = b.String()

		marked, unplaced := checks.ExecMarked(out, marker), checks.ExecMarkerUnplaced(out, marker)
		if marked && unplaced || (marked || unplaced) != strings.Contains(out, marker) {
			t.Fatalf("#%d %q: marked %v, unplaced %v", i, out, marked, unplaced)
		}
		_, asserted := checks.ExecAssertion(out, marker)

		ws := judgeOutput(onStdout(out), x, marker, "r1", places{})[0]
		// The slot judge draws its own marker per attempt: put it where
		// the property's marker is.
		slotOut := out
		ex.run = func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
			return toolexec.Result{Output: strings.ReplaceAll(slotOut, marker, attemptMarker(t, req))}, nil
		}
		slot := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
		_, slotAsserted := slot.AssertionStep()

		switch {
		case marked:
			if ws.Result != toolwire.ResultFail || ws.Assertion != asserted {
				t.Fatalf("#%d %q: workspace %+v, want a fail, by assertion %v", i, out, ws, asserted)
			}
			if slot.Result != toolwire.RunFail || slotAsserted != asserted {
				t.Fatalf("#%d %q: slot %+v, want a fail, by assertion %v", i, out, slot, asserted)
			}
		case unplaced:
			if ws.Result != toolwire.ResultError || ws.Assertion {
				t.Fatalf("#%d %q: workspace %+v, want an error", i, out, ws)
			}
			if slot.Result != toolwire.RunError || slotAsserted {
				t.Fatalf("#%d %q: slot %+v, want an error", i, out, slot)
			}
		default:
			if ws.Result != toolwire.ResultPass || slot.Result != toolwire.RunPass {
				t.Fatalf("#%d %q: workspace %+v, slot %+v, want both to pass", i, out, ws, slot)
			}
		}
	}
}
