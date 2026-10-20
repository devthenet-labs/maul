// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// testMetrics installs, once per test binary, a ManualReader-backed global
// MeterProvider; the package's lazily created instruments delegate to it.
var testMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// sum adds the data points of the named int64 sum whose attributes include
// every pair in attrs.
func sum(t *testing.T, name string, attrs map[string]string) int64 {
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
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
		point:
			for _, dp := range s.DataPoints {
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

// TestMetrics: the request counter by profile, route and outcome, the
// bytes counter, and the in-flight gauge back at zero.
func TestMetrics(t *testing.T) {
	testMetrics()
	goUp := newUpstream(t, okBody("12345"))
	sumUp := newUpstream(t, okBody("tree"))
	h := newHarness(t, Config{}, goUp, sumUp)
	probes := []struct {
		name  string
		attrs map[string]string
		delta int64
	}{
		{"maul.egress_proxy.requests", map[string]string{"profile": "go", "route": "go", "outcome": "proxied"}, 2},
		{"maul.egress_proxy.requests", map[string]string{"profile": "go", "route": "sumdb", "outcome": "proxied"}, 2},
		{"maul.egress_proxy.requests", map[string]string{"profile": "go", "route": "go", "outcome": "not_allowed"}, 1},
		{"maul.egress_proxy.requests", map[string]string{"profile": "go", "route": "none", "outcome": "bad_path"}, 1},
		{"maul.egress_proxy.requests", map[string]string{"profile": "go", "outcome": "method"}, 1},
		// Both uuid answers and the refusal body of rsc.io/quote.
		{"maul.egress_proxy.bytes", map[string]string{"profile": "go", "route": "go"}, -1},
		{"maul.egress_proxy.bytes", map[string]string{"profile": "go", "route": "sumdb"}, 4},
		{"maul.egress_proxy.inflight", map[string]string{"profile": "go"}, 0},
	}
	before := make([]int64, len(probes))
	for i, p := range probes {
		before[i] = sum(t, p.name, p.attrs)
	}
	goBytes := int64(len(h.get(t, "/go/github.com/google/uuid/@v/list").body))
	goBytes += int64(len(h.get(t, "/go/github.com/google/uuid/@v/v1.6.0.info").body))
	h.get(t, "/go/sumdb/sum.golang.org/latest")
	h.get(t, "/go/sumdb/sum.golang.org/supported")
	goBytes += int64(len(h.get(t, "/go/rsc.io/quote/@v/list").body))
	h.get(t, "/go/nothing")
	h.do(t, "go", "POST", "/go/github.com/google/uuid/@v/list", nil)
	for i, p := range probes {
		if p.delta < 0 {
			p.delta = goBytes
		}
		if got := sum(t, p.name, p.attrs) - before[i]; got != p.delta {
			t.Errorf("%s %v grew by %d, want %d", p.name, p.attrs, got, p.delta)
		}
	}
}
