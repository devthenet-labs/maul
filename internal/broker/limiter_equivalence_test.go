// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// legacyIPLimiter is the broker's per-source limiter exactly as it was
// before it moved to internal/httpguard, kept verbatim as the reference
// TestBrokerLimiterEquivalence holds the new construction to.
type legacyIPLimiter struct {
	limit rate.Limit
	burst int
	now   func() time.Time

	mu        sync.Mutex
	entries   map[string]*legacyIPEntry
	lastSweep time.Time
}

type legacyIPEntry struct {
	bucket   *rate.Limiter
	inflight int
	seen     time.Time
}

func newLegacyIPLimiter(perSecond float64, burst int, now func() time.Time) *legacyIPLimiter {
	if perSecond <= 0 {
		return nil
	}
	if burst < 1 {
		burst = max(1, int(math.Ceil(perSecond)))
	}
	return &legacyIPLimiter{limit: rate.Limit(perSecond), burst: burst, now: now, entries: map[string]*legacyIPEntry{}}
}

func (l *legacyIPLimiter) acquire(ip string) (func(), string) {
	if l == nil {
		return func() {}, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	e := l.entryLocked(ip, now)
	if e.inflight >= l.burst {
		return nil, "too many concurrent requests from this source"
	}
	if !e.bucket.AllowN(now, 1) {
		return nil, "request rate from this source exceeded"
	}
	e.inflight++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		e.inflight--
		e.seen = l.now()
	}, ""
}

func (l *legacyIPLimiter) penalize(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.entryLocked(ip, now).bucket.AllowN(now, 1)
}

func (l *legacyIPLimiter) entryLocked(ip string, now time.Time) *legacyIPEntry {
	e, ok := l.entries[ip]
	if !ok {
		e = &legacyIPEntry{bucket: rate.NewLimiter(l.limit, l.burst)}
		l.entries[ip] = e
	}
	e.seen = now
	return e
}

func (l *legacyIPLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for ip, e := range l.entries {
		if e.inflight == 0 && now.Sub(e.seen) > 10*time.Minute {
			delete(l.entries, ip)
		}
	}
}

// limiterOp is one step of a script both limiters replay.
type limiterOp struct {
	kind string // acquire | release | penalize | advance
	ip   string
	// slot picks which held slot a release returns (modulo the count held).
	slot int
	d    time.Duration
}

func (o limiterOp) String() string {
	switch o.kind {
	case "advance":
		return fmt.Sprintf("advance(%v)", o.d)
	case "release":
		return fmt.Sprintf("release(#%d)", o.slot)
	default:
		return fmt.Sprintf("%s(%s)", o.kind, o.ip)
	}
}

// replayLimiters runs script against the broker's construction and the
// legacy limiter on one shared frozen clock and reports the first step at
// which their acquire outcomes differ.
func replayLimiters(t *testing.T, perSecond float64, burst int, script []limiterOp) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	got := newIPLimiter(perSecond, burst, clock)
	want := newLegacyIPLimiter(perSecond, burst, clock)
	if (got == nil) != (want == nil) {
		t.Fatalf("rate %v burst %d: disabled = %v, legacy disabled = %v", perSecond, burst, got == nil, want == nil)
	}
	var gotHeld, wantHeld []func()
	for i, op := range script {
		switch op.kind {
		case "acquire":
			gr, greason := got.acquire(op.ip)
			wr, wreason := want.acquire(op.ip)
			if greason != wreason {
				t.Fatalf("rate %v burst %d, step %d %v: reason = %q, legacy %q\nscript: %v",
					perSecond, burst, i, op, greason, wreason, script)
			}
			if greason == "" {
				gotHeld = append(gotHeld, gr)
				wantHeld = append(wantHeld, wr)
			}
		case "release":
			if len(gotHeld) == 0 {
				continue
			}
			k := op.slot % len(gotHeld)
			gotHeld[k]()
			wantHeld[k]()
			gotHeld = append(gotHeld[:k], gotHeld[k+1:]...)
			wantHeld = append(wantHeld[:k], wantHeld[k+1:]...)
		case "penalize":
			got.penalize(op.ip)
			want.penalize(op.ip)
		case "advance":
			now = now.Add(op.d)
		}
	}
}

// TestBrokerLimiterEquivalence pins W1's refactor: the broker's limiter,
// now httpguard's with the cap at the burst and no queue, gives the same
// acquire/refuse sequence as the old ipLimiter, penalize included, on a
// fixed script and on seeded random ones.
func TestBrokerLimiterEquivalence(t *testing.T) {
	acq := func(ip string) limiterOp { return limiterOp{kind: "acquire", ip: ip} }
	pen := func(ip string) limiterOp { return limiterOp{kind: "penalize", ip: ip} }
	rel := func(k int) limiterOp { return limiterOp{kind: "release", slot: k} }
	adv := func(d time.Duration) limiterOp { return limiterOp{kind: "advance", d: d} }

	fixed := []limiterOp{
		acq("a"), acq("a"), acq("a"), // the third is over the cap
		rel(0), acq("a"), // a slot, but no token
		pen("b"), acq("b"), acq("b"), // a penalty spends one of b's two
		adv(time.Second), acq("a"), pen("a"), acq("a"),
		rel(0), rel(0), rel(0), adv(500 * time.Millisecond), acq("c"), acq("a"),
		adv(11 * time.Minute), acq("d"), // the sweep runs; a full bucket again
		acq("b"), acq("b"), acq("b"),
	}
	for _, cfg := range []struct {
		rate  float64
		burst int
	}{{1, 2}, {2.5, 0}, {0.5, 1}, {10, 3}, {0, 5}} {
		replayLimiters(t, cfg.rate, cfg.burst, fixed)
	}

	const seed = 20261014
	r := rand.New(rand.NewSource(seed))
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	for range 300 {
		rateVal := []float64{0.01, 0.5, 1, 2.5, 7, 100}[r.Intn(6)]
		burst := r.Intn(5) // 0 = unset
		script := make([]limiterOp, 40+r.Intn(80))
		for i := range script {
			switch n := r.Intn(10); {
			case n < 5:
				script[i] = acq(ips[r.Intn(len(ips))])
			case n < 7:
				script[i] = rel(r.Intn(8))
			case n < 8:
				script[i] = pen(ips[r.Intn(len(ips))])
			default:
				ds := []time.Duration{
					10 * time.Millisecond, 300 * time.Millisecond, time.Second, 3 * time.Second,
					time.Minute, 9 * time.Minute, 11 * time.Minute,
				}
				script[i] = adv(ds[r.Intn(len(ds))])
			}
		}
		replayLimiters(t, rateVal, burst, script)
	}
}
