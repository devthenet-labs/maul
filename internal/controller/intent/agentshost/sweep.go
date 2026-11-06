// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// Sweeper deletes orphaned tool Jobs (and their Secrets): every Job of
// maul.run/role tools in the agents namespace whose agents verification is
// gone, or settled at least linger ago; and every one while the agents host
// is off (Enabled false), which serves none. It selects by role, never by
// the intent kind the Jobs also carry. It runs on the leader, every
// Interval.
type Sweeper struct {
	// Client lists the verifications in Namespace (the release namespace).
	Client    client.Reader
	Tools     ToolPods
	Namespace string
	Enabled   bool
	Interval  time.Duration
	Now       func() time.Time
	Log       *slog.Logger
}

// NeedLeaderElection reports true: one sweeper per release.
func (*Sweeper) NeedLeaderElection() bool { return true }

// Start sweeps every Interval until ctx ends; a failed sweep is logged and
// retried at the next tick.
func (s *Sweeper) Start(ctx context.Context) error {
	ticker := time.NewTicker(max(s.Interval, time.Second))
	defer ticker.Stop()
	for {
		if err := s.SweepOnce(ctx); err != nil && ctx.Err() == nil {
			s.log().LogAttrs(ctx, slog.LevelWarn, "tool pod sweep failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// SweepOnce deletes the orphaned tool Jobs once.
func (s *Sweeper) SweepOnce(ctx context.Context) error {
	list, err := s.Tools.List(ctx)
	if err != nil {
		return err
	}
	live := map[types.UID]bool{}
	if s.Enabled {
		var pvs v1alpha1.PreviewVerificationList
		if err := s.Client.List(ctx, &pvs, client.InNamespace(s.Namespace)); err != nil {
			return err
		}
		for i := range pvs.Items {
			pv := &pvs.Items[i]
			if pv.Spec.IsAgents() && pv.DeletionTimestamp.IsZero() && !s.lingered(pv) {
				live[pv.UID] = true
			}
		}
	}
	for i := range list {
		j := &list[i]
		if uid := types.UID(j.Labels[v1alpha1.LabelPreviewVerification]); uid != "" && live[uid] {
			continue
		}
		if !j.DeletionTimestamp.IsZero() {
			continue
		}
		s.log().LogAttrs(ctx, slog.LevelInfo, "deleting an orphaned tool pod", slog.String("job", j.Name),
			slog.String("verification", j.Labels[v1alpha1.LabelPreviewVerification]))
		if err := s.Tools.Delete(ctx, j.Name); err != nil {
			return err
		}
	}
	return nil
}

// lingered reports a settled verification whose linger is over (or that
// records no settle time).
func (s *Sweeper) lingered(pv *v1alpha1.PreviewVerification) bool {
	if !settled(pv.Status.Phase) {
		return false
	}
	return pv.Status.CompletedAt == nil || !s.now().Before(pv.Status.CompletedAt.Add(linger))
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sweeper) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}
