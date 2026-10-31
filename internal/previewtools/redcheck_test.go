// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// A check that shows a defect (the post-demo decision on S1 bug 7): only a
// red that is an assertion of a check that otherwise ran may support a
// fail. These tests pin where tools draws that line.

// TestHTTPAssertionFailures: an http check's unmet expectation (status,
// json, header, body) is an assertion; an application that does not answer,
// a request that cannot be built and a missing capture are not.
func TestHTTPAssertionFailures(t *testing.T) {
	t.Run("json out of range", func(t *testing.T) {
		api := fakeAPI(t, func(int) int { return 99 })
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeDryRun)
		if step, ok := rec.AssertionStep(); !ok || step != 1 {
			t.Fatalf("AssertionStep() = %d, %v (%+v); want step 1", step, ok, rec)
		}
	})
	t.Run("a status not expected", func(t *testing.T) {
		api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
		rec := h.relay.runCheck(context.Background(), parseCheck(t, scoreCheck), toolwire.ModeDryRun)
		if step, ok := rec.AssertionStep(); !ok || step != 0 {
			t.Fatalf("AssertionStep() = %d, %v (%+v); want step 0", step, ok, rec)
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

// TestExecAssertionFailures: an exec check's unmet exit or output is an
// assertion; a program killed at its time limit is not.
func TestExecAssertionFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		res       toolexec.Result
		assertion bool
	}{
		{"wrong exit", toolexec.Result{Exit: 1}, true},
		{"output missing", toolexec.Result{Exit: 0, Output: "nothing"}, true},
		{"timed out", toolexec.Result{TimedOut: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &fakeExec{run: func(*toolwire.ExecRequest, int) (toolexec.Result, error) { return tc.res, nil }}
			h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, execCheck), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail || rec.Digest != bareDigest(t, execCheck) {
				t.Fatalf("got %+v", rec)
			}
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
		})
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
			rec := h.relay.runCheck(context.Background(), parseCheck(t, browserCheck), toolwire.ModeDryRun)
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
