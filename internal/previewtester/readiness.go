// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The readiness wait: before its first criterion the tester waits for every
// target to accept a TCP connection.
//
// Why: the tester is a new Pod on every run, and its slot's NetworkPolicy
// admits it only once the policy controller has programmed it for that Pod.
// On k3s, kube-router does so about a second after the Pod starts and
// REJECTs its traffic until then, which a client sees as ECONNREFUSED, the
// very error a dead listener gives. Sent at once, every try of the first
// criteria failed as refused, and maul judged a correct change as failing.
//
// Why this is safe (an application's runtime outcome must never become
// maul's Infra, or a hostile application could have every run report "could
// not run the checks" instead of a failing verdict): preview-controller
// starts the tester only once every component is Ready at the requested
// heads and the slot policies are in place. A Ready component's Service
// forwards to a Pod whose readiness probe, an HTTP GET on the very port the
// Service targets, has just succeeded; the Pod drops every capability, so
// it cannot program its own network to refuse one peer and admit another.
// While its Pods are Ready and the policy admits the edge, the application
// cannot make its own Service unreachable to the tester; only maul's
// environment not being programmed yet can, and that is what the window
// waits out. An application that kills its listener fails its readiness
// probe (three 5s periods) and is captured as not Ready, or as crashed when
// it exits: preview-controller then settles the attempt EnvFailed, charged
// to the application, whatever the tester recorded (route: the
// environment's classification wins over the tester's output). Once every
// target is reached, transport errors during the criteria remain the
// application's failures, as before.
const (
	// ReadinessWindowMax bounds the readiness wait.
	ReadinessWindowMax = 60 * time.Second
	// readinessBudgetShare keeps the window well inside the tester's budget
	// (finding C5): at most a quarter of it, so the criteria keep the rest.
	readinessBudgetShare = 4
	// readinessBackoffMin and readinessBackoffMax space the attempts:
	// 100ms doubling to 1s.
	readinessBackoffMin = 100 * time.Millisecond
	readinessBackoffMax = time.Second
	// readinessDialTimeout bounds one attempt: past a SYN retransmission
	// or two, where a policy that drops rather than rejects leaves it.
	readinessDialTimeout = 3 * time.Second
)

// ReadinessWindow is the readiness wait's window for a run of the given
// budget: a quarter of it, at most ReadinessWindowMax.
func ReadinessWindow(budget time.Duration) time.Duration {
	return min(budget/readinessBudgetShare, ReadinessWindowMax)
}

// awaitTargets waits, concurrently and within window, for each of targets
// (repository key to "<Service>:<port>") to accept a TCP connection, which
// it closes at once without sending a byte. It returns one wait per target,
// in key order: reached, or with every attempt failed once the window or
// MaxWaitAttempts ran out.
func (c *Client) awaitTargets(ctx context.Context, targets map[string]string,
	window time.Duration) []acceptance.TargetWait {
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	keys := slices.Sorted(maps.Keys(targets))
	waits := make([]acceptance.TargetWait, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Go(func() { waits[i] = c.awaitTarget(ctx, key, targets[key]) })
	}
	wg.Wait()
	return waits
}

// awaitTarget is one target's wait.
func (c *Client) awaitTarget(ctx context.Context, key, addr string) acceptance.TargetWait {
	w := acceptance.TargetWait{Target: key}
	start := time.Now()
	backoff := c.backoff
	for {
		w.Attempts++
		dctx, cancel := context.WithTimeout(ctx, readinessDialTimeout)
		conn, err := c.dial(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			break
		}
		w.Record(classify(err))
		if w.Attempts >= acceptance.MaxWaitAttempts || !sleep(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, readinessBackoffMax)
	}
	w.WaitMs = int32(min(time.Since(start).Milliseconds(), acceptance.MaxWaitMs))
	return w
}

// sleep waits d, reporting false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
