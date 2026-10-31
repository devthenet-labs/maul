// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks/playwright"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestBrowserStepFromTheSpecFrame (review finding 4): a maul assertion
// throws in maul's support module, so the report's error location may be
// that file's; the failing step is read from the spec's own frame in the
// stack, never from a line of another file. With no spec frame, the step is
// unknown: never red by assertion.
func TestBrowserStepFromTheSpecFrame(t *testing.T) {
	// The shape @playwright/test 1.64's JSON reporter writes (captured live
	// with hack/live-browser.sh): errors[] carries the throw site's location,
	// maul-checks.ts, and the stack is on the result's error alone.
	report := func(spec string, stack bool) string {
		line := lineOf(spec, "toContainText")
		loc := map[string]any{"file": "/tmp/check-1/" + playwright.SupportFile, "line": 3, "column": 9}
		e := map[string]any{"message": "Error: maul: visible failed: opacity 0", "location": loc}
		res := map[string]any{"status": "failed", "errors": []any{e}}
		if stack {
			res["error"] = map[string]any{"message": e["message"], "location": loc,
				"stack": "Error: maul: visible failed: opacity 0\n" +
					"    at fail (/tmp/check-1/" + playwright.SupportFile + ":27:9)\n" +
					"    at retry (/tmp/check-1/" + playwright.SupportFile + ":42:7)\n" +
					"    at Object.visible (/tmp/check-1/" + playwright.SupportFile + ":126:5)\n" +
					"    at /tmp/check-1/" + specFile + ":" + strconv.Itoa(line) + ":3"}
		}
		b, _ := json.Marshal(map[string]any{"suites": []any{map[string]any{"specs": []any{map[string]any{
			"tests": []any{map[string]any{"results": []any{res}}}}}}}})
		return string(b)
	}
	// Second review, 4: a spec path in the error's message (expectPath
	// quotes the page's path) is no frame.
	t.Run("a spec path in the message", func(t *testing.T) {
		h, _ := browserCheckHarness(t, func(_ int, spec string) string {
			r := report(spec, false)
			line := lineOf(spec, "toContainText")
			return strings.Replace(r, `"status":"failed"`, `"status":"failed","error":{"message":"m","stack":`+
				`"Error: maul: url failed: the page is at /x/`+specFile+`:`+strconv.Itoa(line)+`:1\n    at fail (/t/`+
				playwright.SupportFile+`:27:9)"}`, 1)
		})
		rec := h.relay.runCheck(context.Background(), parseCheck(t, browserCheck), toolwire.ModeDryRun)
		if _, ok := rec.AssertionStep(); ok {
			t.Errorf("a spec path quoted in the message located the step: %+v", rec)
		}
	})
	for _, stack := range []bool{true, false} {
		h, _ := browserCheckHarness(t, func(_ int, spec string) string { return report(spec, stack) })
		rec := h.relay.runCheck(context.Background(), parseCheck(t, browserCheck), toolwire.ModeDryRun)
		step, ok := rec.AssertionStep()
		if stack && (!ok || step != 2 || rec.FailedAssertionID() != "expect") {
			t.Errorf("with the spec frame: AssertionStep() = %d, %v (%+v); want step 2", step, ok, rec)
		}
		if !stack && ok {
			t.Errorf("with no spec frame: red by assertion at %d (%+v)", step, rec)
		}
	}
}
