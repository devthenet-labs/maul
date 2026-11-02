// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// S1 demo bugs 9 and 10 (0.13.9): red-check confirmation was too
// conservative. Since #305 a person confirms every defect found on checks
// saved red alone, so a false red costs a false alarm, never a false fail.

// TestRedOwnershipIsTransitive (demo bug 10): a write that names only the
// run's own captures is the run's own too, so what it returns is: two
// patterns named {{run}}, a battle of exactly those, then a vote in that
// battle whose stake is wrong is red by assertion.
func TestRedOwnershipIsTransitive(t *testing.T) {
	api := newFakeComponent(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/patterns":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"p_1"}`))
		case "/api/battles":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"b_1"}`))
		default: // the vote: stakes are +1/+1, the defect
			_, _ = w.Write([]byte(`{"stakes":{"a":1,"b":1}}`))
		}
	})
	const check = `{"id":"upset","outcome":"upset","form":"http","http":{"target":"api","steps":[
	 {"request":{"method":"POST","path":"/api/patterns","json":{"name":"{{run}}-a"}},"expect":{"status":201},
	  "capture":{"pida":"/id"}},
	 {"request":{"method":"POST","path":"/api/patterns","json":{"name":"{{run}}-b"}},"expect":{"status":201},
	  "capture":{"pidb":"/id"}},
	 {"request":{"method":"POST","path":"/api/battles","json":{"patternAId":"{{capture.pida}}",
	  "patternBId":"{{capture.pidb}}"}},"expect":{"status":201},"capture":{"battleid":"/id"}},
	 {"request":{"method":"POST","path":"/api/battles/{{capture.battleid}}/votes","json":{"winner":"a"}},
	  "expect":{"status":200,"json":{"/stakes/b":{"$range":[2,1000]}}}}]}}`
	h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
	rec := h.relay.runCheck(context.Background(), parseCheck(t, check), toolwire.ModeDryRun)
	if rec.Result != toolwire.RunFail {
		t.Fatalf("got %+v", rec)
	}
	if step, ok := rec.AssertionStep(); !ok || step != 3 || rec.FailedAssertionID() != "json /stakes/b" {
		t.Errorf("red by assertion at %d (%v, %q), want step 3, json /stakes/b: %+v", step, ok,
			rec.FailedAssertionID(), rec)
	}
}

// layoutCheck reads the page's layout and writes nothing: a pad is 48px
// tall.
const layoutCheck = `{"id":"pads","outcome":"pads","form":"browser","browser":{"steps":[{"goto":"/"},` +
	`{"expect":{"assert":"size","locator":{"testid":"pad-1"},"height":48,"tolerance":0.5}}]}}`

// textCheck reads text keyed by the run and writes nothing: the run made
// no data for it to be about.
const textCheck = `{"id":"title","outcome":"title","form":"browser","browser":{"steps":[{"goto":"/"},` +
	`{"expect":{"assert":"text","locator":{"testid":"title"},"text":{"contains":"{{run}}"}}}]}}`

// TestRedLayoutCheckThatWritesNothing (demo bug 9): a browser check that
// writes nothing may show a defect when its failing assertion measures the
// page's layout (size, a box relation, aligned, legible, clickable,
// visible): its dry run is red by assertion and its red confirmation runs.
// One whose failing assertion is about text is refused, with its own
// reason code (never no-red-dry-run) and its reason in the ledger.
func TestRedLayoutCheckThatWritesNothing(t *testing.T) {
	fails := func(marker, message string) func(int, string) string {
		return func(_ int, spec string) string { return report("failed", nil, message, lineOf(spec, marker)) }
	}
	t.Run("a size read", func(t *testing.T) {
		h, _ := browserCheckHarness(t, fails("maul.size(", "Error: maul: size failed: height 30 not 48 +/- 0.5"))
		_, resp := h.call(toolwire.VerbCheckRun, `{"check":`+layoutCheck+`}`)
		if s, ok := resp.Observed.CheckRun.AssertionStep(); !ok || s != 1 {
			t.Fatalf("dry run %+v, want red by assertion at the size step", resp.Observed.CheckRun)
		}
		if _, resp := h.call(toolwire.VerbConfirm, redConfirm(t, bareDigest(t, layoutCheck), true)); resp.Refused != "" {
			t.Errorf("a red layout check was refused: %+v", resp)
		}
		h.finish()
	})
	t.Run("a text read", func(t *testing.T) {
		h, _ := browserCheckHarness(t, fails("toContainText",
			"Error: expect(locator).toContainText(expected) failed\nExpected: \"Beats\"\nReceived: \"Bets\""))
		h.call(toolwire.VerbCheckRun, `{"check":`+textCheck+`}`)
		_, resp := h.call(toolwire.VerbConfirm, redConfirm(t, bareDigest(t, textCheck), true))
		if resp.Refused != toolwire.RefusedRedRule || !strings.Contains(resp.Reason, "writes data of its own run") {
			t.Fatalf("a red text check that writes nothing: %+v, want refused %s", resp, toolwire.RefusedRedRule)
		}
		for _, e := range h.finish().Entries {
			if e.Seq == resp.Seq && (e.Refused != toolwire.RefusedRedRule || e.Reason != resp.Reason) {
				t.Errorf("ledger entry %+v, want the refusal and its reason", e)
			}
		}
	})
}
