// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The page run (`--as page`): hostile page JS in the tool pod's Chromium
// shares the pod's network with `tools` and the CDP shim (DESIGN 4.2). This
// mode serves a page, on a port of the pod's own, that fetches the tool API
// and the shim from JS, and the browser is pointed at it. Page JS carries an
// Origin and cannot set the key header, so every fetch must fail or be
// unreadable; only a readable answer is REACHED. The page POSTs what it saw
// back to the server that served it, which prints it.
//
// What this cannot see is a side effect the gate allowed that the page could
// not read (a no-cors request is not used here: it would hide an answer). The
// gates' refusals are covered by their own unit tests (internal/cdpshim,
// internal/previewtools).

// pageSpecsFrom is the page's targets: PROBE_PAGE_TARGETS, "name=url" with
// http URLs on ipv4 addresses; by default the tool API and the shim on
// loopback, and the tool API on PROBE_POD_IP when it is given.
func pageSpecsFrom(getenv func(string) string) ([]httpSpec, bool) {
	specs := []httpSpec{
		{name: "tool-api", url: "http://127.0.0.1:" + strconv.Itoa(toolsPort) + "/"},
		{name: "cdp-shim", url: "http://" + shimAddr + "/json/version"},
	}
	if ipEnv := getenv("PROBE_POD_IP"); ipEnv != "" {
		ip := net.ParseIP(ipEnv)
		if ip == nil || ip.To4() == nil {
			return nil, false
		}
		specs = append(specs, httpSpec{name: "tool-api-pod-ip",
			url: "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(toolsPort)) + "/"})
	}
	return specs, true
}

// pageHTML is the page. The targets are JSON in a script block (the encoder
// escapes <, > and &), never spliced into markup.
func pageHTML(specs []httpSpec) string {
	type item struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	items := make([]item, len(specs))
	for i, s := range specs {
		items[i] = item{s.name, s.url}
	}
	data, _ := json.Marshal(items)
	return `<!doctype html><meta charset="utf-8"><title>maul isolation probe</title>
<script>
const targets = ` + string(data) + `;
async function look(t) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 5000);
  try {
    const r = await fetch(t.url, {signal: ctl.signal, cache: 'no-store', credentials: 'omit'});
    await r.text();
    return {name: t.name, outcome: r.ok ? 'REACHED' : 'blocked'};
  } catch (e) {
    return {name: t.name, outcome: 'blocked'};
  } finally {
    clearTimeout(timer);
  }
}
Promise.all(targets.map(look)).then(results =>
  fetch('/report', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(results)}));
</script>
`
}

// pageReport is one target's line in the page's report.
type pageReport struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

// runPage serves the page on ln and waits up to within for its report.
func runPage(ln net.Listener, specs []httpSpec, within time.Duration, emit func(result)) (string, int) {
	start := time.Now()
	reports := make(chan []pageReport, 1)
	mux := http.NewServeMux()
	page := pageHTML(specs)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	})
	mux.HandleFunc("POST /report", func(w http.ResponseWriter, r *http.Request) {
		var got []pageReport
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&got); err != nil {
			http.Error(w, "bad report", http.StatusBadRequest)
			return
		}
		select {
		case reports <- got:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	finish := func(outcome string, code int) (string, int) {
		emit(result{Name: "summary", Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
		return outcome, code
	}
	var got []pageReport
	select {
	case got = <-reports:
	case <-time.After(within):
		return finish(outcomeInconclusive, 3)
	}
	return judgePage(specs, got, start, emit, finish)
}

// judgePage requires exactly one valid line per target.
func judgePage(specs []httpSpec, got []pageReport, start time.Time, emit func(result),
	finish func(string, int) (string, int)) (string, int) {
	want := map[string]bool{}
	for _, s := range specs {
		want[s.name] = true
	}
	seen := map[string]string{}
	for _, r := range got {
		if !want[r.Name] || (r.Outcome != outcomeReached && r.Outcome != "blocked") || seen[r.Name] != "" {
			return finish(outcomeInconclusive, 3)
		}
		seen[r.Name] = r.Outcome
	}
	if len(seen) != len(want) {
		return finish(outcomeInconclusive, 3)
	}
	reached := false
	for _, s := range specs {
		emit(result{Name: s.name, Outcome: seen[s.name], ElapsedMS: time.Since(start).Milliseconds()})
		reached = reached || seen[s.name] == outcomeReached
	}
	if reached {
		return finish(outcomeSecurityFailure, 2)
	}
	return finish(outcomePass, 0)
}

// pageMode serves the page on PROBE_PAGE_ADDR (default 127.0.0.1:8099).
func pageMode() int {
	specs, ok := pageSpecsFrom(os.Getenv)
	addr := os.Getenv("PROBE_PAGE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}
	if !ok || !ipv4Addr(addr) || strings.HasPrefix(addr, "0.0.0.0") {
		emit(result{Name: "summary", Outcome: outcomeInconclusive})
		return 3
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		emit(result{Name: "summary", Outcome: outcomeInconclusive})
		return 3
	}
	_, code := runPage(ln, specs, pageWait(os.Getenv), emit)
	return code
}
