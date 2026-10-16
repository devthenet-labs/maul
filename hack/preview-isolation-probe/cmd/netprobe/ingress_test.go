// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIngressVerdict(t *testing.T) {
	for _, tc := range []struct {
		reachable, inconclusive bool
		want                    string
	}{
		{false, false, outcomeBlocked},
		{true, false, outcomeReached},
		{true, true, outcomeReached},
		{false, true, outcomeInconclusive},
	} {
		if got := ingressVerdict(tc.reachable, tc.inconclusive); got != tc.want {
			t.Errorf("ingressVerdict(%v, %v) = %s, want %s", tc.reachable, tc.inconclusive, got, tc.want)
		}
	}
}

func TestConnectTarget(t *testing.T) {
	if tg, err := connectTarget("10.1.2.3:8080"); err != nil || tg.addr != "10.1.2.3:8080" || tg.name != "slot-pod" {
		t.Fatalf("connectTarget = %+v, %v", tg, err)
	}
	for _, bad := range []string{"10.1.2.3", "slot.svc:8080", "[::1]:8080", "10.1.2.3:0", "10.1.2.3:x", ""} {
		if _, err := connectTarget(bad); err == nil {
			t.Errorf("connectTarget(%q) passed", bad)
		}
	}
}

func TestIngressModeArgs(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"egress"}} {
		if handled, _ := ingressMode(args); handled {
			t.Errorf("ingressMode(%q) handled the egress run's arguments", args)
		}
	}
	for _, args := range [][]string{{"connect"}, {"connect", "a", "b"}, {"connect", "host:1"}} {
		if handled, code := ingressMode(args); !handled || code != 2 {
			t.Errorf("ingressMode(%q) = %v, %d; want a usage error", args, handled, code)
		}
	}
}

// TestBurstReachesAListener: the connect burst against a live listener is
// REACHED, and against an address that drops every SYN is BLOCKED.
func TestBurstReachesAListener(t *testing.T) {
	srv := httptest.NewServer(okHandler())
	t.Cleanup(srv.Close)
	tg, err := connectTarget(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var results []result
	reachable, inconclusive := burst(time.Now(), []target{tg}, func(r result) { results = append(results, r) })
	if got := ingressVerdict(reachable, inconclusive); got != outcomeReached || len(results) == 0 {
		t.Fatalf("a live listener: %s after %d results", got, len(results))
	}
	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "netprobe\n" {
		t.Errorf("listener answered %d %q", resp.StatusCode, body)
	}
	// 192.0.2.0/24 is TEST-NET-1: nothing answers there, so each attempt
	// times out as a dropped connection does.
	if _, err := net.DialTimeout("tcp", "192.0.2.1:8080", 50*time.Millisecond); err == nil {
		t.Skip("TEST-NET-1 answered on this network")
	}
	blackhole := target{name: "slot-pod", addr: "192.0.2.1:8080"}
	reachable, inconclusive = burst(time.Now(), []target{blackhole}, func(result) {})
	if got := ingressVerdict(reachable, inconclusive); got != outcomeBlocked && got != outcomeInconclusive {
		t.Errorf("a black hole: %s", got)
	}
}
