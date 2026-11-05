// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// A check that shows a defect (the post-demo decision on S1 bug 7): only a
// red that is an assertion of a check that otherwise ran may support a
// fail. These tests pin where tools draws that line.

// TestHTTPAssertionFailures: an http check's unmet expectation is an
// assertion only on content the application answered with (second review,
// 1b, conservative): a JSON pointer that resolves to a different value, a
// header that is present with a different value, body text missing from a
// non-HTML answer. No status is one, a 500 included (it can be the check's
// own malformed request); nor a JSON pointer or header that resolves to
// nothing (the http "locator found nothing"), a non-JSON body where JSON
// was expected (an SPA's HTML fallback), no answer, an unbuilt request or a
// missing capture.
func TestHTTPAssertionFailures(t *testing.T) {
	t.Run("json out of range", func(t *testing.T) {
		api := fakeAPI(t, func(int) int { return 99 })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeDryRun)
		if step, ok := rec.AssertionStep(); !ok || step != 1 || rec.FailedAssertionID() != "json /score" {
			t.Fatalf("AssertionStep() = %d, %v, %q (%+v); want step 1, json /score", step, ok, rec.FailedAssertionID(),
				rec)
		}
	})
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
		assertion   bool
	}{
		{"a 500", http.StatusInternalServerError, "", "", false},
		{"a success not expected", http.StatusOK, "application/json", `{"name":"x"}`, false},
		{"another json value", http.StatusCreated, "application/json", `{"name":"someone-else"}`, true},
		{"a json pointer that resolves to nothing", http.StatusCreated, "application/json", `{"other":1}`, false},
		{"a 404", http.StatusNotFound, "", "", false},
		{"a 405", http.StatusMethodNotAllowed, "", "", false},
		{"a 400", http.StatusBadRequest, "", "", false},
		{"a gateway's 502", http.StatusBadGateway, "", "", false},
		{"an unavailable 503", http.StatusServiceUnavailable, "", "", false},
		{"html where json was expected", http.StatusCreated, "text/html", "<!doctype html><div id=app></div>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
			rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail {
				t.Fatalf("got %+v", rec)
			}
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
		})
	}
	t.Run("body text in an html page", func(t *testing.T) {
		const check = `{"id":"page.text","outcome":"page","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/api/x?r={{run}}"},"expect":{"status":200,"bodyContains":"Elo"}}]}}`
		api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html>"))
		})
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		rec := h.relay.runCheck(context.Background(), parseCheck(t, check), toolwire.ModeDryRun)
		if _, ok := rec.AssertionStep(); rec.Result != toolwire.RunFail || ok {
			t.Errorf("an HTML page missing a text is red by assertion: %+v", rec)
		}
	})
	t.Run("no response", func(t *testing.T) {
		h := newHarness(t, baseConfig(), routes{}, nil)
		rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeDryRun)
		if rec.Result != toolwire.RunFail {
			t.Fatalf("got %+v", rec)
		}
		if _, ok := rec.AssertionStep(); ok {
			t.Errorf("an application that did not answer is red by assertion: %+v", rec)
		}
	})
}

// markedExecCheck's program names the attempt's marker, {{marker}}, and
// its expect is met by output that ends with DONE: either its marker or
// nothing decides it.
const markedExecCheck = `{"id":"ws.echo","outcome":"live","form":"exec","exec":{
 "argv":["python3","client.py","{{run}}"],"files":[{"path":"client.py","content":"print('{{marker}} x: y')\n"}],
 "expect":{"exit":0,"stdoutContains":"DONE"}}}`

// attemptMarkerPattern matches the marker maul substituted into an
// attempt's program: the line start only that attempt's output may carry.
var attemptMarkerPattern = regexp.MustCompile(`MAUL-ASSERTION-FAILED-[0-9a-f]{32}:`)

func attemptMarker(t *testing.T, req *toolwire.ExecRequest) string {
	t.Helper()
	content := string(req.Files[0].Content)
	m := attemptMarkerPattern.FindString(content)
	if m == "" || strings.Contains(content, checks.ExecMarkerPlaceholder) {
		t.Fatalf("{{marker}} not substituted in %q", content)
	}
	return m
}

// TestExecAssertionFailures: an exec check is red by assertion only when
// its program said so itself, printing its attempt's marker (what
// {{marker}} stood for) with the failing assertion's id at the start of a
// line: it reached and judged that assertion. An exit code alone (a crash,
// 126, 127, a signal), a traceback, missing output, a marker naming no id,
// the bare MAUL-ASSERTION-FAILED an application can echo, or a program
// killed at its time limit never is. %M in an output is the attempt's
// marker.
func TestExecAssertionFailures(t *testing.T) {
	marked := "%M upset-stake: the stake was 1, expected more than 1\n"
	for _, tc := range []struct {
		name      string
		res       toolexec.Result
		assertion bool
	}{
		{"its own failed assertion, exit 1", toolexec.Result{Exit: 1, Output: marked}, true},
		{"its own failed assertion, output unmet", toolexec.Result{Exit: 0, Output: marked}, true},
		{"a marker naming no id, then a valid one", toolexec.Result{Exit: 1, Output: "%M no id\n" + marked}, true},
		{"wrong exit alone", toolexec.Result{Exit: 1}, false},
		{"command not found", toolexec.Result{Exit: 127, Output: "sh: client.py: not found\n"}, false},
		{"not executable", toolexec.Result{Exit: 126}, false},
		{"a signal", toolexec.Result{Exit: -1}, false},
		{"a traceback", toolexec.Result{Exit: 1, Output: "Traceback (most recent call last):\n" +
			"SyntaxError: invalid syntax\n"}, false},
		{"output missing", toolexec.Result{Exit: 0, Output: "nothing"}, false},
		{"a marker naming no assertion", toolexec.Result{Exit: 1, Output: "%M no\n"}, false},
		{"the marker mid-line", toolexec.Result{Exit: 1, Output: "echo " + marked}, false},
		{"the bare marker the application echoed", toolexec.Result{Exit: 1,
			Output: "MAUL-ASSERTION-FAILED: upset-stake: x\n"}, false},
		{"timed out", toolexec.Result{TimedOut: true, Output: marked}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{run: func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
				res := tc.res
				res.Output = strings.ReplaceAll(res.Output, "%M", attemptMarker(t, req))
				return res, nil
			}}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail || rec.Digest != bareDigest(t, markedExecCheck) {
				t.Fatalf("got %+v", rec)
			}
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
			if tc.assertion && rec.FailedAssertionID() != "marker upset-stake" {
				t.Errorf("assertion id %q, want marker upset-stake", rec.FailedAssertionID())
			}
		})
	}
}

// TestExecMarkerFailsTheCheck (S1 demo #57; PR #320 F1, F3): a program
// that prints its attempt's marker has said the outcome does not hold, so
// the check fails whatever its exit code and its expect. Only that marker
// counts: text the application controls and the program echoes (the bare
// MAUL-ASSERTION-FAILED:, another attempt's marker) never turns a green
// check red. A run whose output was cut at its bound never passes: a marker
// past the cut would be lost, so it errs (never a fail) unless a marker
// was kept. %M is the attempt's marker.
func TestExecMarkerFailsTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		res       toolexec.Result
		want      toolwire.RunResult
		assertion bool
	}{
		{"no marker", toolexec.Result{Output: "ok bpm=150\nDONE\n"}, toolwire.RunPass, false},
		{"a marker, then exit 0 and the sentinel",
			toolexec.Result{Output: "%M header-present: no X-Duration-Ms\nDONE\n"}, toolwire.RunFail, true},
		{"a marker naming no assertion", toolexec.Result{Output: "%M no header\nDONE\n"}, toolwire.RunFail, false},
		{"the marker mid-line", toolexec.Result{Output: "echo %M a: x\nDONE\n"}, toolwire.RunPass, false},
		{"the application's text echoed", toolexec.Result{Output: "MAUL-ASSERTION-FAILED: header-present: " +
			"from a response body\nMAUL-ASSERTION-FAILED-0123: a: x\nDONE\n"}, toolwire.RunPass, false},
		{"another attempt's marker", toolexec.Result{Output: checks.ExecMarker(strings.Repeat("ab", 16)) +
			" header-present: x\nDONE\n"}, toolwire.RunPass, false},
		{"cut at its bound", toolexec.Result{Output: "DONE\n", Truncated: true}, toolwire.RunError, false},
		{"cut at its bound, a marker kept",
			toolexec.Result{Output: "%M header-present: x\nDONE\n", Truncated: true}, toolwire.RunFail, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{run: func(req *toolwire.ExecRequest, _ int) (toolexec.Result, error) {
				res := tc.res
				res.Output = strings.ReplaceAll(res.Output, "%M", attemptMarker(t, req))
				return res, nil
			}}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
			if rec.Result != tc.want {
				t.Fatalf("result %s, want %s (%+v)", rec.Result, tc.want, rec)
			}
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
			if tc.assertion && rec.FailedAssertionID() != "marker header-present" {
				t.Errorf("assertion id %q, want marker header-present", rec.FailedAssertionID())
			}
		})
	}
}

// TestExecMarkerIsFreshPerAttempt (PR #320 F1): every attempt's program
// gets a marker of its own, so one attempt's output can never decide
// another's.
func TestExecMarkerIsFreshPerAttempt(t *testing.T) {
	ex := &fakeExec{run: func(*toolwire.ExecRequest, int) (toolexec.Result, error) {
		return toolexec.Result{Exit: 1}, nil
	}}
	h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
	rec := h.relay.runCheck(context.Background(), parseCheck(t, markedExecCheck), toolwire.ModeDryRun)
	if rec.Result != toolwire.RunFail || len(ex.reqs) != 2 {
		t.Fatalf("got %+v after %d attempts, want a fail after 2", rec, len(ex.reqs))
	}
	if a, b := attemptMarker(t, &ex.reqs[0]), attemptMarker(t, &ex.reqs[1]); a == b {
		t.Errorf("both attempts got the marker %q", a)
	}
}

// TestBrowserAssertionFailures: an expect step whose matcher or maul
// assertion did not hold is an assertion; a timed-out test, an action that
// timed out, an unresolved or ambiguous locator, an element with no box, a
// trial click and a check error are not.
func TestBrowserAssertionFailures(t *testing.T) {
	at := func(status, msg, where string, ann ...string) func(int, string) string {
		return func(_ int, spec string) string { return report(status, ann, msg, lineOf(spec, where)) }
	}
	for _, tc := range []struct {
		name      string
		reports   func(int, string) string
		assertion bool
	}{
		{"text expectation", at("failed", "Error: expect(locator).toContainText(expected) failed\n"+
			"Expected substring: \"1\"\nReceived string: \"0\"", "toContainText"), true},
		{"maul's measurement", at("failed", "Error: maul: size failed: width 30 not 40 +/- 1", "toContainText"), true},
		{"a count of elements that are there", at("failed", "Error: expect(locator).toHaveCount(expected) failed\n"+
			"Expected: 1\nReceived: 2", "toContainText"), true},
		{"a count of nothing", at("failed", "Error: expect(locator).toHaveCount(expected) failed\n"+
			"Expected: 1\nReceived: 0", "toContainText"), false},
		{"element not found", at("failed", "Error: expect(locator).toContainText(expected) failed\n"+
			"Received: <element(s) not found>", "toContainText"), false},
		{"strict mode", at("failed", "Error: expect(locator).toContainText(expected) failed\n"+
			"Error: strict mode violation: resolved to 2 elements", "toContainText"), false},
		{"no box", at("failed", "Error: maul: size failed: the element has no box (not visible)", "toContainText"),
			false},
		{"at an action", at("failed", "Error: expect(locator).toContainText(expected) failed", ".click()"), false},
		{"action timeout", at("failed", "Error: locator.click: Timeout 5000ms exceeded.", ".click()"), false},
		{"test timed out", at("timedOut", "Test timeout of 30000ms exceeded.", "toContainText"), false},
		{"indeterminate", at("failed", "Error: maul: indeterminate legible: background image", "toContainText",
			playwright.AnnotationIndeterminate), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := browserCheckHarness(t, tc.reports)
			rec := h.relay.runCheck(context.Background(), parseCheck(t, keyedBrowserCheck), toolwire.ModeDryRun)
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
		})
	}
}

// TestConfirmExpectedFailure: a check proposed to show a defect is
// confirmed only against a dry run red by assertion, and never one that
// also ran green; its confirmation entry records the expectation, and a
// check expected to pass still needs a green dry run.
func TestConfirmExpectedFailure(t *testing.T) {
	confirm := func(digest string, red bool) string {
		req := toolwire.ConfirmRequest{Digests: []string{digest}}
		if red {
			req.ExpectFail = []string{digest}
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	digest := bareDigest(t, scoreCheck)

	t.Run("red by assertion", func(t *testing.T) {
		api := fakeAPI(t, func(int) int { return 99 })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(digest, false)); resp.Refused != toolwire.RefusedNoDryRun {
			t.Fatalf("a red check confirmed as expected to pass: %+v", resp)
		}
		_, resp := h.call(toolwire.VerbConfirm, confirm(digest, true))
		if resp.Refused != "" || resp.Observed == nil || len(resp.Observed.CheckRuns) != 1 {
			t.Fatalf("confirm: %+v", resp)
		}
		if _, ok := resp.Observed.CheckRuns[0].AssertionStep(); !ok {
			t.Errorf("confirmation %+v is not red by assertion", resp.Observed.CheckRuns[0])
		}
		l := h.finish()
		var args map[string]string
		for _, e := range l.Entries {
			if e.CheckRun != nil && e.CheckRun.Mode == toolwire.ModeConfirm {
				if err := json.Unmarshal(e.Args, &args); err != nil {
					t.Fatal(err)
				}
			}
		}
		if args["digest"] != digest || args["expect"] != "fail" {
			t.Errorf("confirmation entry args %v, want the digest and expect fail", args)
		}
	})
	t.Run("red, not by assertion", func(t *testing.T) {
		h := newHarness(t, baseConfig(), routes{}, nil) // no component answers
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(digest, true)); resp.Refused != toolwire.RefusedNoRedDryRun {
			t.Errorf("a check red without an assertion was confirmed: %+v", resp)
		}
		h.finish()
	})
	t.Run("green", func(t *testing.T) {
		api := fakeAPI(t, func(int) int { return 5 })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(digest, true)); resp.Refused != toolwire.RefusedNoRedDryRun {
			t.Errorf("a green check was confirmed as showing a defect: %+v", resp)
		}
		h.finish()
	})
	t.Run("red and green", func(t *testing.T) {
		api := fakeAPI(t, func(n int) int { return []int{99, 99, 5}[min(n, 2)] })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(digest, true)); resp.Refused != toolwire.RefusedNoRedDryRun {
			t.Errorf("a check that also ran green was confirmed as showing a defect: %+v", resp)
		}
		h.finish()
	})
}

// redConfirm is a confirm of digest, expected to fail when red.
func redConfirm(t *testing.T, digest string, red bool) string {
	t.Helper()
	req := toolwire.ConfirmRequest{Digests: []string{digest}}
	if red {
		req.ExpectFail = []string{digest}
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestConfirmAgainstTheReplay (review finding 3): a saved check this
// round's replay saw green is never confirmed red, nor one it saw red
// confirmed green; and a red check must act on data of its own run.
func TestConfirmAgainstTheReplay(t *testing.T) {
	digest := bareDigest(t, scoreCheck)
	confirm := func(d string, red bool) string { return redConfirm(t, d, red) }
	t.Run("replayed green this round", func(t *testing.T) {
		api := fakeAPI(t, func(int) int { return 99 })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		h.relay.replays = []toolwire.CheckRunRecord{{Digest: digest, Mode: toolwire.ModeReplay,
			Result: toolwire.RunPass, Attempts: []toolwire.Attempt{{Run: "tok0", Result: toolwire.ResultPass}}}}
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(digest, true)); resp.Refused != toolwire.RefusedNoRedDryRun {
			t.Errorf("a check replayed green was confirmed as showing a defect: %+v", resp)
		}
		h.finish()
	})
	t.Run("replayed red this round, confirmed green", func(t *testing.T) {
		api := fakeAPI(t, func(int) int { return 5 })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		h.relay.replays = []toolwire.CheckRunRecord{{Digest: digest, Mode: toolwire.ModeReplay,
			Result: toolwire.RunFail, Attempts: []toolwire.Attempt{{Run: "tok0", Result: toolwire.ResultFail},
				{Run: "tok00", Result: toolwire.ResultFail}}}}
		h.call(toolwire.VerbCheckRun, `{"check":`+scoreCheck+`}`)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(digest, false)); resp.Refused != toolwire.RefusedNoDryRun {
			t.Errorf("a check replayed red was confirmed green: %+v", resp)
		}
		h.finish()
	})
	t.Run("a red check that creates no data of its own", func(t *testing.T) {
		const static = `{"id":"score.static","outcome":"score","form":"http","http":{"target":"api","steps":[
		 {"request":{"path":"/api/players/1"},"expect":{"json":{"/score":{"$range":[0,10]}}}}]}}`
		api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"score":99}`))
		})
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		// Its read is about no data of its own run either, so its dry run is
		// not red by assertion (F3); the confirm is refused all the same.
		h.call(toolwire.VerbCheckRun, `{"check":`+static+`}`)
		d := bareDigest(t, static)
		if _, resp := h.call(toolwire.VerbConfirm, confirm(d, true)); resp.Refused != toolwire.RefusedNoRedDryRun {
			t.Errorf("a red check acting on no run data of its own was confirmed: %+v", resp)
		}
		h.finish()
	})
}
