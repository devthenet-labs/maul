// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpguard

import (
	"context"
	"testing"
	"time"
)

// TestIPLimiter is the broker's construction (cap = burst, no queue): moved
// here from the broker with the limiter.
func TestIPLimiter(t *testing.T) {
	if l := NewIPLimiter(IPLimiterConfig{Rate: 0, Burst: 10}); l != nil {
		t.Fatal("zero rate did not disable the limiter")
	}
	if l := NewIPLimiter(IPLimiterConfig{Rate: 2.5}); l.Burst() != 3 || l.InFlight() != 3 {
		t.Fatalf("unset burst, in-flight = %d, %d; want one second of the rate rounded up (3) for both",
			l.Burst(), l.InFlight())
	}
	var disabled *IPLimiter
	if release, reason := disabled.Acquire(t.Context(), "1.2.3.4"); reason != "" {
		t.Fatalf("disabled limiter refused: %s", reason)
	} else {
		release()
	}
	disabled.Charge("1.2.3.4") // must not panic

	now := time.Unix(1_800_000_000, 0)
	l := NewIPLimiter(IPLimiterConfig{Rate: 1, Burst: 2, Now: func() time.Time { return now }})
	r1, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatalf("first: %s", reason)
	}
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatalf("second: %s", reason)
	}
	// Two in flight is the cap, and the bucket is empty either way.
	if _, reason := l.Acquire(t.Context(), "a"); reason != ReasonConcurrency {
		t.Fatalf("third acquire over the burst: reason = %q, want %q", reason, ReasonConcurrency)
	}
	r1()
	if _, reason := l.Acquire(t.Context(), "a"); reason != ReasonRate {
		t.Fatalf("with a slot free but no tokens: reason = %q, want a rate refusal", reason)
	}
	// Another IP has its own bucket.
	rb, reason := l.Acquire(t.Context(), "b")
	if reason != "" {
		t.Fatalf("other ip: %s", reason)
	}
	rb()
	// Time refills.
	now = now.Add(time.Second)
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatalf("after refill: %s", reason)
	}
	// Idle entries are swept once idle past the TTL.
	now = now.Add(IdleTTL + 2*time.Minute)
	l.mu.Lock()
	l.sweepLocked(now)
	if _, ok := l.entries["b"]; ok {
		t.Error("idle entry survived the sweep")
	}
	if _, ok := l.entries["a"]; !ok {
		t.Error("entry with a request in flight was swept")
	}
	l.mu.Unlock()
}

// TestIPLimiterCharge: a charge spends a token without a slot and never
// takes the bucket below empty.
func TestIPLimiterCharge(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewIPLimiter(IPLimiterConfig{Rate: 1, Burst: 2, Now: func() time.Time { return now }})
	l.Charge("a")
	l.Charge("a")
	l.Charge("a") // empty already: no debt
	if _, reason := l.Acquire(t.Context(), "a"); reason != ReasonRate {
		t.Fatalf("after charging the bucket empty: reason = %q, want %q", reason, ReasonRate)
	}
	now = now.Add(time.Second)
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatalf("one second after the charges: %s (a charge must not go into debt)", reason)
	}
}

// TestIPLimiterSeparateInFlight: an in-flight cap below the burst refuses
// at the cap while tokens remain, and a released slot is usable again.
func TestIPLimiterSeparateInFlight(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewIPLimiter(IPLimiterConfig{Rate: 1, Burst: 10, InFlight: 2, Now: func() time.Time { return now }})
	if l.InFlight() != 2 || l.Burst() != 10 {
		t.Fatalf("in-flight, burst = %d, %d; want 2, 10", l.InFlight(), l.Burst())
	}
	r1, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatal(reason)
	}
	if _, reason := l.Acquire(t.Context(), "a"); reason != ReasonConcurrency {
		t.Fatalf("third with tokens left: reason = %q, want %q", reason, ReasonConcurrency)
	}
	r1()
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatalf("after a release, tokens left: %s", reason)
	}
}

// TestIPLimiterWaitsForSlot: with a wait, a caller over the in-flight cap
// queues and is admitted when a slot is released within the wait.
func TestIPLimiterWaitsForSlot(t *testing.T) {
	l := NewIPLimiter(IPLimiterConfig{Rate: 1000, Burst: 1000, InFlight: 1, Wait: 5 * time.Second})
	r1, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatal(reason)
	}
	type result struct {
		release func()
		reason  string
	}
	done := make(chan result, 1)
	go func() {
		rel, why := l.Acquire(t.Context(), "a")
		done <- result{rel, why}
	}()
	waitQueued(t, l, "a", 1)
	select {
	case <-done:
		t.Fatal("queued acquire returned while the slot was held")
	default:
	}
	r1()
	select {
	case got := <-done:
		if got.reason != "" {
			t.Fatalf("queued acquire refused after a release: %s", got.reason)
		}
		got.release()
	case <-time.After(5 * time.Second):
		t.Fatal("queued acquire not admitted after the release")
	}
}

// TestIPLimiterWaitRefusesAtDeadline: a caller queued for a slot that never
// frees is refused for concurrency once the wait passes, and leaves no
// waiter behind.
func TestIPLimiterWaitRefusesAtDeadline(t *testing.T) {
	wait := 50 * time.Millisecond
	l := NewIPLimiter(IPLimiterConfig{Rate: 1000, Burst: 1000, InFlight: 1, Wait: wait})
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatal(reason)
	}
	start := time.Now()
	if _, reason := l.Acquire(t.Context(), "a"); reason != ReasonConcurrency {
		t.Fatalf("reason = %q, want %q", reason, ReasonConcurrency)
	}
	if elapsed := time.Since(start); elapsed < wait {
		t.Fatalf("refused after %v, before the %v wait", elapsed, wait)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.entries["a"].waiting; w != 0 {
		t.Fatalf("waiting = %d after the refusal, want 0", w)
	}
}

// TestIPLimiterWaitHonoursContext: a cancelled caller stops queueing at
// once, whatever is left of the wait.
func TestIPLimiterWaitHonoursContext(t *testing.T) {
	l := NewIPLimiter(IPLimiterConfig{Rate: 1000, Burst: 1000, InFlight: 1, Wait: time.Minute})
	if _, reason := l.Acquire(t.Context(), "a"); reason != "" {
		t.Fatal(reason)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, reason := l.Acquire(ctx, "a"); reason != ReasonConcurrency {
		t.Fatalf("reason = %q, want %q", reason, ReasonConcurrency)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancelled caller queued for %v", elapsed)
	}
}

// TestIPLimiterWaitsForToken: with a wait, an empty bucket queues for the
// next token when it comes within the wait.
func TestIPLimiterWaitsForToken(t *testing.T) {
	// 20 tokens a second: the next token is 50ms away.
	l := NewIPLimiter(IPLimiterConfig{Rate: 20, Burst: 1, InFlight: 10, Wait: 5 * time.Second})
	r1, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatal(reason)
	}
	defer r1()
	start := time.Now()
	r2, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatalf("second acquire refused though a token comes within the wait: %s", reason)
	}
	defer r2()
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("second acquire admitted after %v, without waiting for the token", elapsed)
	}
}

// TestIPLimiterTokenBeyondWaitRefused: a token further away than the wait is
// refused at once for rate, and the slot and the reservation are returned.
func TestIPLimiterTokenBeyondWaitRefused(t *testing.T) {
	// One token a minute; the wait is far shorter.
	l := NewIPLimiter(IPLimiterConfig{Rate: 1.0 / 60, Burst: 1, InFlight: 1, Wait: 50 * time.Millisecond})
	r1, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatal(reason)
	}
	r1()
	start := time.Now()
	if _, reason := l.Acquire(t.Context(), "a"); reason != ReasonRate {
		t.Fatalf("reason = %q, want %q", reason, ReasonRate)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("a refusal it could predict took %v", elapsed)
	}
	l.mu.Lock()
	inflight := l.entries["a"].inflight
	l.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("in flight after the refusal = %d, want 0 (slot returned)", inflight)
	}
}

// TestIPLimiterCancelWhileWaitingForToken: a caller that holds a slot and
// is waiting for its next token, and whose context ends first, is refused
// for rate and gives back both the slot and the reserved token. Without
// either, every cancelled request would leak: a slot (the source is refused
// for concurrency for good once InFlight are gone) or a token (the next
// caller waits a full interval longer).
func TestIPLimiterCancelWhileWaitingForToken(t *testing.T) {
	// Two tokens a second: the next token is 500ms away.
	l := NewIPLimiter(IPLimiterConfig{Rate: 2, Burst: 1, InFlight: 1, Wait: 5 * time.Second})
	start := time.Now()
	r1, reason := l.Acquire(t.Context(), "a")
	if reason != "" {
		t.Fatal(reason)
	}
	r1()

	// Cancel rather than a short deadline: a deadline before the token is
	// refused up front, never reaching the wait this test is about.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)
	waitStart := time.Now()
	if _, reason := l.Acquire(ctx, "a"); reason != ReasonRate {
		t.Fatalf("reason = %q, want %q", reason, ReasonRate)
	}
	if elapsed := time.Since(waitStart); elapsed < 10*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("refused after %v: not the cancel during the token wait", elapsed)
	}
	l.mu.Lock()
	inflight := l.entries["a"].inflight
	l.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("in flight after the cancelled wait = %d, want 0 (slot returned)", inflight)
	}

	// Past the first token's refill but well before a second one: only a
	// returned reservation leaves a token here. A caller that cannot wait
	// for a later token is refused for rate, and one stuck behind a leaked
	// slot for concurrency.
	time.Sleep(time.Until(start.Add(650 * time.Millisecond)))
	ctx2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	r3, reason := l.Acquire(ctx2, "a")
	if reason != "" {
		t.Fatalf("acquire after the cancelled wait refused: %s (token or slot not returned)", reason)
	}
	r3()
}

// TestIPLimiterSweepKeepsWaiters: an entry with a queued caller is never
// swept, even when it looks idle.
func TestIPLimiterSweepKeepsWaiters(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewIPLimiter(IPLimiterConfig{Rate: 1, Burst: 1, Now: func() time.Time { return now }})
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked("a", now)
	e.waiting = 1
	l.sweepLocked(now.Add(IdleTTL + 2*time.Minute))
	if _, ok := l.entries["a"]; !ok {
		t.Fatal("entry with a waiter was swept")
	}
}

// waitQueued blocks until n callers are queued for ip's slot.
func waitQueued(t *testing.T, l *IPLimiter, ip string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		e, ok := l.entries[ip]
		got := 0
		if ok {
			got = e.waiting
		}
		l.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d callers never queued for %s", n, ip)
}
