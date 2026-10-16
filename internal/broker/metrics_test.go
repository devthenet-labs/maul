// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// testMetrics installs, once per test binary, a ManualReader-backed global
// MeterProvider. The package's instruments are created lazily from the
// global provider, which delegates to the one installed here even for
// instruments created before it.
var testMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// counter sums the data points of the named int64 counter whose attributes
// include every pair in attrs.
func counter(t *testing.T, name string, attrs map[string]string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
		point:
			for _, dp := range sum.DataPoints {
				for k, v := range attrs {
					if got, ok := dp.Attributes.Value(attribute.Key(k)); !ok || got.AsString() != v {
						continue point
					}
				}
				total += dp.Value
			}
		}
	}
	return total
}

// TestMetrics: the request, pre-authentication, token and cost counters
// record what the broker did, by route, outcome, reason and class.
func TestMetrics(t *testing.T) {
	testMetrics()
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(100, 5, true))
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{"foundry": {Target: mustTarget(t, up.URL)}}}, nil)
	h := s.Handler()
	// Model "m" is unpriced, so it is charged at the ceiling price. Each
	// response reports input 100, cache writes 10, cache reads 20, output 5.
	observed := s.ledger.prices.cost("m", usage{input: 100, cacheCreation: 10, cacheRead: 20, output: 5})
	if observed <= 0 {
		t.Fatalf("ceiling price of one response = %d micro-USD, want > 0", observed)
	}

	type probe struct {
		name  string
		attrs map[string]string
		want  int64
	}
	probes := []probe{
		{"patchy.broker.requests", map[string]string{"route": "foundry", "outcome": "proxied"}, 2},
		{"patchy.broker.requests", map[string]string{"route": "foundry", "outcome": "preauth_token"}, 1},
		{"patchy.broker.requests", map[string]string{"route": "foundry", "outcome": "surface"}, 1},
		{"patchy.broker.preauth.rejections", map[string]string{"reason": "token_shape"}, 1},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "input"}, 200},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "cache_creation"}, 20},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "cache_read"}, 40},
		{"patchy.broker.tokens", map[string]string{"route": "foundry", "class": "output"}, 10},
		{"patchy.broker.cost", map[string]string{"route": "foundry", "class": "observed"}, 2 * observed},
		{"patchy.broker.cost", map[string]string{"route": "foundry", "class": "estimated"}, 0},
	}
	before := make([]int64, len(probes))
	for i, p := range probes {
		before[i] = counter(t, p.name, p.attrs)
	}

	for range 2 {
		if rec := post(h, "/foundry/v1/messages", `{"model":"m","max_tokens":10}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	}
	if rec := post(h, "/foundry/v1/files", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("off-surface: status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/foundry/v1/models", nil)
	req.Header.Set(TokenHeader, "junk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("junk token: status = %d", rec.Code)
	}

	for i, p := range probes {
		if got := counter(t, p.name, p.attrs) - before[i]; got != p.want {
			t.Errorf("%s%v grew by %d, want %d", p.name, p.attrs, got, p.want)
		}
	}
}

// TestCostMetricCutStream: a stream cut before message_stop records its
// observed cost and, separately, the estimated cost of the shortfall, priced
// at the output rate of the tokens the broker estimated.
func TestCostMetricCutStream(t *testing.T) {
	testMetrics()
	var hits atomic.Int64
	up := countingUpstream(t, &hits, "text/event-stream", sseUsage(100, 5, false))
	s := newTestServer(t, Config{Upstreams: map[string]Upstream{"anthropic": {Target: mustTarget(t, up.URL)}}}, nil)
	h := s.Handler()

	observedAttrs := map[string]string{"route": "anthropic", "class": "observed"}
	estimatedAttrs := map[string]string{"route": "anthropic", "class": "estimated"}
	beforeObserved := counter(t, "patchy.broker.cost", observedAttrs)
	beforeEstimated := counter(t, "patchy.broker.cost", estimatedAttrs)
	beforeTokens := counter(t, "patchy.broker.tokens", estimatedAttrs)

	if rec := post(h, "/anthropic/v1/messages", `{"model":"m","max_tokens":10}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	estimatedTokens := counter(t, "patchy.broker.tokens", estimatedAttrs) - beforeTokens
	if estimatedTokens <= 0 {
		t.Fatalf("cut stream estimated %d tokens, want > 0", estimatedTokens)
	}
	wantObserved := s.ledger.prices.cost("m", usage{input: 100, cacheCreation: 10, cacheRead: 20, output: 5})
	wantEstimated := s.ledger.prices.outputCost("m", estimatedTokens)
	if wantEstimated <= 0 {
		t.Fatalf("estimated cost of %d tokens = %d micro-USD, want > 0", estimatedTokens, wantEstimated)
	}
	if got := counter(t, "patchy.broker.cost", observedAttrs) - beforeObserved; got != wantObserved {
		t.Errorf("observed cost grew by %d, want %d", got, wantObserved)
	}
	if got := counter(t, "patchy.broker.cost", estimatedAttrs) - beforeEstimated; got != wantEstimated {
		t.Errorf("estimated cost grew by %d, want %d", got, wantEstimated)
	}
}
