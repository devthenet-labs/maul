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
// A target still unreachable after the whole window is the application's
// failure, never maul's Infra. An application's runtime outcome must never
// become Infra, or a hostile application could have every run report
// "could not run the checks" instead of a failing verdict, and it can make
// itself unreachable to the tester while staying Ready: a process with no
// capability can attach a classic BPF socket filter (SO_ATTACH_FILTER) to
// its listening socket that drops every SYN but the node's, so the
// kubelet's readiness probe keeps passing while every tester dial fails.
// What the window absorbs is the transient on maul's side, the slot policy
// being programmed for the new tester Pod (about a second on k3s, against a
// window of up to a minute). Past it, preview-controller marks the
// component unreachable (its environment fact's unreachableForMs) and
// settles the attempt EnvFailed, component-unreachable, charged to the
// change: it counts, and may arm a fix round. Once every target is reached,
// transport errors during the criteria remain the application's failures,
// as before.
const (
	// ReadinessWindowMax bounds the readiness wait.
	ReadinessWindowMax = 60 * time.Second
	// readinessBudgetShare keeps the window well inside the tester's budget
	// (finding C5): at most a quarter of it, so the criteria keep the rest.
	readinessBudgetShare = 4
	// readinessBackoffMin and readinessBackoffMax space the attempts:
	// 100ms doubling to 1s. A client's backoff is never below a
	// millisecond, so the wait never spins.
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
