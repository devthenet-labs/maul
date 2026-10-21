// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package rollup

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/stats"
)

// DefaultCompactionInterval is how often the UsageRollups are compacted.
const DefaultCompactionInterval = time.Hour

// UsageCompactor enforces every UsageRollup's retention (stats.CompactUsage):
// day buckets past the window are dropped, month buckets kept. The run
// controllers aggregate; the rollup loop only compacts, so a scope no run
// touches any more still loses its old days. It reads uncached (a list per
// pass, and a get per object it rewrites), so it holds no informer on
// UsageRollups.
type UsageCompactor struct {
	// Client writes the compacted status.
	Client client.Client
	// Reader lists and reads UsageRollups uncached (the API reader).
	Reader client.Reader
	// Namespace the UsageRollups live in.
	Namespace string
	// Interval between passes; zero takes DefaultCompactionInterval.
	Interval time.Duration
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Log receives diagnostics; nil discards.
	Log *slog.Logger
}

// NeedLeaderElection keeps compaction to the elected replica.
func (c *UsageCompactor) NeedLeaderElection() bool { return true }

// Start compacts once at start, then every Interval, until ctx ends.
func (c *UsageCompactor) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultCompactionInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := c.Compact(ctx); err != nil {
			c.log().LogAttrs(ctx, slog.LevelWarn, "usage rollup compaction failed", slog.Any("error", err))
		} else if n > 0 {
			c.log().LogAttrs(ctx, slog.LevelInfo, "usage rollups compacted", slog.Int("rollups", n))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Compact runs one pass, returning how many rollups it rewrote. A rollup
// with nothing past retention is never written.
func (c *UsageCompactor) Compact(ctx context.Context) (int, error) {
	var list v1alpha1.UsageRollupList
	if err := c.Reader.List(ctx, &list, client.InNamespace(c.Namespace)); err != nil {
		return 0, err
	}
	now := c.now()
	rewritten := 0
	for i := range list.Items {
		item := &list.Items[i]
		if !stats.CompactUsage(item.Status.DeepCopy(), now) {
			continue
		}
		wrote := false
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur v1alpha1.UsageRollup
			if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(item), &cur); err != nil {
				return client.IgnoreNotFound(err)
			}
			if !stats.CompactUsage(&cur.Status, now) {
				return nil
			}
			if err := c.Client.Status().Update(ctx, &cur); err != nil {
				return err
			}
			wrote = true
			return nil
		})
		if err != nil {
			return rewritten, err
		}
		if wrote {
			rewritten++
		}
	}
	return rewritten, nil
}

func (c *UsageCompactor) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *UsageCompactor) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}
