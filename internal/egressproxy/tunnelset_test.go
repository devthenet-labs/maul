// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shutdownLines counts the tunnel close lines (msg tunnel or refused) with
// outcome shutdown, read once: no polling, so a line still being written
// is not counted.
func shutdownLines(t *testing.T, h *tunnelHarness) int {
	t.Helper()
	n := 0
	for _, l := range h.log.lines(t) {
		if l["route"] == "tunnel" && (l["msg"] == "tunnel" || l["msg"] == "refused") && l["outcome"] == OutcomeShutdown {
			n++
		}
	}
	return n
}

// TestDrainTunnelsWaitsForCloseLines: Shutdown returns without waiting for
// the hijacked tunnels, so a process exiting after it could lose their
// close lines; once DrainTunnels returns every open tunnel's shutdown line
// is already written.
func TestDrainTunnelsWaitsForCloseLines(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	const open = 3
	for range open {
		if _, err := h.open(t, "web", "pkg.registry.dev", ""); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.https["web"].Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.DrainTunnels(ctx); err != nil {
		t.Fatalf("DrainTunnels = %v", err)
	}
	if got := shutdownLines(t, h); got != open {
		t.Errorf("%d shutdown close lines when DrainTunnels returned, want %d:\n%s", got, open, h.log.String())
	}
}

// TestTunnelHijackedAfterCloseEnds: a CONNECT hijacked after CloseTunnels
// walked the open tunnels (a connection accepted before the listener
// closed) is refused with a 503 and outcome shutdown, never dialled, and
// DrainTunnels waits for its close line too.
func TestTunnelHijackedAfterCloseEnds(t *testing.T) {
	h := newTunnelHarness(t, Config{})
	h.srv.CloseTunnels()
	_, br := h.connect(t, "web", "CONNECT pkg.registry.dev:443 HTTP/1.1")
	r := readRaw(t, br)
	if want := "HTTP/1.1 503 " + shuttingDownPhrase; r.statusLine != want {
		t.Errorf("a CONNECT after CloseTunnels = %q, want %q", r.statusLine, want)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.srv.DrainTunnels(ctx); err != nil {
		t.Fatalf("DrainTunnels = %v", err)
	}
	if n := shutdownLines(t, h); n != 1 {
		t.Errorf("%d shutdown close lines, want 1:\n%s", n, h.log.String())
	}
	if h.up.accepted.Load() != 0 {
		t.Error("a tunnel hijacked after CloseTunnels dialled upstream")
	}
}

// TestTunnelSetWait: wait returns at once with no handler in flight,
// blocks (to ctx's error) while one is between enter and leave, whether
// it is hijacked or not, and returns once it leaves; an enter after an
// idle spell blocks it again.
func TestTunnelSetWait(t *testing.T) {
	short := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(t.Context(), 20*time.Millisecond)
	}
	ts := newTunnelSet()
	ctx, cancel := short()
	if err := ts.wait(ctx); err != nil {
		t.Errorf("wait with nothing in flight = %v", err)
	}
	cancel()

	ts.enter()
	ctx, cancel = short()
	if err := ts.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait with an unhijacked handler in flight = %v, want the deadline", err)
	}
	cancel()
	tn := &tunnel{}
	if !ts.track(tn) {
		t.Error("track before closeAll refused the tunnel")
	}
	ts.leave(tn)
	ctx, cancel = short()
	if err := ts.wait(ctx); err != nil {
		t.Errorf("wait after leave = %v", err)
	}
	cancel()

	ts.enter()
	ctx, cancel = short()
	if err := ts.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait after a second enter = %v, want the deadline", err)
	}
	cancel()
	ts.closeAll()
	if ts.track(&tunnel{}) {
		t.Error("track after closeAll admitted the tunnel")
	}
}
