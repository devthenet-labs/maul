// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"strconv"
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
	for _, stack := range []bool{true, false} {
		h, _ := browserCheckHarness(t, func(_ int, spec string) string { return report(spec, stack) })
		rec := h.relay.runCheck(context.Background(), parseCheck(t, browserCheck), toolwire.ModeDryRun)
		step, ok := rec.AssertionStep()
		if stack && (!ok || step != 2) {
			t.Errorf("with the spec frame: AssertionStep() = %d, %v (%+v); want step 2", step, ok, rec)
		}
		if !stack && ok {
			t.Errorf("with no spec frame: red by assertion at %d (%+v)", step, rec)
		}
	}
}
