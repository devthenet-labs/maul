// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagerollup

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

var testMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// usageSum adds up the named usage counter's points carrying attrs.
func usageSum(t *testing.T, name string, attrs map[string]string) float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	match := func(set attribute.Set) bool {
		for k, v := range attrs {
			if got, ok := set.Value(attribute.Key(k)); !ok || got.AsString() != v {
				return false
			}
		}
		return true
	}
	var total float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					if match(dp.Attributes) {
						total += float64(dp.Value)
					}
				}
			case metricdata.Sum[float64]:
				for _, dp := range data.DataPoints {
					if match(dp.Attributes) {
						total += dp.Value
					}
				}
			}
		}
	}
	return total
}

// TestSettleRecordsUsageOnce: settling a run emits patchy.usage.tokens and
// patchy.usage.cost once, attributed to its user, project and kind, even
// when a crash between the rollup writes and the run's marker replays the
// whole settle; the user x project and project scopes never emit again.
func TestSettleRecordsUsageOnce(t *testing.T) {
	testMetrics()
	attrs := map[string]string{"user": "github-id:42", "project": "web", "kind": "build"}
	tokens := func() float64 {
		return usageSum(t, "patchy.usage.tokens", attrs)
	}
	input := func() float64 {
		return usageSum(t, "patchy.usage.tokens", map[string]string{"user": "github-id:42", "project": "web",
			"kind": "build", "source": "measured", "class": "input"})
	}
	cost := func() float64 {
		return usageSum(t, "patchy.usage.cost", map[string]string{"user": "github-id:42", "project": "web",
			"kind": "build", "pricing": "list"})
	}
	beforeTokens, beforeInput, beforeCost := tokens(), input(), cost()

	run := intentRun(alice, v1alpha1.RunComplete, measured(true))
	crashed := false
	c := newClientWith(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*v1alpha1.IntentRun); ok && !crashed {
				crashed = true
				return errors.New("crash")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}, intent(), run)
	a := aggregator(c, true)
	if err := Settle(context.Background(), a, client.ObjectKeyFromObject(run), IntentRuns(c)); err == nil {
		t.Fatal("the crash was not reported")
	}
	settle(t, a, c, run) // the replay
	settle(t, a, c, run) // nothing owed any more

	// measured(true): 100 input and 20 output tokens metered, $0.50 list.
	if got := tokens() - beforeTokens; got != 120 {
		t.Errorf("usage tokens delta = %v, want 120 (one run)", got)
	}
	if got := input() - beforeInput; got != 100 {
		t.Errorf("input tokens delta = %v, want 100", got)
	}
	if got := cost() - beforeCost; got != 0.5 {
		t.Errorf("list cost delta = %v, want 0.5", got)
	}
}
