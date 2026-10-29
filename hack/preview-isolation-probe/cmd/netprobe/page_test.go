// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

var pageSpecs = []httpSpec{
	{name: "tool-api", url: "http://127.0.0.1:8090/"},
	{name: "cdp-shim", url: "http://127.0.0.1:9222/json/version"},
}

const (
	blockedTool = `{"name":"tool-api","outcome":"blocked"}`
	blockedShim = `{"name":"cdp-shim","outcome":"blocked"}`
)

func TestPageSpecs(t *testing.T) {
	specs, ok := pageSpecsFrom(getenvOf(nil))
	if !ok || len(specs) != 2 || specs[0].url != "http://127.0.0.1:8090/" ||
		specs[1].url != "http://127.0.0.1:9222/json/version" {
		t.Fatalf("defaults = %+v, %v", specs, ok)
	}
	specs, ok = pageSpecsFrom(getenvOf(map[string]string{"PROBE_POD_IP": "10.42.0.9"}))
	if !ok || len(specs) != 3 || specs[2].url != "http://10.42.0.9:8090/" {
		t.Fatalf("with the pod address = %+v, %v", specs, ok)
	}
	if _, ok := pageSpecsFrom(getenvOf(map[string]string{"PROBE_POD_IP": "x"})); ok {
		t.Error("accepted a bad pod address")
	}
}

func TestPageHTMLCarriesEveryTargetAndStaysReadable(t *testing.T) {
	html := pageHTML(pageSpecs)
	for _, s := range pageSpecs {
		if !strings.Contains(html, s.url) {
			t.Errorf("the page does not fetch %s", s.url)
		}
	}
	if !strings.Contains(html, "/report") {
		t.Error("the page does not report back")
	}
	// An opaque no-cors response would hide an answer: only a readable one
	// may count as REACHED.
	if strings.Contains(html, "no-cors") {
		t.Error("the page fetches in no-cors mode")
	}
}

// browse plays the browser: it fetches the page and posts a report.
func browse(base, report string) {
	resp, err := http.Get(base + "/")
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	resp, err = http.Post(base+"/report", "application/json", strings.NewReader(report))
	if err == nil {
		_ = resp.Body.Close()
	}
}

func TestRunPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report string // empty: the browser never reports
		want   string
		code   int
	}{
		{"every fetch refused", "[" + blockedTool + "," + blockedShim + "]", outcomePass, 0},
		{"one fetch read an answer", "[" + blockedTool + `,{"name":"cdp-shim","outcome":"REACHED"}]`,
			outcomeSecurityFailure, 2},
		{"a target left out", "[" + blockedTool + "]", outcomeInconclusive, 3},
		{"an unknown outcome", `[{"name":"tool-api","outcome":"ok"},` + blockedShim + "]", outcomeInconclusive, 3},
		{"an unknown target", "[" + blockedTool + "," + blockedShim + `,{"name":"x","outcome":"blocked"}]`,
			outcomeInconclusive, 3},
		{"not JSON", `nope`, outcomeInconclusive, 3},
		{"the page never ran", "", outcomeInconclusive, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			if tc.report != "" {
				go func() { time.Sleep(50 * time.Millisecond); browse("http://"+ln.Addr().String(), tc.report) }()
			}
			var lines []result
			got, code := runPage(ln, pageSpecs, 800*time.Millisecond, func(r result) { lines = append(lines, r) })
			if got != tc.want || code != tc.code {
				t.Fatalf("runPage = %s, %d; want %s, %d (%+v)", got, code, tc.want, tc.code, lines)
			}
			if last := lines[len(lines)-1]; last.Name != "summary" || last.Outcome != tc.want {
				t.Errorf("last line %+v", last)
			}
		})
	}
}

func TestPageServesNothingElse(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go runPage(ln, pageSpecs, 800*time.Millisecond, func(result) {})
	for _, c := range []struct{ method, path string }{{"GET", "/other"}, {"GET", "/report"}, {"POST", "/"}} {
		req, _ := http.NewRequest(c.method, "http://"+ln.Addr().String()+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s %s = %d", c.method, c.path, resp.StatusCode)
		}
	}
}
