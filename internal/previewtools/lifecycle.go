// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TerminationPath is the container's termination message file (the
// Kubernetes default terminationMessagePath).
const TerminationPath = "/dev/termination-log"

// shutdownGrace bounds how long a finished relay waits for the response
// to the call that finished it.
const shutdownGrace = 5 * time.Second

// RunOptions configure Run.
type RunOptions struct {
	Options
	// Listener serves the tool endpoints; the caller guards it
	// (GuardListener). A round that finishes before serving never uses it.
	Listener net.Listener
	// Timeout replaces the config's wall clock when positive (tests).
	Timeout time.Duration
	// ReadinessWindow replaces ReadinessWindow(timeout) when positive
	// (tests).
	ReadinessWindow time.Duration
	// Front, when set, serves the preview front (NewFront) for the
	// browser from the start: a loopback listener (FrontAddr).
	Front net.Listener
}

// Run is the tools container's life (DESIGN 2.2 steps 5 to 8; SPEC Q18):
//
//  1. wait, within the readiness window, for every component to accept a
//     TCP connection, recorded as the first entry (a relay http entry
//     whose args are {"readiness": …} and whose output is the
//     acceptance.TargetWait list); a component still unreachable finishes
//     the round there (FinishUnreachable), no check run;
//  2. replay every stored check, each alone with fresh run tokens, one
//     relay check-run entry each;
//  3. a replay round finishes there (FinishReplayOnly);
//  4. otherwise open the browser session, when the pod has a browser (a
//     relay session entry), and serve the agent until the runner's
//     finish, or finish on its own at the wall clock (FinishDeadline) or
//     when ctx ends (FinishStopped).
//
// The preview front (o.Front) serves the browser throughout, replays
// included.
//
// Every way out closes the ledger with its summary and writes the
// termination message, so a crashed evaluator still leaves a ledger.
func Run(ctx context.Context, o RunOptions) (toolwire.Summary, error) {
	r, err := NewRelay(o.Options)
	if err != nil {
		return toolwire.Summary{}, err
	}
	timeout := o.Config.Timeout()
	if o.Timeout > 0 {
		timeout = o.Timeout
	}
	window := ReadinessWindow(timeout)
	if o.ReadinessWindow > 0 {
		window = o.ReadinessWindow
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if o.Front != nil {
		front := &http.Server{Handler: NewFront(o.Config, o.Client.dial), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = front.Serve(o.Front) }()
		defer func() { _ = front.Close() }()
	}

	if !r.awaitComponents(dctx, window) {
		r.finishRelay(stopReason(ctx, dctx, FinishUnreachable))
		return r.result()
	}
	r.replay(dctx)
	if o.Config.ReplayOnly || dctx.Err() != nil {
		r.finishRelay(stopReason(ctx, dctx, FinishReplayOnly))
		return r.result()
	}
	if o.Listener == nil {
		r.finishRelay(FinishStopped)
		return r.result()
	}
	r.openBrowser(dctx)
	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return dctx }}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(o.Listener) }()
	select {
	case <-r.Done():
	case <-dctx.Done():
		r.finishRelay(stopReason(ctx, dctx, FinishDeadline))
	case err := <-served:
		r.finishRelay(FinishStopped)
		if !errors.Is(err, http.ErrServerClosed) {
			sum, _ := r.result()
			return sum, err
		}
	}
	sctx, scancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer scancel()
	_ = srv.Shutdown(sctx)
	return r.result()
}

// stopReason is FinishStopped when ctx ended (the container told to stop),
// FinishDeadline when only the wall clock dctx did, else reason.
func stopReason(ctx, dctx context.Context, reason string) string {
	switch {
	case ctx.Err() != nil:
		return FinishStopped
	case dctx.Err() != nil:
		return FinishDeadline
	}
	return reason
}

// result is the closed ledger's summary.
func (r *Relay) result() (toolwire.Summary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		return toolwire.Summary{}, errors.New("previewtools: the ledger is not closed")
	}
	return r.summary, nil
}

// awaitComponents runs the readiness wait and records it, reporting
// whether every component was reached.
func (r *Relay) awaitComponents(ctx context.Context, window time.Duration) bool {
	waits := r.client.awaitTargets(ctx, r.cfg.targets(), window)
	out, _ := json.Marshal(waits)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.record(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbHTTP,
		Args: mustArgs(map[string]any{"readiness": map[string]any{"targets": r.cfg.keys()}}), Output: string(out)})
	return len(acceptance.Unreached(waits)) == 0 && ctx.Err() == nil
}

// replay runs every stored check alone, in the config's order, each a
// relay check-run entry.
func (r *Relay) replay(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.cfg.Checks {
		if ctx.Err() != nil {
			return
		}
		c, err := checks.Parse([]byte(s.Body)) // Config.Validate parsed it
		if err != nil {
			continue
		}
		rec := r.runCheck(ctx, c, toolwire.ModeReplay)
		if _, err := r.record(toolwire.LedgerEntry{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun,
			Args: mustArgs(map[string]string{"digest": rec.Digest}), CheckRun: &rec}); err != nil {
			return
		}
		r.replays = append(r.replays, rec)
	}
}
