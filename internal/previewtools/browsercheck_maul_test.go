// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The checks the reports in testdata/browser-reports were captured with,
// by the pinned test runner against a real Chromium behind the keyed shim
// (code review 2026-11-18, relay finding 9): the compiled spec's lines are
// the same under every run token, so each report judges the check it was
// run for.
const (
	// reportedTextCheck goes, clicks and expects text naming the run.
	reportedTextCheck = `{"id":"v","outcome":"v","form":"browser","browser":{"steps":[{"goto":"/battles/1"},` +
		`{"click":{"testid":"vote-a"}},` +
		`{"expect":{"assert":"text","locator":{"testid":"score-a"},"text":{"contains":"{{run}}"}}}]}}`
	// reportedClickCheck clicks an element that never appears.
	reportedClickCheck = `{"id":"v","outcome":"v","form":"browser","browser":{"steps":[{"goto":"/battles/1"},` +
		`{"click":{"testid":"nope"}},` +
		`{"expect":{"assert":"text","locator":{"testid":"score-a"},"text":{"contains":"{{run}}"}}}]}}`
	// reportedPageTextCheck goes and expects text naming the run, and
	// reportedClickableCheck goes and expects an element clickable: the
	// checks of the reports captured with a renderer killed under a step
	// and with a page whose own text holds Playwright's words.
	reportedPageTextCheck = `{"id":"v","outcome":"v","form":"browser","browser":{"steps":[{"goto":"/p"},` +
		`{"expect":{"assert":"text","locator":{"testid":"t"},"text":{"contains":"{{run}}"}}}]}}`
	reportedClickableCheck = `{"id":"v","outcome":"v","form":"browser","browser":{"steps":[{"goto":"/p"},` +
		`{"expect":{"assert":"clickable","locator":{"testid":"b"}}}]}}`
	// reportedVisibleCheck is one of maul's own assertions.
	reportedVisibleCheck = `{"id":"v","outcome":"v","form":"browser","browser":{"steps":[{"goto":"/battles/1"},` +
		`{"expect":{"assert":"visible","locator":{"testid":"nope-{{run}}"}}}]}}`
)

// capturedReport is the report testdata/browser-reports/<name>.json holds.
func capturedReport(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "browser-reports", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestBrowserCheckMaulFailuresAreErrors (code review 2026-11-18, relay
// finding 9): a failure that is maul's own, not the application's, is a
// check error (unsure), never a fail and never a fail by assertion: the
// fixture failing before any step (the shim refusing, busy or unreachable,
// a wrong or unreadable key, the environment unset), the browser closing
// or crashing during a step (an expect matcher it interrupted included),
// the test timing out, a failure placed at no step. Only an assertion that
// ran and did not hold fails, red by assertion; an action that found no
// element stays a bare fail. Each attempt is judged alone, so maul's own
// failure beside a pass is quarantined and beside a fail an error, never a
// pass or a fail.
func TestBrowserCheckMaulFailuresAreErrors(t *testing.T) {
	synthetic := func(status, message string, line int) func(*testing.T, string) string {
		return func(_ *testing.T, spec string) string {
			if line < 0 {
				line = lineOf(spec, ".click()")
			}
			return report(status, nil, message, line)
		}
	}
	captured := func(name string) func(*testing.T, string) string {
		return func(t *testing.T, _ string) string { return capturedReport(t, name) }
	}
	tests := []struct {
		name  string
		check string
		// reports answer attempt 1 and, when it does not pass, attempt 2.
		reports  [2]func(*testing.T, string) string
		result   toolwire.RunResult
		step     int
		detail   string
		asserted bool
	}{
		// maul's own: the worker fixture, before any step ran.
		{name: "the shim refuses the connection", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("fixture-connect-refused"),
				captured("fixture-connect-refused")},
			result: toolwire.RunError, detail: "the browser did not answer"},
		{name: "the shim is at its cap", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("fixture-shim-busy"), captured("fixture-shim-busy")},
			result:  toolwire.RunError, detail: "the browser did not answer"},
		{name: "the key is wrong", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("fixture-wrong-key"), captured("fixture-wrong-key")},
			result:  toolwire.RunError, detail: "the browser did not answer"},
		{name: "the key file is unreadable", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("fixture-no-key-file"),
				captured("fixture-no-key-file")},
			result: toolwire.RunError, detail: reasonOutsideSteps},
		{name: "the shim's URL is unset", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("fixture-no-cdp-url"),
				captured("fixture-no-cdp-url")},
			result: toolwire.RunError, detail: reasonOutsideSteps},
		{name: "the fixture's teardown timed out", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{
				synthetic("failed", `Tearing down "browser" exceeded the test timeout of 30000ms.`, 0),
				synthetic("failed", `Tearing down "browser" exceeded the test timeout of 30000ms.`, 0)},
			result: toolwire.RunError, detail: reasonOutsideSteps},
		{name: "a failure with no error", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{synthetic("failed", "", 0), synthetic("failed", "", 0)},
			result:  toolwire.RunError, detail: reasonOutsideSteps},
		// maul's own: the browser going away during a step.
		{name: "the browser closes during goto", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("closed-during-goto"),
				captured("closed-during-goto")},
			result: toolwire.RunError, detail: reasonBrowserGone},
		{name: "the browser closes during a click", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{captured("closed-during-click"),
				captured("closed-during-click")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the browser closes during an expect matcher", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("closed-during-expect"),
				captured("closed-during-expect")},
			result: toolwire.RunError, step: 2, detail: reasonBrowserGone},
		{name: "the browser closes during maul's assertion", check: reportedVisibleCheck,
			reports: [2]func(*testing.T, string) string{captured("closed-during-maul-assertion"),
				captured("closed-during-maul-assertion")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the browser closes during clickable's trial click", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{
				synthetic("failed", "Error: maul: clickable failed: trial click: locator.click: Target page, "+
					"context or browser has been closed", -1),
				synthetic("failed", "Error: maul: clickable failed: trial click: locator.click: Target page, "+
					"context or browser has been closed", -1)},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the page crashes", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{synthetic("failed", "Error: locator.click: Target crashed ", -1),
				synthetic("failed", "Error: locator.click: Target crashed ", -1)},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		// A renderer killed under a step (the pinned runner's words are
		// "Page crashed"; "Target crashed" only under the trial click): an
		// expect matcher it cut short reported the text it had seen so far.
		{name: "the page crashes during an expect matcher", check: reportedPageTextCheck,
			reports: [2]func(*testing.T, string) string{captured("crashed-during-expect"),
				captured("crashed-during-expect")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the page crashes during goto", check: reportedPageTextCheck,
			reports: [2]func(*testing.T, string) string{captured("crashed-during-goto"),
				captured("crashed-during-goto")},
			result: toolwire.RunError, detail: reasonBrowserGone},
		{name: "the page crashes during a click", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{captured("crashed-during-click"),
				captured("crashed-during-click")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the page crashes during maul's assertion", check: reportedVisibleCheck,
			reports: [2]func(*testing.T, string) string{captured("crashed-during-maul-assertion"),
				captured("crashed-during-maul-assertion")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the page crashes during the real trial click", check: reportedClickableCheck,
			reports: [2]func(*testing.T, string) string{captured("crashed-during-trial-click"),
				captured("crashed-during-trial-click")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the browser closes during the real trial click", check: reportedClickableCheck,
			reports: [2]func(*testing.T, string) string{captured("closed-during-trial-click"),
				captured("closed-during-trial-click")},
			result: toolwire.RunError, step: 1, detail: reasonBrowserGone},
		{name: "the test times out", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("timed-out"), captured("timed-out")},
			result:  toolwire.RunError, detail: "the check timed out"},
		// maul's own beside the application's verdict.
		{name: "maul's own then a pass", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("closed-during-expect"),
				func(*testing.T, string) string { return report("passed", nil, "", 0) }},
			result: toolwire.RunQuarantined, step: 2, detail: reasonBrowserGone},
		{name: "an assertion then maul's own", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("assertion-failed"),
				captured("closed-during-expect")},
			result: toolwire.RunError, step: 2, detail: "the text expectation was not met", asserted: true},
		// The application's: unchanged.
		{name: "an assertion that did not hold", check: reportedTextCheck,
			reports: [2]func(*testing.T, string) string{captured("assertion-failed"), captured("assertion-failed")},
			result:  toolwire.RunFail, step: 2, detail: "the text expectation was not met", asserted: true},
		// The application's own text never makes its failure maul's: an
		// element whose text holds Playwright's words for a closed and a
		// crashed browser, line for line as a call log writes them, and
		// an element covering the target whose id holds the trial click's.
		{name: "an assertion failing on text that names a closed browser", check: reportedPageTextCheck,
			reports: [2]func(*testing.T, string) string{captured("text-names-closed"),
				captured("text-names-closed")},
			result: toolwire.RunFail, step: 1, detail: "the text expectation was not met", asserted: true},
		{name: "a covering element whose id names a crashed trial click", check: reportedClickableCheck,
			reports: [2]func(*testing.T, string) string{captured("covering-element-names-crashed"),
				captured("covering-element-names-crashed")},
			result: toolwire.RunFail, step: 1,
			detail: "maul: clickable failed: centre (20,19) hits div#trial click failed"},
		{name: "a covering element whose id quotes a whole crashed trial click", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{
				synthetic("failed", "Error: maul: clickable failed: centre (20,19) hits div#x trial click: "+
					"locator.click: Page crashed", -1),
				synthetic("failed", "Error: maul: clickable failed: centre (20,19) hits div#x trial click: "+
					"locator.click: Page crashed", -1)},
			result: toolwire.RunFail, step: 1,
			detail: "maul: clickable failed: centre (20,19) hits div#x trial click failed"},
		{name: "a page function's error that names a crash", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{
				synthetic("failed", "Error: locator.evaluate: Error: Page crashed\n    at eval", -1),
				synthetic("failed", "Error: locator.evaluate: Error: Page crashed\n    at eval", -1)},
			result: toolwire.RunFail, step: 1, detail: "the step failed"},
		{name: "an action that found no element", check: reportedClickCheck,
			reports: [2]func(*testing.T, string) string{
				synthetic("failed", "Error: locator.click: Timeout 5000ms exceeded.", -1),
				synthetic("failed", "Error: locator.click: Timeout 5000ms exceeded.", -1)},
			result: toolwire.RunFail, step: 1, detail: "an action timed out"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := browserCheckHarness(t, func(n int, spec string) string { return tc.reports[min(n, 1)](t, spec) })
			rec := h.relay.runCheck(context.Background(), parseCheck(t, tc.check), toolwire.ModeReplay)
			if rec.Result != tc.result {
				t.Fatalf("result %s, want %s (%+v)", rec.Result, tc.result, rec.Attempts)
			}
			first := rec.Attempts[0].Steps
			if last := first[len(first)-1]; last.Index != tc.step || last.Detail != tc.detail ||
				last.Assertion != tc.asserted {
				t.Fatalf("attempt 1 ends %+v, want step %d %q, assertion %v", last, tc.step, tc.detail, tc.asserted)
			}
			for _, a := range rec.Attempts {
				for _, s := range a.Steps {
					if s.Result == toolwire.ResultError && s.Assertion {
						t.Errorf("an error step is an assertion: %+v", s)
					}
					if strings.Contains(s.Detail, "/") || strings.Contains(s.Detail, "127.0.0.1") {
						t.Errorf("a step detail quotes a path or address: %q", s.Detail)
					}
				}
			}
			// The ledger records it in the wire shape it always had, and
			// the status reads it back with the same result: no assertion
			// step unless the run failed by one.
			e := toolwire.LedgerEntry{Seq: 3, Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun, CheckRun: &rec}
			runs := toolledger.ReplayRuns([]toolwire.LedgerEntry{e})
			if len(runs) != 1 {
				t.Fatalf("replay runs %+v", runs)
			}
			if want := map[toolwire.RunResult]v1alpha1.CheckRunResult{toolwire.RunError: v1alpha1.CheckRunError,
				toolwire.RunFail: v1alpha1.CheckRunFail, toolwire.RunQuarantined: v1alpha1.CheckRunQuarantined,
			}[tc.result]; runs[0].Result != want {
				t.Errorf("status result %s, want %s", runs[0].Result, want)
			}
			if (runs[0].AssertionStep > 0) != (tc.result == toolwire.RunFail && tc.asserted) {
				t.Errorf("status assertion step %d for a %s run", runs[0].AssertionStep, tc.result)
			}
		})
	}
}
