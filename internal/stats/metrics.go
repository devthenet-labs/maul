// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package stats

import (
	"context"
	"slices"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// scopeName identifies this package's meter.
const scopeName = "github.com/bitwise-media-group/patchy/internal/stats"

// The instruments fire at rollup time — exactly once per run/finding,
// aligned with the CR counters. Anyone with an OTLP backend gets the same
// numbers the FindingRollup objects carry; the CRs remain the durable
// record. `repo` is the only high-cardinality attribute (~estate size);
// constrained backends can drop it with a metric view.
var (
	stageRuns = sync.OnceValue(func() metric.Int64Counter {
		c, _ := otel.Meter(scopeName).Int64Counter("patchy.stage.runs",
			metric.WithDescription("agent runs by stage/harness/model/outcome"))
		return c
	})
	stageTokens = sync.OnceValue(func() metric.Int64Counter {
		c, _ := otel.Meter(scopeName).Int64Counter("patchy.stage.tokens",
			metric.WithDescription("agent tokens by stage and class"))
		return c
	})
	stageCost = sync.OnceValue(func() metric.Float64Counter {
		c, _ := otel.Meter(scopeName).Float64Counter("patchy.stage.cost",
			metric.WithDescription("agent spend"), metric.WithUnit("usd"))
		return c
	})
	findingCompleted = sync.OnceValue(func() metric.Int64Counter {
		c, _ := otel.Meter(scopeName).Int64Counter("patchy.finding.completed",
			metric.WithDescription("findings reaching a terminal phase"))
		return c
	})
	findingDeleted = sync.OnceValue(func() metric.Int64Counter {
		c, _ := otel.Meter(scopeName).Int64Counter("patchy.finding.deleted",
			metric.WithDescription("finding resources deleted"))
		return c
	})
)

// RecordStage emits one run's metrics (called at total-scope aggregation so
// it fires exactly once per run).
func RecordStage(ctx context.Context, d StageDelta, repo string) {
	attrs := metric.WithAttributes(
		attribute.String("stage", d.Stage),
		attribute.String("harness", d.Harness),
		attribute.String("model", d.Model),
		attribute.String("outcome", d.Outcome),
		attribute.String("repo", repo),
	)
	stageRuns().Add(ctx, 1, attrs)
	stageCost().Add(ctx, float64(d.CostMicroUSD)/1e6, attrs)
	for class, n := range map[string]int64{
		"input":          d.InputTokens,
		"output":         d.OutputTokens,
		"cache_read":     d.CacheReadTokens,
		"cache_creation": d.CacheCreationTokens,
	} {
		if n > 0 {
			stageTokens().Add(ctx, n, metric.WithAttributes(
				attribute.String("stage", d.Stage),
				attribute.String("class", class)))
		}
	}
}

// RecordCompletion emits a finding's terminal-entry metrics (first rollup
// only).
func RecordCompletion(ctx context.Context, phase, recommendation, repo string) {
	findingCompleted().Add(ctx, 1, metric.WithAttributes(
		attribute.String("phase", phase),
		attribute.String("recommendation", recommendation),
		attribute.String("repo", repo)))
}

// RecordDeleted emits a finding deletion (reason: ttl or manual).
func RecordDeleted(ctx context.Context, reason string) {
	findingDeleted().Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// The usage instruments fire when a run's own controller folds the run into
// its user UsageRollup, exactly once per run (the rollup ledger decides),
// aligned with the UsageRollup counters. `user` is the attributed-to value
// ("org" or "github-id:<id>"): its cardinality is bounded by headcount.
var (
	usageTokens = sync.OnceValue(func() metric.Int64Counter {
		c, _ := otel.Meter(scopeName).Int64Counter("patchy.usage.tokens",
			metric.WithDescription("agent run tokens by user, project, run kind, source and class"))
		return c
	})
	usageCost = sync.OnceValue(func() metric.Float64Counter {
		c, _ := otel.Meter(scopeName).Float64Counter("patchy.usage.cost",
			metric.WithDescription("agent run cost by user, project, run kind and pricing "+
				"(list, subscription: notional, reported: the pod's own untrusted figure)"),
			metric.WithUnit("usd"))
		return c
	})
)

// RecordUsage emits one run's usage metrics. Metered runs report their
// token classes and their list and subscription costs; a reported run its
// pod's own totals, under source and pricing "reported", so a dashboard can
// keep them out of trusted sums.
func RecordUsage(ctx context.Context, user string, d UsageDelta) {
	source := string(d.Source)
	base := []attribute.KeyValue{
		attribute.String("user", user),
		attribute.String("project", d.Project),
		attribute.String("kind", d.Kind),
	}
	with := func(extra ...attribute.KeyValue) metric.MeasurementOption {
		return metric.WithAttributes(append(slices.Clone(base), extra...)...)
	}
	u := d.Usage
	tokens := map[string]int64{"reported": u.ReportedTokens}
	if d.Trusted() {
		tokens = map[string]int64{
			"input":       u.InputTokens,
			"output":      u.OutputTokens,
			"cache_read":  u.CacheReadTokens,
			"cache_write": u.CacheWriteTokens,
		}
	}
	for class, n := range tokens {
		if n > 0 {
			usageTokens().Add(ctx, n, with(attribute.String("source", source), attribute.String("class", class)))
		}
	}
	for pricing, micro := range map[string]int64{
		string(v1alpha1.UsagePricingList):         u.CostMicroUSD,
		string(v1alpha1.UsagePricingSubscription): u.NotionalCostMicroUSD,
		"reported": u.ReportedCostMicroUSD,
	} {
		if micro > 0 {
			usageCost().Add(ctx, float64(micro)/1e6, with(attribute.String("pricing", pricing)))
		}
	}
}
