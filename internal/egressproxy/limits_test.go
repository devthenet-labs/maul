// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

const listPath = "/go/github.com/google/uuid/@v/list"

// blockingUpstream holds every request until release is closed, signalling
// each arrival on arrived.
func blockingUpstream(t *testing.T) (up *recordingUpstream, arrived chan struct{}, release func()) {
	t.Helper()
	arrived = make(chan struct{}, 16)
	gate := make(chan struct{})
	var once sync.Once
	up = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("ok"))
	})
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return up, arrived, release
}

// inFlight starts a request that the blocking upstream holds, and waits
// until it has arrived there.
func inFlight(t *testing.T, h *harness, arrived chan struct{}) chan result {
	t.Helper()
	done := make(chan result, 1)
	go func() { done <- h.get(t, listPath) }()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the upstream")
	}
	return done
}

func TestPerSourceConcurrencyQueuesThenRefuses(t *testing.T) {
	up, arrived, release := blockingUpstream(t)
	h := newHarness(t, Config{Limits: Limits{PerSourceConcurrent: 1, Wait: 150 * time.Millisecond}},
		up, failingUpstream(t))
	first := inFlight(t, h, arrived)
	start := time.Now()
	r := h.get(t, listPath)
	if r.status != http.StatusServiceUnavailable {
		t.Errorf("second request from the source = %d, want 503", r.status)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Errorf("refused after %s, want it to queue for the wait first", waited)
	}
	release()
	if r := <-first; r.status != http.StatusOK {
		t.Errorf("first request = %d, want 200", r.status)
	}
	waitForLine(t, h.log, func(l map[string]any) bool {
		return l["outcome"] == OutcomeLimited && l["reason"] == reasonConcurrency
	})
}

func TestPerSourceConcurrencyQueueAdmits(t *testing.T) {
	up, arrived, release := blockingUpstream(t)
	h := newHarness(t, Config{Limits: Limits{PerSourceConcurrent: 1, Wait: 5 * time.Second}}, up, failingUpstream(t))
	first := inFlight(t, h, arrived)
	second := make(chan result, 1)
	go func() { second <- h.get(t, listPath) }()
	time.Sleep(100 * time.Millisecond)
	release()
	if r := <-first; r.status != http.StatusOK {
		t.Errorf("first = %d", r.status)
	}
	if r := <-second; r.status != http.StatusOK {
		t.Errorf("queued second request = %d, want 200 once the slot freed", r.status)
	}
}

func TestGlobalConcurrencyRefuses(t *testing.T) {
	up, arrived, release := blockingUpstream(t)
	h := newHarness(t, Config{Limits: Limits{MaxConcurrent: 1, Wait: 100 * time.Millisecond}}, up,
		failingUpstream(t))
	first := inFlight(t, h, arrived)
	if r := h.get(t, listPath); r.status != http.StatusServiceUnavailable {
		t.Errorf("request over the global cap = %d, want 503", r.status)
	}
	release()
	<-first
	waitForLine(t, h.log, func(l map[string]any) bool {
		return l["outcome"] == OutcomeLimited && l["reason"] == reasonGlobal
	})
}

func TestRateLimitRefusesABurst(t *testing.T) {
	up := newUpstream(t, okBody("ok"))
	h := newHarness(t, Config{Limits: Limits{PerSourceRate: 1, PerSourceBurst: 2, Wait: 50 * time.Millisecond}},
		up, failingUpstream(t))
	codes := make([]int, 0, 4)
	for range 4 {
		codes = append(codes, h.get(t, listPath).status)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 503 || codes[3] != 503 {
		t.Errorf("burst of 4 at burst 2 = %v, want 200 200 503 503", codes)
	}
	waitForLine(t, h.log, func(l map[string]any) bool {
		return l["outcome"] == OutcomeLimited && l["reason"] == reasonRate
	})
}

func TestRequestTimeoutCutsASlowUpstream(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("late"))
	})
	h := newHarness(t, Config{Limits: Limits{RequestTimeout: 100 * time.Millisecond}}, up, failingUpstream(t))
	start := time.Now()
	r := h.get(t, listPath)
	if r.status != http.StatusBadGateway || r.body != "egress-proxy: upstream unreachable (timeout)\n" {
		t.Errorf("slow upstream = %d %q, want 502 timeout", r.status, r.body)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
}

func TestLimitsDefaults(t *testing.T) {
	got := Limits{}.withDefaults()
	want := Limits{
		MaxConcurrent:       128,
		PerSourceConcurrent: 32,
		PerSourceRate:       100,
		PerSourceBurst:      200,
		Wait:                30 * time.Second,
		RequestTimeout:      10 * time.Minute,
		HeaderTimeout:       2 * time.Minute,
		MaxZipBytes:         524288000,
	}
	if got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	caps := map[Request]int64{
		{Route: RouteGo, Kind: KindZip}:       524288000,
		{Route: RouteGo, Kind: KindMod}:       16 << 20,
		{Route: RouteGo, Kind: KindInfo}:      64 << 10,
		{Route: RouteGo, Kind: KindLatest}:    64 << 10,
		{Route: RouteGo, Kind: KindList}:      4 << 20,
		{Route: RouteSumDB, Kind: KindLatest}: 8 << 10,
		{Route: RouteSumDB, Kind: KindLookup}: 64 << 10,
		{Route: RouteSumDB, Kind: KindTile}:   1 << 20,
	}
	for r, c := range caps {
		if got := want.bodyCap(r); got != c {
			t.Errorf("cap %s/%s = %d, want %d", r.Route, r.Kind, got, c)
		}
	}
}

// TestRequestTimeoutBoundsAClientThatStopsReading: a client that requests
// a large zip and never reads the answer blocks the proxy's copy in a write
// to its connection. The request's deadline must end that write too, so
// its limit slots are released after --request-timeout rather than when
// the connection finally dies.
func TestRequestTimeoutBoundsAClientThatStopsReading(t *testing.T) {
	const zipPath = "/go/github.com/google/uuid/@v/v1.6.0.zip"
	streaming := make(chan struct{}, 1)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/github.com/google/uuid/@v/v1.6.0.zip" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		select {
		case streaming <- struct{}{}:
		default:
		}
		chunk := make([]byte, 64<<10)
		for range 4096 { // 256 MiB, far past any socket buffer
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	const timeout = 300 * time.Millisecond
	h := newHarness(t, Config{Limits: Limits{MaxConcurrent: 1, Wait: 50 * time.Millisecond,
		RequestTimeout: timeout}}, up, failingUpstream(t))

	u := mustURL(t, h.urls["go"])
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	if _, err := io.WriteString(conn, "GET "+zipPath+" HTTP/1.1\r\nHost: "+u.Host+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-streaming:
	case <-time.After(5 * time.Second):
		t.Fatal("the zip request never reached the upstream")
	}
	// The stalled client holds the only global slot until its deadline.
	start := time.Now()
	for {
		r := h.get(t, listPath)
		if r.status == http.StatusOK {
			break
		}
		if r.status != http.StatusServiceUnavailable {
			t.Fatalf("request beside the stalled client = %d %q, want 503 then 200", r.status, r.body)
		}
		if time.Since(start) > 10*timeout+2*time.Second {
			t.Fatalf("the stalled client still holds its slot %s after the request timeout of %s",
				time.Since(start), timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitForLine(t, h.log, func(l map[string]any) bool {
		return l["kind"] == "zip" && l["outcome"] == OutcomeUpstreamError && l["reason"] == "stream"
	})
}
