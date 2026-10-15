// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"sync"
)

// tunnelSet tracks the CONNECT handlers from before their hijack to after
// their close line, which net/http stops doing at the hijack: Shutdown
// returns at once and runs its OnShutdown funcs without waiting for them,
// so without this a process could exit before a tunnel's close line is
// written.
type tunnelSet struct {
	mu sync.Mutex
	// open are the hijacked tunnels; active counts every handler between
	// enter and leave, hijacked or not yet.
	open   map[*tunnel]struct{}
	active int
	// closed is set once by closeAll: a tunnel tracked after it is ended
	// at once.
	closed bool
	// idle is closed while active is zero.
	idle chan struct{}
}

func newTunnelSet() *tunnelSet {
	idle := make(chan struct{})
	close(idle)
	return &tunnelSet{open: map[*tunnel]struct{}{}, idle: idle}
}

// enter counts one CONNECT handler, before its hijack.
func (ts *tunnelSet) enter() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.active == 0 {
		ts.idle = make(chan struct{})
	}
	ts.active++
}

// track records the hijacked tunnel; false once closeAll has run, when the
// caller ends it (outcome shutdown) instead of serving it.
func (ts *tunnelSet) track(t *tunnel) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.open[t] = struct{}{}
	return !ts.closed
}

// leave uncounts a handler entered with enter, after its close line.
func (ts *tunnelSet) leave(t *tunnel) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t != nil {
		delete(ts.open, t)
	}
	ts.active--
	if ts.active == 0 {
		close(ts.idle)
	}
}

// closeAll refuses every later tunnel and ends the open ones.
func (ts *tunnelSet) closeAll() {
	ts.mu.Lock()
	ts.closed = true
	open := make([]*tunnel, 0, len(ts.open))
	for t := range ts.open {
		open = append(open, t)
	}
	ts.mu.Unlock()
	for _, t := range open {
		t.end(OutcomeShutdown, "")
	}
}

// wait returns once no handler is between enter and leave, or ctx's error.
func (ts *tunnelSet) wait(ctx context.Context) error {
	ts.mu.Lock()
	idle := ts.idle
	ts.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
