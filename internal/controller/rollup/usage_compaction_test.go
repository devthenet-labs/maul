// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package rollup

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/stats"
)

func usageRollup(name string, days ...string) *v1alpha1.UsageRollup {
	r := &v1alpha1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "patchy"},
		Spec:       v1alpha1.UsageRollupSpec{Scope: v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: "org"}},
		Status: v1alpha1.UsageRollupStatus{
			Days:   map[string]v1alpha1.UsageBucket{},
			Months: map[string]v1alpha1.UsageMonthBucket{},
		},
	}
	for _, d := range days {
		b := v1alpha1.UsageBucket{UsageAggregate: v1alpha1.UsageAggregate{Runs: 1}}
		r.Status.Days[d] = b
		m := r.Status.Months[d[:7]]
		m.Runs++
		r.Status.Months[d[:7]] = m
	}
	return r
}

// TestUsageCompactor: a pass drops day buckets past the 400-day window and
// keeps every month; a rollup with nothing to drop is never written; a
// second pass rewrites nothing.
func TestUsageCompactor(t *testing.T) {
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -v1alpha1.UsageRetentionDays).Format("2006-01-02")
	kept := now.AddDate(0, 0, -(v1alpha1.UsageRetentionDays - 1)).Format("2006-01-02")
	stale := usageRollup("usage-user-stale", old, kept, "2026-10-15")
	fresh := usageRollup("usage-user-fresh", "2026-10-15")
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithStatusSubresource(&v1alpha1.UsageRollup{}).WithObjects(stale, fresh).Build()
	var before v1alpha1.UsageRollup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(fresh), &before); err != nil {
		t.Fatal(err)
	}
	comp := &UsageCompactor{Client: c, Reader: c, Namespace: "patchy", Now: func() time.Time { return now }}

	n, err := comp.Compact(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Compact = %d, %v; want 1 rewritten", n, err)
	}
	var got v1alpha1.UsageRollup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Status.Days[old]; ok || len(got.Status.Days) != 2 {
		t.Errorf("days = %v", got.Status.Days)
	}
	var months int64
	for _, m := range got.Status.Months {
		months += m.Runs
	}
	if months != 3 {
		t.Errorf("month runs = %d, want 3 (compaction never drops a month's total)", months)
	}
	var after v1alpha1.UsageRollup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(fresh), &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("a rollup with nothing to compact was written")
	}
	if n, err := comp.Compact(context.Background()); err != nil || n != 0 {
		t.Errorf("second pass = %d, %v; want nothing", n, err)
	}
}

// TestUsageCompactorAfterApply: a rollup built by the aggregation arithmetic
// compacts exactly as CompactUsage says, once the clock moves past a day.
func TestUsageCompactorAfterApply(t *testing.T) {
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	r := usageRollup("usage-user-a")
	edge := now.AddDate(0, 0, -(v1alpha1.UsageRetentionDays - 1))
	stats.ApplyUsage(&r.Status, "u:1", stats.UsageDelta{Kind: stats.UsageKindPlan, CompletedAt: edge,
		Source: v1alpha1.UsageSourceMeasured, Usage: v1alpha1.UsageAggregate{Runs: 1}}, "", true, now)
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithStatusSubresource(&v1alpha1.UsageRollup{}).WithObjects(r).Build()
	comp := &UsageCompactor{Client: c, Reader: c, Namespace: "patchy",
		Now: func() time.Time { return now.Add(24 * time.Hour) }}
	if n, err := comp.Compact(context.Background()); err != nil || n != 1 {
		t.Fatalf("Compact = %d, %v", n, err)
	}
	var got v1alpha1.UsageRollup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(r), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Days) != 0 || got.Status.Months[edge.Format("2006-01")].Runs != 1 ||
		len(got.Status.Recent) != 1 {
		t.Errorf("status = %+v", got.Status)
	}
}

// TestUsageCompactorStart: Start compacts at once and returns when its
// context ends.
func TestUsageCompactorStart(t *testing.T) {
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -v1alpha1.UsageRetentionDays-3).Format("2006-01-02")
	r := usageRollup("usage-user-b", old)
	c := fake.NewClientBuilder().WithScheme(kube.Scheme()).
		WithStatusSubresource(&v1alpha1.UsageRollup{}).WithObjects(r).Build()
	comp := &UsageCompactor{Client: c, Reader: c, Namespace: "patchy", Interval: time.Hour,
		Now: func() time.Time { return now }}
	if !comp.NeedLeaderElection() {
		t.Error("compaction must run on the leader only")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- comp.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got v1alpha1.UsageRollup
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(r), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Status.Days) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start did not compact")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start = %v", err)
	}
}
