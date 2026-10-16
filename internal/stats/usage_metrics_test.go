// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package stats

import (
	"context"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestRecordUsage: patchy.usage.tokens and patchy.usage.cost carry the
// user, project and kind of the run. A metered run emits its token classes
// under its source (zero classes skipped) and its list and subscription
// costs in USD; a reported run emits only its pod's own totals, under class
// and pricing "reported", so trusted sums can leave it out.
func TestRecordUsage(t *testing.T) {
	testMetrics()
	ctx := context.Background()
	user := "github-id:" + t.Name() // deltas below keep -count=N exact
	base := map[string]string{"user": user, "project": "orders", "kind": UsageKindBuild}
	with := func(kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	type check struct {
		name  string
		attrs map[string]string
		delta float64
	}
	checks := []check{
		{"patchy.usage.tokens", with("source", "measured", "class", "input"), 100},
		{"patchy.usage.tokens", with("source", "measured", "class", "output"), 40},
		{"patchy.usage.tokens", with("source", "measured", "class", "cache_read"), 0},
		{"patchy.usage.tokens", with("source", "measured", "class", "cache_write"), 7},
		{"patchy.usage.tokens", with("source", "partial", "class", "input"), 5},
		{"patchy.usage.tokens", with("class", "reported"), 30},
		{"patchy.usage.tokens", with("source", "reported", "class", "input"), 0},
		{"patchy.usage.cost", with("pricing", "list"), 1.5},
		{"patchy.usage.cost", with("pricing", "subscription"), 0.25},
		{"patchy.usage.cost", with("pricing", "reported"), 0.75},
	}
	before := make([]float64, len(checks))
	for i, c := range checks {
		before[i] = sumOf(t, c.name, c.attrs)
	}
	RecordUsage(ctx, user, UsageDelta{Kind: UsageKindBuild, Project: "orders", Source: v1alpha1.UsageSourceMeasured,
		Usage: v1alpha1.UsageAggregate{Runs: 1, InputTokens: 100, OutputTokens: 40, CacheWriteTokens: 7,
			CostMicroUSD: 1_500_000}})
	RecordUsage(ctx, user, UsageDelta{Kind: UsageKindBuild, Project: "orders", Source: v1alpha1.UsageSourcePartial,
		Usage: v1alpha1.UsageAggregate{Runs: 1, PartialRuns: 1, InputTokens: 5, NotionalCostMicroUSD: 250_000}})
	// Reported: the pod's own figures only, even were trusted fields set.
	RecordUsage(ctx, user, UsageDelta{Kind: UsageKindBuild, Project: "orders", Source: v1alpha1.UsageSourceReported,
		Usage: v1alpha1.UsageAggregate{Runs: 1, ReportedRuns: 1, ReportedTokens: 30, ReportedCostMicroUSD: 750_000}})
	for i, c := range checks {
		if got := sumOf(t, c.name, c.attrs) - before[i]; got != c.delta {
			t.Errorf("%s %v: delta = %v, want %v", c.name, c.attrs, got, c.delta)
		}
	}
	// Another user's sums are untouched.
	if got := sumOf(t, "patchy.usage.tokens", map[string]string{"user": user + "-other"}); got != 0 {
		t.Errorf("another user's tokens = %v", got)
	}
}
