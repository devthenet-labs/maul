// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// Stream close reasons, the streams-closed counter's.
const (
	closedPreviewChanged = "preview_changed"
	closedRevoked        = "revoked"
)

// tracked is one proxied request in flight: a stream, an upgraded
// connection or an ordinary request still being answered. Cancelling it
// closes the upstream side, which ends the proxy's copy and so the viewer's
// connection.
type tracked struct {
	label, uid, project string
	slot                int
	identity            previewauth.Identity
	cancel              context.CancelFunc
}

// tracker holds every proxied request in flight. Each one's context already
// ends at its host session's absolute end; the tracker is how a re-check or
// a Preview change ends it sooner.
type tracker struct {
	mu    sync.Mutex
	next  uint64
	conns map[uint64]*tracked
}

func newTracker() *tracker { return &tracker{conns: map[uint64]*tracked{}} }

func (t *tracker) add(c *tracked) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	t.conns[t.next] = c
	return t.next
}

func (t *tracker) remove(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, id)
}

func (t *tracker) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// snapshot copies the tracked requests, optionally only one label's.
func (t *tracker) snapshot(label string) map[uint64]*tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[uint64]*tracked, len(t.conns))
	for id, c := range t.conns {
		if label == "" || c.label == label {
			out[id] = c
		}
	}
	return out
}

// close cancels one tracked request and drops it.
func (t *tracker) close(ctx context.Context, id uint64, c *tracked, reason string) {
	c.cancel()
	t.remove(id)
	count(ctx, streamsClosedCounter, attribute.String("reason", reason))
}

// Recheck re-checks every proxied request in flight against its Preview and
// its viewer's access review, as a new request would be, and closes those
// that would now be refused: the Preview is gone or replaced (its UID or slot
// changed), or the review says no. A failed lookup or review keeps the
// connection, as a new request would answer 503 and keep the cookie. Answers
// are shared per label and per viewer, so a busy page costs one review.
func (s *Server) Recheck(ctx context.Context) {
	if s.gw != nil {
		s.gw.recheck(ctx, "")
	}
}

// PreviewChanged re-checks the requests in flight to label's host at once;
// the Preview informer calls it on every update and delete.
func (s *Server) PreviewChanged(ctx context.Context, label string) {
	if s.gw != nil && previewauth.ValidLabel(label) {
		s.gw.recheck(ctx, label)
	}
}

// RunRechecks re-checks every proxied request in flight every interval
// until ctx ends. It returns at once when the gateway is off.
func (s *Server) RunRechecks(ctx context.Context) {
	if s.gw == nil {
		return
	}
	tick := time.NewTicker(s.gw.cfg.Recheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.gw.recheck(ctx, "")
		}
	}
}

func (g *gateway) recheck(ctx context.Context, label string) {
	conns := g.streams.snapshot(label)
	views := map[string]previewauth.View{}
	lookupFailed := map[string]bool{}
	reviews := map[string]bool{}
	for id, c := range conns {
		v, seen := views[c.label]
		if !seen && !lookupFailed[c.label] {
			var err error
			if v, err = g.s.cfg.Lookup.ByLabel(ctx, c.label); err != nil {
				lookupFailed[c.label] = true
			} else {
				views[c.label] = v
			}
		}
		if lookupFailed[c.label] {
			continue
		}
		b := previewauth.Bound{Client: previewauth.ClientID(c.slot), Slot: c.slot, UID: c.uid, Label: c.label}
		if b.Matches(v) != nil {
			g.streams.close(ctx, id, c, closedPreviewChanged)
			continue
		}
		key := reviewKey(c.identity, v.Project)
		allowed, decided := reviews[key]
		if !decided {
			ok, err := g.s.review(ctx, c.identity, v)
			if err != nil && !errors.Is(err, previewauth.ErrNoProject) {
				continue // unknown: keep the connection, as a new request would
			}
			allowed = err == nil && ok
			reviews[key] = allowed
		}
		if !allowed {
			g.streams.close(ctx, id, c, closedRevoked)
		}
	}
}

func reviewKey(id previewauth.Identity, project string) string {
	key := project + "\x00" + id.Username
	for _, gr := range id.Groups {
		key += "\x00" + gr
	}
	return key
}
