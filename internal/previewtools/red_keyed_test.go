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

// itemsAPI answers a POST under /api/items with a new item {"id":"7"} and
// every GET with body.
func itemsAPI(t *testing.T, body string) *fakeComponent {
	t.Helper()
	return newFakeComponent(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"7"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	})
}

// TestHTTPAssertionAboutTheRunsData (third review, F3; F2's whole-document
// pointer): an http failure is an assertion only when the assertion is
// about data the run made: the failing step reads a resource its path
// addresses by {{run}} or by a value captured from one of the check's own
// writes, or the expected value names {{run}} or such a capture. A total, a
// count or a list over shared rows ($order, $includes), a GET whose token is
// only in its query, and the whole document ("") are not.
func TestHTTPAssertionAboutTheRunsData(t *testing.T) {
	const write = `{"request":{"method":"POST","path":"/api/items","json":{"name":"{{run}}"}},` +
		`"expect":{"status":201},"capture":{"id":"/id"}}`
	check := func(read string) string {
		return `{"id":"items.k","outcome":"items","form":"http","http":{"target":"api","steps":[` + write + `,` +
			read + `]}}`
	}
	for _, tc := range []struct {
		name, read, body string
		assertion        bool
	}{
		{"its own item, by a captured id", `{"request":{"path":"/api/items/{{capture.id}}"},` +
			`"expect":{"json":{"/stake":16}}}`, `{"stake":1}`, true},
		{"a value naming the run", `{"request":{"path":"/api/items"},` +
			`"expect":{"json":{"/last":"{{run}}"}}}`, `{"last":"someone"}`, true},
		{"a shared total", `{"request":{"path":"/api/stats"},"expect":{"json":{"/total":1}}}`, `{"total":3}`, false},
		{"the token only in the query", `{"request":{"path":"/api/stats?run={{run}}"},` +
			`"expect":{"json":{"/total":1}}}`, `{"total":3}`, false},
		{"a list over shared rows", `{"request":{"path":"/api/items/{{capture.id}}"},` +
			`"expect":{"json":{"/names":{"$includes":"{{run}}"}}}}`, `{"names":["a"]}`, false},
		{"the whole document", `{"request":{"path":"/api/items/{{capture.id}}"},` +
			`"expect":{"json":{"":{"$contains":"{{run}}"}}}}`, `{"x":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := itemsAPI(t, tc.body)
			h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
			rec := h.relay.runCheck(context.Background(), parseCheck(t, check(tc.read)), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail {
				t.Fatalf("got %+v", rec)
			}
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
		})
	}
}

// keyedBrowserCheck fills in the run's own name and asserts on the row
// that carries it.
const keyedBrowserCheck = `{"id":"vote","outcome":"vote","form":"browser","browser":{"steps":[{"goto":"/battles/1"},` +
	`{"fill":{"locator":{"label":"Name"},"text":"{{run}}"}},` +
	`{"expect":{"assert":"text","locator":{"role":"row","hasText":"{{run}}"},"text":{"contains":"1"}}}]}}`

// TestBrowserAssertionAboutTheRunsData (third review, F3): a browser
// expect is an assertion only on an element keyed by the run's own data
// (its locator or expected text names {{run}}), and never a count.
func TestBrowserAssertionAboutTheRunsData(t *testing.T) {
	fail := func(_ int, spec string) string {
		return report("failed", nil, "Error: expect(locator).toContainText(expected) failed\nReceived string: \"0\"",
			lineOf(spec, "toContainText"))
	}
	countFail := func(_ int, spec string) string {
		return report("failed", nil, "Error: expect(locator).toHaveCount(expected) failed\nExpected: 2\nReceived: 1",
			lineOf(spec, "toHaveCount"))
	}
	for _, tc := range []struct {
		name, check string
		reports     func(int, string) string
		assertion   bool
	}{
		{"keyed by the run", keyedBrowserCheck, fail, true},
		{"not keyed by the run", browserCheck, fail, false},
		{"a count, keyed", strings.Replace(keyedBrowserCheck,
			`"assert":"text","locator":{"role":"row","hasText":"{{run}}"},"text":{"contains":"1"}`,
			`"assert":"count","locator":{"role":"row","hasText":"{{run}}"},"count":2`, 1), countFail, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := browserCheckHarness(t, tc.reports)
			rec := h.relay.runCheck(context.Background(), parseCheck(t, tc.check), toolwire.ModeDryRun)
			if rec.Result != toolwire.RunFail {
				t.Fatalf("got %+v", rec)
			}
			if _, ok := rec.AssertionStep(); ok != tc.assertion {
				t.Errorf("red by assertion %v, want %v (%+v)", ok, tc.assertion, rec)
			}
		})
	}
}
