// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// g1HalfRuleSplit is gate 4's G1 red check (bbcount-102, half-rule-split),
// as the check reviewer was handed it: the binary run on its own pattern,
// its stdout held to the line the outcome promises.
const g1HalfRuleSplit = `{"id":"half-rule-split","outcome":"half-rule-split","form":"exec","exec":{
 "argv":["{{bin}}","-half","p.json"],"files":[{"path":"p.json","content":"{\"name\":\"x\",\"author\":\"a\",` +
	`\"bpm\":90,\"swing\":0,\"tracks\":{\"kick\":[false,true,true,false,true,false,false,false,` +
	`false,false,false,true,false,false,false,false]}}\n"}],"timeout":"30s",` +
	`"expect":{"exit":0,"stdoutContains":"half kick 3 1"}}}`

// g1Output is what the build under review printed for it (gate 4, G1
// round 2, ledger seq 7): A and B swapped.
const g1Output = "kick 4\nsnare 1\nhat 1\ntotal 6\nhalf kick 1 3\nhalf snare 0 1\nhalf hat 1 0\n"

// heldToTheReviewersBounds fails t unless detail is within the ledger's
// bound, UTF-8, and holds none of the attempts' markers. (The retired check
// reviewer's own bound, agentrun.ValidFailure, is gone with it; this
// runner goes in W8.)
func heldToTheReviewersBounds(t *testing.T, detail string, markers []string) {
	t.Helper()
	if len(detail) > 2<<10 || !utf8.ValidString(detail) {
		t.Errorf("detail (%d bytes) is over the ledger's bound or not UTF-8", len(detail))
	}
	for _, m := range markers {
		if strings.Contains(detail, m) {
			t.Errorf("detail %q holds an attempt's marker", detail)
		}
	}
}

// TestWorkspaceFailKeepsTheProgramsOutput (G4-B-CLI-1): a cli exec or
// library run check that fails without its marker keeps, beside maul's
// words, the exit code and the end of what the program printed, quoted on
// one line, so the check reviewer sees what the build printed. In G1 the
// reviewer refused every red check because maul recorded no output. The
// verdict and its assertion are unchanged.
func TestWorkspaceFailKeepsTheProgramsOutput(t *testing.T) {
	long := strings.Repeat("noise \"quoted\"\t\x01 line\n", 2000) + "half kick 1 3\n"
	// The longest detail a miss can record: a pointer and a value of
	// characters each quoted as six bytes, and a long output of them.
	wide := strings.Repeat("\u0085", 63) + "x"
	worstJSON := `{"id":"w.json","outcome":"w","form":"exec","exec":{"argv":["{{bin}}"],
 "expect":{"exit":0,"stdoutJSON":{"/` + wide + `":"ok"}}}}`
	worstOut := `{"` + wide + `":"` + strings.Repeat("\u0085", 200) + `","pad":"` +
		strings.Repeat("\u0085", 2000) + `"}` + "\n"
	const halfJSON = `{"id":"half.json","outcome":"half","form":"exec","exec":{"argv":["{{bin}}","-json"],
 "expect":{"exit":0,"stdoutJSON":{"/half/kick":[3,1]}}}}`
	for _, tt := range []struct {
		name, check, out string
		exit             int
		id               string
		want             []string
	}{
		{"G1: the expected line is missing", g1HalfRuleSplit, g1Output, 0, "stdout", []string{
			"the output does not contain the expected text", "exit 0",
			`"kick 4\nsnare 1\nhat 1\ntotal 6\nhalf kick 1 3\nhalf snare 0 1\nhalf hat 1 0\n"`}},
		{"a long hostile output keeps its end", g1HalfRuleSplit, long, 0, "stdout", []string{
			"the output does not contain the expected text", "exit 0", `line\nhalf kick 1 3\n"`, "the end of"}},
		{"the program printed nothing", g1HalfRuleSplit, "", 0, "stdout", []string{
			"exit 0", "the program printed nothing"}},
		{"a run token printed", g1HalfRuleSplit, "seen %R\n", 0, "stdout", []string{`"seen %R\n"`}},
		{"a json value differs", halfJSON, `{"half":{"kick":[1,3]}}` + "\n", 0, "stdout", []string{
			`stdoutJSON "/half/kick" did not match; the value there is "[1,3]"`, "exit 0",
			`"{\"half\":{\"kick\":[1,3]}}\n"`}},
		// The tail of a long document can miss the value: it is kept apart.
		{"a json value differs early in a long document", halfJSON,
			`{"half":{"kick":[1,3]},"pad":"` + strings.Repeat("x", 4000) + `"}` + "\n", 0, "stdout", []string{
				`stdoutJSON "/half/kick" did not match; the value there is "[1,3]"`, "the end of"}},
		{"a wrong exit", g1HalfRuleSplit, "usage: bbcount [-half] file\n", 2, "", []string{
			"exit 2, expected 0", `"usage: bbcount [-half] file\n"`}},
		{"a library run check", cliRunCheck, "count=2\n", 0, "stdout", []string{
			"the output does not contain the expected text", "exit 0", `"count=2\n"`}},
		{"the longest detail a miss records", worstJSON, worstOut, 0, "stdout", []string{
			"did not match; the value there is", "exit 0", "the program printed (untrusted, the end of its output)"}},
		// A marker that starts no line errs instead, its detail holding no
		// marker either: TestMarkerUnplacedIsAnError.
	} {
		t.Run(tt.name, func(t *testing.T) {
			var markers []string
			marker := ""
			head := &fakeSandbox{do: func(req *toolexec.Request, _ int) (toolexec.Result, error) {
				if req.Kind == toolexec.KindBuild {
					return toolexec.Result{}, nil
				}
				for _, f := range req.Files {
					if m := attemptMarkerPattern.FindString(string(f.Content)); m != "" {
						marker, markers = m, append(markers, m)
					}
				}
				out := strings.ReplaceAll(tt.out, "%R", req.Run)
				if marker != "" {
					out = strings.ReplaceAll(out, "%M", marker)
				}
				return toolexec.Result{Output: out, Exit: tt.exit}, nil
			}}
			h := newWorkspaceHarness(t, wsConfig(), head, &fakeSandbox{})
			_, resp := h.callKeyed(toolwire.VerbCheckRun, mustJSON(t, toolwire.CheckRunRequest{Spec: json.RawMessage(tt.check)}))
			if resp.Refused != "" {
				t.Fatalf("dry run refused: %+v", resp)
			}
			for _, a := range resp.Observed.CheckRun.Attempts {
				s := a.Steps[len(a.Steps)-1]
				if a.Result != toolwire.ResultFail || s.AssertionID != toolwireID(tt.id) || s.Assertion != (tt.id != "") {
					t.Fatalf("attempt %+v, want a fail with assertion %q", a, tt.id)
				}
				for _, w := range tt.want {
					if w = strings.ReplaceAll(w, "%R", a.Run); !strings.Contains(s.Detail, w) {
						t.Errorf("detail %q lacks %q", s.Detail, w)
					}
				}
				heldToTheReviewersBounds(t, s.Detail, markers)
			}
		})
	}
}

// TestSlotExecFailKeepsTheProgramsOutput (G4-B-CLI-1): a preview's exec
// check that fails without its marker keeps the exit code and the end of
// what the program printed in the slot's sandbox too; such a fail is never
// an assertion there, and stays none.
func TestSlotExecFailKeepsTheProgramsOutput(t *testing.T) {
	for _, tt := range []struct {
		name, out string
		exit      int
		want      []string
	}{
		{"the expected text is missing", "PENDING\n", 0, []string{
			"the output does not contain the expected text", "exit 0", `"PENDING\n"`}},
		{"a wrong exit", "Traceback: boom\n", 1, []string{"exit 1, expected 0", `"Traceback: boom\n"`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var markers []string
			ex := &fakeExec{run: func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
				m := attemptMarker(t, req)
				markers = append(markers, m)
				return toolexec.Result{Exit: tt.exit, Output: strings.ReplaceAll(tt.out, "%M", m)}, nil
			}}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail {
				t.Fatalf("result %s, want fail (%+v)", rec.Result, rec)
			}
			for _, a := range rec.Attempts {
				s := a.Steps[0]
				if s.Assertion {
					t.Errorf("step %+v is an assertion; a slot exec fail without its marker is none", s)
				}
				for _, w := range tt.want {
					if !strings.Contains(s.Detail, w) {
						t.Errorf("detail %q lacks %q", s.Detail, w)
					}
				}
				heldToTheReviewersBounds(t, s.Detail, markers)
			}
		})
	}
}
