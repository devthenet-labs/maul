// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"time"

	"github.com/devthenet-labs/maul/internal/httpguard"
)

// The defaults behind every zero Limits field (and the binary's flags).
const (
	DefaultMaxConcurrent       = 128
	DefaultPerSourceConcurrent = 32
	DefaultPerSourceRate       = 100
	DefaultPerSourceBurst      = 200
	DefaultLimitWait           = 30 * time.Second
	DefaultRequestTimeout      = 10 * time.Minute
	DefaultHeaderTimeout       = 2 * time.Minute
	// DefaultMaxZipBytes is golang.org/x/mod/zip.MaxZipFile.
	DefaultMaxZipBytes = 500 << 20
)

// The fixed body caps per kind (the zip cap is Limits.MaxZipBytes).
const (
	maxModBytes      = 16 << 20 // modfile / zip.MaxGoMod
	maxInfoBytes     = 64 << 10 // .info and @latest
	maxListBytes     = 4 << 20
	maxSumLatestByte = 8 << 10
	maxLookupBytes   = 64 << 10
	maxTileBytes     = 1 << 20
)

// Limits bounds the proxy's concurrency, per-source rate and time. A zero
// field takes its default.
type Limits struct {
	// MaxConcurrent is the global in-flight cap.
	MaxConcurrent int
	// PerSourceConcurrent is one source IP's in-flight cap.
	PerSourceConcurrent int
	// PerSourceRate and PerSourceBurst are one source IP's token bucket.
	PerSourceRate  float64
	PerSourceBurst int
	// Wait is how long a request queues for any of the three before a 503.
	Wait time.Duration
	// RequestTimeout bounds a whole request, the body stream included.
	RequestTimeout time.Duration
	// HeaderTimeout bounds the wait for the upstream's response headers.
	HeaderTimeout time.Duration
	// MaxZipBytes caps a module zip.
	MaxZipBytes int64
}

// withDefaults fills every zero (or negative) field.
func (l Limits) withDefaults() Limits {
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = DefaultMaxConcurrent
	}
	if l.PerSourceConcurrent <= 0 {
		l.PerSourceConcurrent = DefaultPerSourceConcurrent
	}
	if l.PerSourceRate <= 0 {
		l.PerSourceRate = DefaultPerSourceRate
	}
	if l.PerSourceBurst <= 0 {
		l.PerSourceBurst = DefaultPerSourceBurst
	}
	if l.Wait <= 0 {
		l.Wait = DefaultLimitWait
	}
	if l.RequestTimeout <= 0 {
		l.RequestTimeout = DefaultRequestTimeout
	}
	if l.HeaderTimeout <= 0 {
		l.HeaderTimeout = DefaultHeaderTimeout
	}
	if l.MaxZipBytes <= 0 {
		l.MaxZipBytes = DefaultMaxZipBytes
	}
	return l
}

// bodyCap is the response-body cap for a request.
func (l Limits) bodyCap(r Request) int64 {
	switch {
	case r.Route == RouteSumDB && r.Kind == KindLatest:
		return maxSumLatestByte
	case r.Kind == KindZip:
		return l.MaxZipBytes
	case r.Kind == KindMod:
		return maxModBytes
	case r.Kind == KindList:
		return maxListBytes
	case r.Kind == KindLookup:
		return maxLookupBytes
	case r.Kind == KindTile:
		return maxTileBytes
	default: // .info, @latest
		return maxInfoBytes
	}
}

// The limit refusal reasons, for the audit line.
const (
	reasonGlobal      = "global_concurrency"
	reasonConcurrency = "source_concurrency"
	reasonRate        = "source_rate"
)

// limiter is the global semaphore plus the per-source limiter.
type limiter struct {
	global chan struct{}
	wait   time.Duration
	source *httpguard.IPLimiter
}

func newLimiter(l Limits, now func() time.Time) *limiter {
	return &limiter{
		global: make(chan struct{}, l.MaxConcurrent),
		wait:   l.Wait,
		source: httpguard.NewIPLimiter(httpguard.IPLimiterConfig{
			Rate:     l.PerSourceRate,
			Burst:    l.PerSourceBurst,
			InFlight: l.PerSourceConcurrent,
			Wait:     l.Wait,
			Now:      now,
		}),
	}
}

// acquire takes a per-source slot and token, then a global slot, queueing
// for all three within one wait. It returns the release, or the refusal
// reason.
func (l *limiter) acquire(ctx context.Context, ip string) (func(), string) {
	ctx, cancel := context.WithTimeout(ctx, l.wait)
	defer cancel()
	release, reason := l.source.Acquire(ctx, ip)
	if reason != "" {
		if reason == httpguard.ReasonRate {
			return nil, reasonRate
		}
		return nil, reasonConcurrency
	}
	select {
	case l.global <- struct{}{}:
		return func() { <-l.global; release() }, ""
	case <-ctx.Done():
	}
	release()
	return nil, reasonGlobal
}
