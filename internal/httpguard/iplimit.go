// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpguard

import (
	"context"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// IdleTTL is how long an idle source IP's entry is kept. A swept source
// starts again with a full bucket.
const IdleTTL = 10 * time.Minute

// sweepEvery bounds how often the idle sweep runs.
const sweepEvery = time.Minute

// The refusal reasons Acquire returns; callers put them in the response and
// the audit line.
const (
	ReasonConcurrency = "too many concurrent requests from this source"
	ReasonRate        = "request rate from this source exceeded"
)

// IPLimiterConfig configures an IPLimiter.
type IPLimiterConfig struct {
	// Rate is the per-source token rate, per second. Not positive disables
	// the limiter: NewIPLimiter returns nil.
	Rate float64
	// Burst is the bucket size. Below one it is one second of Rate,
	// rounded up and at least one, so a rate alone never serialises a
	// source's parallel requests.
	Burst int
	// InFlight caps a source's concurrent requests. Below one it is the
	// effective Burst, which is the broker's semantics: the cap is the
	// burst.
	InFlight int
	// Wait is how long Acquire queues for an in-flight slot and then a
	// token before refusing. Zero refuses at once, without queueing.
	Wait time.Duration
	// Now is the clock the buckets and the idle sweep read; nil is
	// time.Now. Queueing (Wait > 0) always waits on real timers.
	Now func() time.Time
}

// IPLimiter is the per-source-IP token bucket and in-flight cap. A nil
// *IPLimiter is disabled: Acquire always succeeds and Charge does nothing.
type IPLimiter struct {
	limit    rate.Limit
	burst    int
	inflight int
	wait     time.Duration
	now      func() time.Time

	mu        sync.Mutex
	entries   map[string]*ipEntry
	lastSweep time.Time
}

type ipEntry struct {
	bucket   *rate.Limiter
	inflight int
	// waiting counts Acquire calls queued for a slot; an entry with a
	// waiter is never swept.
	waiting int
	seen    time.Time
	// freed is closed (and cleared) when a slot is released, waking the
	// queued Acquire calls; nil while nobody waits.
	freed chan struct{}
}

// NewIPLimiter returns nil when cfg.Rate is not positive.
func NewIPLimiter(cfg IPLimiterConfig) *IPLimiter {
	if cfg.Rate <= 0 {
		return nil
	}
	burst := cfg.Burst
	if burst < 1 {
		burst = max(1, int(math.Ceil(cfg.Rate)))
	}
	inflight := cfg.InFlight
	if inflight < 1 {
		inflight = burst
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &IPLimiter{
		limit:    rate.Limit(cfg.Rate),
		burst:    burst,
		inflight: inflight,
		wait:     max(0, cfg.Wait),
		now:      now,
		entries:  map[string]*ipEntry{},
	}
}

// Burst is the effective bucket size.
func (l *IPLimiter) Burst() int { return l.burst }

// InFlight is the effective per-source concurrency cap.
func (l *IPLimiter) InFlight() int { return l.inflight }

// Acquire takes one in-flight slot and one token for ip, returning the
// release for the slot or the refusal reason (ReasonConcurrency or
// ReasonRate). With Wait zero it never blocks and ctx is not consulted: a
// full cap or an empty bucket is refused at once, and a refusal for the cap
// spends no token. With Wait positive it queues for a slot, then for a
// token, until Wait has passed or ctx is done, whichever is first; a refusal
// returns the slot and any reserved token.
func (l *IPLimiter) Acquire(ctx context.Context, ip string) (func(), string) {
	if l == nil {
		return func() {}, ""
	}
	if l.wait > 0 {
		return l.acquireWaiting(ctx, ip)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	e := l.entryLocked(ip, now)
	if e.inflight >= l.inflight {
		return nil, ReasonConcurrency
	}
	if !e.bucket.AllowN(now, 1) {
		return nil, ReasonRate
	}
	e.inflight++
	return l.releaser(e), ""
}

// acquireWaiting is Acquire with a queue: a slot first, so a queued caller
// spends no token, then a token reservation it waits out.
func (l *IPLimiter) acquireWaiting(ctx context.Context, ip string) (func(), string) {
	ctx, cancel := context.WithTimeout(ctx, l.wait)
	defer cancel()

	l.mu.Lock()
	now := l.now()
	l.sweepLocked(now)
	e := l.entryLocked(ip, now)
	for e.inflight >= l.inflight {
		if e.freed == nil {
			e.freed = make(chan struct{})
		}
		freed := e.freed
		e.waiting++
		l.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			l.mu.Lock()
			e.waiting--
			e.seen = l.now()
			l.mu.Unlock()
			return nil, ReasonConcurrency
		}
		l.mu.Lock()
		e.waiting--
	}
	e.inflight++
	release := l.releaser(e)
	now = l.now()
	res := e.bucket.ReserveN(now, 1)
	l.mu.Unlock()

	if !res.OK() {
		release()
		return nil, ReasonRate
	}
	delay := res.DelayFrom(now)
	if delay <= 0 {
		return release, ""
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
		res.CancelAt(now)
		release()
		return nil, ReasonRate
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return release, ""
	case <-ctx.Done():
		res.CancelAt(l.now())
		release()
		return nil, ReasonRate
	}
}

// releaser returns the release for one slot of e. Releasing wakes every
// queued Acquire for the source; they race for the freed slot under the
// lock.
func (l *IPLimiter) releaser(e *ipEntry) func() {
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		e.inflight--
		e.seen = l.now()
		if e.freed != nil {
			close(e.freed)
			e.freed = nil
		}
	}
}

// Charge takes one token from ip's bucket if one is there, never waiting
// and taking no slot: a caller-side penalty, such as the broker's charge
// for a failed authentication.
func (l *IPLimiter) Charge(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.entryLocked(ip, now).bucket.AllowN(now, 1)
}

// entryLocked returns ip's entry, creating a full bucket on first sight.
func (l *IPLimiter) entryLocked(ip string, now time.Time) *ipEntry {
	e, ok := l.entries[ip]
	if !ok {
		e = &ipEntry{bucket: rate.NewLimiter(l.limit, l.burst)}
		l.entries[ip] = e
	}
	e.seen = now
	return e
}

// sweepLocked drops idle entries (nothing in flight, nobody queued, unseen
// for IdleTTL); at most once a minute.
func (l *IPLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for ip, e := range l.entries {
		if e.inflight == 0 && e.waiting == 0 && now.Sub(e.seen) > IdleTTL {
			delete(l.entries, ip)
		}
	}
}
