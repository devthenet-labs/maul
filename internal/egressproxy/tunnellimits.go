// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The defaults behind every zero TunnelLimits field (and the binary's
// --tunnel-* flags; §2.5 of the step-3 spec), set from the live client
// fixtures (TestLiveTunnel). Upload, the exfiltration direction: the
// largest tunnel measured sent 21 KB (pip, one keep-alive tunnel to
// pypi.org) and the largest source 602 KB (yarn, 162 packages, 3.7 KB a
// package), so 1 MiB a tunnel and 8 MiB a source cover every measured
// ecosystem at 4x, and a yarn tree of about 2,000 packages (7.4 MB). The
// concurrency and rate limits only keep one pod from exhausting the proxy
// (the byte budget bounds exfiltration): pnpm 11 and yarn 4 opened 50
// tunnels at once and yarn 183 new ones a second, so the per-source limits
// sit above that and the global one fits eight such pods.
const (
	DefaultTunnelMaxConcurrent        = 512
	DefaultTunnelPerSourceConcurrent  = 64
	DefaultTunnelPerSourceRate        = 50
	DefaultTunnelPerSourceBurst       = 200
	DefaultTunnelLimitWait            = 10 * time.Second
	DefaultTunnelDialTimeout          = 10 * time.Second
	DefaultTunnelHelloTimeout         = 10 * time.Second
	DefaultTunnelIdleTimeout          = 2 * time.Minute
	DefaultTunnelMaxDuration          = 30 * time.Minute
	DefaultTunnelMaxUploadBytes       = 1 << 20
	DefaultTunnelMaxDownloadBytes     = 2 << 30
	DefaultTunnelSourceUploadBudget   = 8 << 20
	DefaultTunnelSourceDownloadBudget = 8 << 30
	DefaultTunnelSourceBudgetIdle     = 2 * time.Hour
	DefaultTunnelMinBudgetIdle        = 2 * time.Hour
	DefaultTunnelMaxSources           = 4096
	DefaultTunnelProgressInterval     = 5 * time.Minute
	tunnelRefusalWriteTimeout         = 10 * time.Second
	tunnelCopyBuffer                  = 32 << 10
	tunnelIdleCheckFloor              = 10 * time.Millisecond
)

// TunnelLimits bounds the CONNECT tunnel: its own concurrency and rate
// (separate from the Go route's request limits, so neither starves the
// other), its durations, its per-tunnel byte caps and its per-source byte
// budgets. A zero field takes its default.
type TunnelLimits struct {
	// MaxConcurrent is the open tunnels across every source.
	MaxConcurrent int
	// PerSourceConcurrent is one source IP's open tunnels.
	PerSourceConcurrent int
	// PerSourceRate and PerSourceBurst are one source IP's new tunnels
	// per second.
	PerSourceRate  float64
	PerSourceBurst int
	// Wait is how long a CONNECT queues for the three above before a 503.
	Wait time.Duration
	// DialTimeout bounds the resolve and connect.
	DialTimeout time.Duration
	// HelloTimeout bounds the wait from the 200 to a complete ClientHello.
	HelloTimeout time.Duration
	// IdleTimeout ends a tunnel with no byte in either direction.
	IdleTimeout time.Duration
	// MaxDuration is one tunnel's absolute lifetime.
	MaxDuration time.Duration
	// MaxUploadBytes and MaxDownloadBytes cap one tunnel's bytes per
	// direction (TLS bytes on the wire); a profile's upload.perTunnel
	// overrides the upload cap.
	MaxUploadBytes, MaxDownloadBytes int64
	// SourceUploadBudget and SourceDownloadBudget cap the bytes of every
	// tunnel of one source IP on one profile, per budget window; a
	// profile's upload.perSource overrides the upload budget.
	SourceUploadBudget, SourceDownloadBudget int64
	// SourceBudgetIdle is how long a source's budget entry outlives its
	// last tunnel. It may not be below MinBudgetIdle.
	SourceBudgetIdle time.Duration
	// MinBudgetIdle is the floor of SourceBudgetIdle: the longest Job
	// deadline, so no pod can pause long enough to refill its own budget.
	MinBudgetIdle time.Duration
	// MaxSources caps the budget table's entries per profile. A full table
	// refuses a new source and never evicts one early.
	MaxSources int
	// ProgressInterval is the period of an open tunnel's progress line;
	// negative turns the line off.
	ProgressInterval time.Duration
	// Deny are further ranges the tunnel's dial refuses, beside the Go
	// route's (Config.Policy.Deny) and the always-denied ranges: the API
	// server's addresses. The Go route never reads them.
	Deny []netip.Prefix
}

// withDefaults fills every zero field and refuses a budget window below
// its floor.
func (l TunnelLimits) withDefaults() (TunnelLimits, error) {
	setInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	setDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	setBytes := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	setInt(&l.MaxConcurrent, DefaultTunnelMaxConcurrent)
	setInt(&l.PerSourceConcurrent, DefaultTunnelPerSourceConcurrent)
	if l.PerSourceRate <= 0 {
		l.PerSourceRate = DefaultTunnelPerSourceRate
	}
	setInt(&l.PerSourceBurst, DefaultTunnelPerSourceBurst)
	setDur(&l.Wait, DefaultTunnelLimitWait)
	setDur(&l.DialTimeout, DefaultTunnelDialTimeout)
	setDur(&l.HelloTimeout, DefaultTunnelHelloTimeout)
	setDur(&l.IdleTimeout, DefaultTunnelIdleTimeout)
	setDur(&l.MaxDuration, DefaultTunnelMaxDuration)
	setBytes(&l.MaxUploadBytes, DefaultTunnelMaxUploadBytes)
	setBytes(&l.MaxDownloadBytes, DefaultTunnelMaxDownloadBytes)
	setBytes(&l.SourceUploadBudget, DefaultTunnelSourceUploadBudget)
	setBytes(&l.SourceDownloadBudget, DefaultTunnelSourceDownloadBudget)
	setDur(&l.SourceBudgetIdle, DefaultTunnelSourceBudgetIdle)
	setDur(&l.MinBudgetIdle, DefaultTunnelMinBudgetIdle)
	setInt(&l.MaxSources, DefaultTunnelMaxSources)
	if l.ProgressInterval == 0 {
		l.ProgressInterval = DefaultTunnelProgressInterval
	}
	if l.SourceBudgetIdle < l.MinBudgetIdle {
		return l, fmt.Errorf("tunnel source budget idle %s is below the minimum %s (the longest Job deadline): "+
			"a pod could pause long enough to refill its own budget", l.SourceBudgetIdle, l.MinBudgetIdle)
	}
	return l, nil
}

// limits is the request limiter's shape for the tunnel's own instance.
func (l TunnelLimits) limits() Limits {
	return Limits{
		MaxConcurrent:       l.MaxConcurrent,
		PerSourceConcurrent: l.PerSourceConcurrent,
		PerSourceRate:       l.PerSourceRate,
		PerSourceBurst:      l.PerSourceBurst,
		Wait:                l.Wait,
	}
}

// The tunnel's limit refusal reasons beside the limiter's (source_rate,
// source_concurrency, global_concurrency), and the per-tunnel cap reason.
const (
	reasonSourceUploadBudget   = "source_upload_budget"
	reasonSourceDownloadBudget = "source_download_budget"
	reasonSourceTableFull      = "source_table_full"
	reasonTunnelCap            = "tunnel"
	reasonTimeout              = "timeout"
)

// direction is one way through a tunnel.
type direction int

const (
	up   direction = iota // client to upstream: the exfiltration direction
	down                  // upstream to client
)

func (d direction) String() string {
	if d == up {
		return "up"
	}
	return "down"
}

// sourceBudget is one source IP's bytes on one profile, kept until
// SourceBudgetIdle after its last tunnel closed.
type sourceBudget struct {
	used      [2]int64
	open      int
	lastClose time.Time
}

// budgetTable is one profile's per-source budgets: bounded, never evicting
// a live entry early (eviction would refill a budget).
type budgetTable struct {
	limit [2]int64
	idle  time.Duration
	max   int
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*sourceBudget
}

func newBudgetTable(p netprofile.Profile, l TunnelLimits, now func() time.Time) *budgetTable {
	t := &budgetTable{
		limit:   [2]int64{l.SourceUploadBudget, l.SourceDownloadBudget},
		idle:    l.SourceBudgetIdle,
		max:     l.MaxSources,
		now:     now,
		entries: map[string]*sourceBudget{},
	}
	if p.Upload != nil && p.Upload.PerSource > 0 {
		t.limit[up] = p.Upload.PerSource
	}
	return t
}

// expired reports whether e may be forgotten: no tunnel open and none for
// a whole window.
func (t *budgetTable) expired(e *sourceBudget, now time.Time) bool {
	return e.open == 0 && now.Sub(e.lastClose) >= t.idle
}

// open admits one tunnel of ip, returning its entry, or the refusal
// reason: the table is full of live entries, or a budget is spent.
func (t *budgetTable) open(ip string) (*sourceBudget, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.entries[ip]
	if e != nil && t.expired(e, now) {
		delete(t.entries, ip)
		e = nil
	}
	if e == nil {
		if len(t.entries) >= t.max {
			for k, other := range t.entries {
				if t.expired(other, now) {
					delete(t.entries, k)
				}
			}
		}
		if len(t.entries) >= t.max {
			return nil, reasonSourceTableFull
		}
		e = &sourceBudget{}
		t.entries[ip] = e
	}
	switch {
	case e.used[up] >= t.limit[up]:
		return nil, reasonSourceUploadBudget
	case e.used[down] >= t.limit[down]:
		return nil, reasonSourceDownloadBudget
	}
	e.open++
	return e, ""
}

// close records the end of one of e's tunnels.
func (t *budgetTable) close(e *sourceBudget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.open--
	e.lastClose = t.now()
}

// take spends up to n bytes of e's budget in d and returns what it
// granted: less than n means the budget ran out.
func (t *budgetTable) take(e *sourceBudget, d direction, n int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	g := min(n, max(t.limit[d]-e.used[d], 0))
	e.used[d] += g
	return g
}
