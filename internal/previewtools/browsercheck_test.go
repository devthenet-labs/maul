// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

const browserCheck = `{"id":"vote","outcome":"vote","form":"browser","browser":{"steps":[{"goto":"/battles/1"},` +
	`{"click":{"testid":"vote-a"}},` +
	`{"expect":{"assert":"text","locator":{"testid":"score-a"},"text":{"contains":"1"}}}]}}`

// fakeRunner is the Playwright test runner: it records each spec and
// answers with a JSON report.
type fakeRunner struct {
	mu      sync.Mutex
	specs   []string
	reports func(n int, spec string) string
}

func (f *fakeRunner) RunSpec(_ context.Context, spec []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.specs)
	f.specs = append(f.specs, string(spec))
	return []byte(f.reports(n, string(spec))), nil
}

// report is a JSON reporter report of one test with one result.
func report(status string, annotations []string, message string, line int) string {
	type loc struct {
		File string `json:"file"`
		Line int    `json:"line"`
	}
	type errT struct {
		Message  string `json:"message"`
		Location *loc   `json:"location,omitempty"`
	}
	type ann struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}
	var errs []errT
	if message != "" {
		e := errT{Message: message}
		if line > 0 {
			e.Location = &loc{File: "/tmp/check-1/" + specFile, Line: line}
		}
		errs = append(errs, e)
	}
	anns := make([]ann, 0, len(annotations))
	for _, a := range annotations {
		anns = append(anns, ann{Type: a, Description: "why"})
	}
	r := map[string]any{"suites": []any{map[string]any{"file": specFile, "specs": []any{map[string]any{
		"tests": []any{map[string]any{"annotations": anns, "results": []any{map[string]any{
			"status": status, "errors": errs, "annotations": anns}}}}}}}}}
	b, _ := json.Marshal(r)
	return string(b)
}

// lineOf is the 1-based line of the first line of spec containing s.
func lineOf(spec, s string) int {
	for i, l := range strings.Split(spec, "\n") {
		if strings.Contains(l, s) {
			return i + 1
		}
	}
	return 0
}

func browserCheckHarness(t *testing.T, reports func(n int, spec string) string) (*harness, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{reports: reports}
	cfg := baseConfig()
	cfg.Browser = true
	h := newHarness(t, cfg, routes{}, func(o *Options) {
		o.Browser = &BrowserOptions{CLI: &fakeCLI{dir: t.TempDir(), script: defaultScript}, Runner: run,
			Dir: t.TempDir(), Base: frontBase}
	})
	return h, run
}

// TestBrowserCheckRunCompilesAndRunsAlone: each attempt is the compiled
// spec under its own fresh run token, one runner process each.
func TestBrowserCheckRunCompilesAndRunsAlone(t *testing.T) {
	h, run := browserCheckHarness(t, func(int, string) string { return report("passed", nil, "", 0) })
	rec := h.relay.runCheck(context.Background(), parseCheck(t, browserCheck), toolwire.ModeDryRun)
	if rec.Result != toolwire.RunPass || len(rec.Attempts) != 1 {
		t.Fatalf("got %+v", rec)
	}
	if len(run.specs) != 1 || !strings.Contains(run.specs[0], `from "`+playwright.FixtureModule+`"`) ||
		!strings.Contains(run.specs[0], `getByTestId("vote-a")`) {
		t.Fatalf("specs %q", run.specs)
	}
	if n := len(rec.Attempts[0].Steps); n != 3 {
		t.Errorf("steps %+v, want one passing record per step", rec.Attempts[0].Steps)
	}
}

// TestBrowserCheckVerdicts: the report's result is the attempt's: a
// failure at a step fails that step (maul's words, never the page's), the
// indeterminate annotation is a check error, a run with no result is an
// error, and a fail then a pass is quarantined.
func TestBrowserCheckVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		reports func(n int, spec string) string
		result  toolwire.RunResult
		step    int
		detail  string
	}{
		{"fail at the expectation", func(_ int, spec string) string {
			return report("failed", nil, "Error: expect(locator).toContainText(expected) failed\nReceived: \"secret page text\"",
				lineOf(spec, "toContainText"))
		}, toolwire.RunFail, 2, "the text expectation was not met"},
		{"maul's clickable failure", func(_ int, spec string) string {
			return report("failed", nil, "Error: maul: clickable failed: covered by article.panel.side\nat x",
				lineOf(spec, ".click()"))
		}, toolwire.RunFail, 1, "maul: clickable failed: covered by article.panel.side"},
		{"trial click text is cut", func(_ int, spec string) string {
			return report("failed", nil, "Error: maul: clickable failed: trial click: <div>page bytes</div> intercepts",
				lineOf(spec, ".click()"))
		}, toolwire.RunFail, 1, "maul: clickable failed: trial click failed"},
		{"indeterminate is an error", func(_ int, spec string) string {
			return report("failed", []string{playwright.AnnotationIndeterminate},
				"Error: maul: indeterminate legible: background image", lineOf(spec, "toContainText"))
		}, toolwire.RunError, 2, "maul: indeterminate legible: background image"},
		{"timed out", func(int, string) string {
			return report("timedOut", nil, "Test timeout of 30000ms exceeded.", 0)
		}, toolwire.RunFail, 0, "the check timed out"},
		{"no result", func(int, string) string { return `{"suites":[]}` }, toolwire.RunError, 0,
			"the test runner reported no result"},
		{"not a report", func(int, string) string { return `nope` }, toolwire.RunError, 0,
			"the test runner's report is not JSON"},
		{"fail then pass", func(n int, spec string) string {
			if n == 0 {
				return report("failed", nil, "Error: locator.click: Timeout 5000ms exceeded.", lineOf(spec, ".click()"))
			}
			return report("passed", nil, "", 0)
		}, toolwire.RunQuarantined, 1, "an action timed out"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := browserCheckHarness(t, tc.reports)
			rec := h.relay.runCheck(context.Background(), parseCheck(t, browserCheck), toolwire.ModeReplay)
			if rec.Result != tc.result {
				t.Fatalf("result %s, want %s (%+v)", rec.Result, tc.result, rec)
			}
			steps := rec.Attempts[0].Steps
			last := steps[len(steps)-1]
			if last.Index != tc.step || last.Detail != tc.detail {
				t.Fatalf("last step %+v, want index %d detail %q", last, tc.step, tc.detail)
			}
			for _, s := range steps {
				if strings.Contains(s.Detail, "secret page text") || strings.Contains(s.Detail, "<div>") {
					t.Errorf("a step detail quotes the page: %q", s.Detail)
				}
			}
		})
	}
}

// TestBrowserCheckRunThroughTheEndpoint: check run takes the browser path
// when the pod has a browser, and records a green dry run.
func TestBrowserCheckRunThroughTheEndpoint(t *testing.T) {
	h, _ := browserCheckHarness(t, func(int, string) string { return report("passed", nil, "", 0) })
	code, resp := h.call(toolwire.VerbCheckRun, `{"check":`+browserCheck+`}`)
	if code != 200 || resp.Refused != "" || resp.Observed == nil || resp.Observed.CheckRun == nil ||
		resp.Observed.CheckRun.Result != toolwire.RunPass {
		t.Fatalf("got %d %+v", code, resp)
	}
}

// TestJudgeReportBounds: a report's step index never leaves the spec's
// steps, whatever line it names.
func TestJudgeReportBounds(t *testing.T) {
	spec := "x\n  // step 1: goto\n  a\n  // step 2: click\n  b\n"
	for _, line := range []int{-5, 0, 1, 3, 5, 99} {
		steps := judgeReport([]byte(report("failed", nil, "Error: boom", line)), []byte(spec))
		last := steps[len(steps)-1]
		if last.Index < 0 || last.Index > 1 || last.Result != toolwire.ResultFail {
			t.Errorf("line %d: %+v", line, steps)
		}
	}
}
