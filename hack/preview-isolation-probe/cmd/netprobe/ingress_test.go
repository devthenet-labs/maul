// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
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
	for _, args := range [][]string{
		{"connect"}, {"connect", "a", "b"}, {"connect", "host:1"},
		{"connect", refusedIsBlocked}, {"connect", refusedIsBlocked, "10.1.2.3:8080", "x"},
		{"connect", "--refused-is-allowed", "10.1.2.3:8080"}, {"connect", "10.1.2.3:8080", refusedIsBlocked},
	} {
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

// refusingTarget is a loopback address with nothing listening: every dial
// is refused, as kube-router's REJECT (icmp-port-unreachable) refuses one.
func refusingTarget(t *testing.T) target {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	tg, err := connectTarget(addr)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// TestRefusedIsBlocked: a refused connection is INCONCLUSIVE by default
// (the listener might be down) and BLOCKED with --refused-is-blocked, each
// attempt logged as refused rather than as a timeout.
func TestRefusedIsBlocked(t *testing.T) {
	tg := refusingTarget(t)
	reachable, inconclusive := burst(time.Now(), []target{tg}, func(result) {})
	if got := ingressVerdict(reachable, inconclusive); got != outcomeInconclusive {
		t.Errorf("a refusal by default: %s, want %s", got, outcomeInconclusive)
	}
	tg.refusedBlocks = true
	var outcomes []string
	reachable, inconclusive = burst(time.Now(), []target{tg}, func(r result) { outcomes = append(outcomes, r.Outcome) })
	if got := ingressVerdict(reachable, inconclusive); got != outcomeBlocked {
		t.Errorf("a refusal with %s: %s, want %s", refusedIsBlocked, got, outcomeBlocked)
	}
	if len(outcomes) != 16 {
		t.Errorf("%d attempts, want the burst's 16", len(outcomes))
	}
	for _, o := range outcomes {
		if o != outcomeRefused {
			t.Errorf("an attempt logged %q, want %q", o, outcomeRefused)
		}
	}
	// The flag never turns a live listener into a refusal.
	srv := httptest.NewServer(okHandler())
	t.Cleanup(srv.Close)
	live, err := connectTarget(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	live.refusedBlocks = true
	reachable, inconclusive = burst(time.Now(), []target{live}, func(result) {})
	if got := ingressVerdict(reachable, inconclusive); got != outcomeReached {
		t.Errorf("a live listener with %s: %s", refusedIsBlocked, got)
	}
}

// TestRefusedIsBlockedOnlyRefusals: the flag counts ECONNREFUSED alone;
// every other error the egress run calls inconclusive stays so.
func TestRefusedIsBlockedOnlyRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{syscall.ECONNREFUSED, outcomeRefused},
		{fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), outcomeRefused},
		{syscall.ECONNRESET, "inconclusive"},
		{os.ErrDeadlineExceeded, "blocked"},
		{nil, "REACHABLE"},
	} {
		if got := connectOutcome(tc.err, true); got != tc.want {
			t.Errorf("connectOutcome(%v, true) = %q, want %q", tc.err, got, tc.want)
		}
		if got, want := connectOutcome(tc.err, false), classify(tc.err); got != want {
			t.Errorf("connectOutcome(%v, false) = %q, want classify's %q", tc.err, got, want)
		}
	}
}

// TestIMDSRefusalHonoursRefusedIsBlocked: the instance-metadata row counts a
// refusal the way every other row does (a live run found it did not).
func TestIMDSRefusalHonoursRefusedIsBlocked(t *testing.T) {
	tg := refusingTarget(t)
	tg.imds = true
	for _, tc := range []struct {
		refusedBlocks bool
		want          string
	}{{false, "inconclusive"}, {true, outcomeRefused}} {
		tg.refusedBlocks = tc.refusedBlocks
		r := probe(context.Background(), &net.Dialer{}, tg, 1, 1, time.Now())
		if r.Outcome != tc.want {
			t.Errorf("refusedBlocks=%v: outcome %q, want %q", tc.refusedBlocks, r.Outcome, tc.want)
		}
	}
}
