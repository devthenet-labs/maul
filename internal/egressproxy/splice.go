// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// splice copies both directions until each side has closed its half, a
// side fails, a cap trips or a deadline passes (the watcher ends the
// tunnel, which unblocks both copies).
func (t *tunnel) splice() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.copy(up, t.upstream, t.fromClient) }()
	go func() { defer wg.Done(); t.copy(down, t.client, t.upstream) }()
	wg.Wait()
}

// copy moves one direction through pass. A clean end of stream is passed
// on as a half-close, so TLS close_notify and the other direction finish
// normally; any other read failure ends the tunnel.
func (t *tunnel) copy(d direction, dst net.Conn, src io.Reader) {
	buf := make([]byte, tunnelCopyBuffer)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if t.pass(d, dst, buf[:n]) != nil {
				return
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
				return
			}
		}
		t.end(OutcomeTunnelled, "")
		return
	}
}

// watch enforces an open tunnel's idle timeout and absolute lifetime and
// writes its progress lines, until stop closes or the tunnel ends. base
// is the tunnel's identity as of the 200; only the byte counts and the
// duration move.
func (t *tunnel) watch(stop <-chan struct{}, base tunnelEntry, start time.Time) {
	l := t.s.cfg.TunnelLimits
	idle := time.NewTimer(l.IdleTimeout)
	defer idle.Stop()
	lifetime := time.NewTimer(l.MaxDuration)
	defer lifetime.Stop()
	var tick <-chan time.Time
	if l.ProgressInterval > 0 {
		tk := time.NewTicker(l.ProgressInterval)
		defer tk.Stop()
		tick = tk.C
	}
	for {
		select {
		case <-stop:
			return
		case <-t.ctx.Done():
			return
		case <-lifetime.C:
			t.end(OutcomeMaxDuration, "")
			return
		case <-idle.C:
			since := time.Since(time.Unix(0, t.last.Load()))
			if since >= l.IdleTimeout {
				t.end(OutcomeIdleTimeout, "")
				return
			}
			idle.Reset(max(l.IdleTimeout-since, tunnelIdleCheckFloor))
		case <-tick:
			e := base
			e.bytesUp, e.bytesDown = t.bytes[up].Load(), t.bytes[down].Load()
			e.elapsed = t.s.cfg.Now().Sub(start)
			auditTunnelProgress(t.ctx, t.s.log, e)
		}
	}
}
