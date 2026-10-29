// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// The sandbox run (`--as sandbox`): the tool pod's `sandbox` container shares
// the pod's network with `tools` and the CDP shim, so the only things that
// keep it off them are application gates (DESIGN 4.2): `tools` closes any
// connection from loopback or the pod's own addresses at accept, and the
// shim demands the per-evaluation key and refuses any Origin. This mode makes
// the requests those gates exist for, with no key and with a wrong one, and
// no Origin at all, and every one must be refused.
//
// A gate answers with a 4xx status or closes the connection: both are BLOCKED.
// A 2xx/3xx is REACHED. A refused connection (nothing listens) and a 5xx
// prove nothing about a gate, so they are INCONCLUSIVE; run.sh reaches the
// same listeners from the evaluator first, so a listener that is down fails
// the run there.
const (
	toolsPort = 8090
	shimAddr  = "127.0.0.1:9222"
	// shimKeyHeader is cdpshim.KeyHeader; the probe is stdlib-only.
	shimKeyHeader = "X-Maul-Cdp-Key"
	httpWithin    = 3 * time.Second
)

type httpSpec struct {
	name    string
	url     string
	headers map[string]string
}

// sandboxSpecsFrom is the refusals to expect, given the pod's own address
// (the second route to :8090 the guard must also close).
func sandboxSpecsFrom(getenv func(string) string) ([]httpSpec, bool) {
	ip := net.ParseIP(getenv("PROBE_POD_IP"))
	if ip == nil || ip.To4() == nil {
		return nil, false
	}
	shim := "http://" + shimAddr + "/json/version"
	return []httpSpec{
		{name: "tool-api-loopback", url: "http://127.0.0.1:" + strconv.Itoa(toolsPort) + "/"},
		{name: "tool-api-pod-ip", url: "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(toolsPort)) + "/"},
		{name: "cdp-shim-no-key", url: shim},
		{name: "cdp-shim-wrong-key", url: shim, headers: map[string]string{shimKeyHeader: "not-the-key"}},
	}, true
}

// probeHTTP makes one GET with no Origin and no redirects and classifies it.
func probeHTTP(s httpSpec, within time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return outcomeInconclusive
	}
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return classifyHTTPError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode < 400:
		return outcomeReached
	case resp.StatusCode < 500:
		return "blocked"
	}
	return outcomeInconclusive
}

// classifyHTTPError: a gate that closes the connection (EOF, reset) or never
// answers is blocking; a refused connection is not evidence of a gate.
func classifyHTTPError(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return outcomeInconclusive
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, context.DeadlineExceeded):
		return "blocked"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "blocked"
	}
	return outcomeInconclusive
}

// runSandbox probes every spec and summarises: SECURITY_FAILURE if any was
// REACHED, INCONCLUSIVE if any proved nothing, else PASS.
func runSandbox(specs []httpSpec, within time.Duration, emit func(result)) (string, int) {
	start := time.Now()
	reached, inconclusive := false, false
	for _, s := range specs {
		out := probeHTTP(s, within)
		emit(result{Name: s.name, Outcome: out, ElapsedMS: time.Since(start).Milliseconds()})
		reached = reached || out == outcomeReached
		inconclusive = inconclusive || out == outcomeInconclusive
	}
	outcome, code := outcomePass, 0
	switch {
	case reached:
		outcome, code = outcomeSecurityFailure, 2
	case inconclusive:
		outcome, code = outcomeInconclusive, 3
	}
	emit(result{Name: "summary", Outcome: outcome, ElapsedMS: time.Since(start).Milliseconds()})
	return outcome, code
}
