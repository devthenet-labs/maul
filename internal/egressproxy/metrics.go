// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// scopeName identifies this package's meter.
const scopeName = "github.com/bitwise-media-group/patchy/internal/egressproxy"

// The proxy's instruments: every request by profile, route and outcome, the
// response bytes written by profile and route, and the requests in flight
// per profile.
var (
	requestsCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.egress_proxy.requests",
			metric.WithDescription("egress proxy requests by profile, route and outcome"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	bytesCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.egress_proxy.bytes",
			metric.WithDescription("response bytes written by profile and route"), metric.WithUnit("By"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	inflightCounter = sync.OnceValue(func() metric.Int64UpDownCounter {
		c, err := otel.Meter(scopeName).Int64UpDownCounter("patchy.egress_proxy.inflight",
			metric.WithDescription("egress proxy requests in flight by profile"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
)

func countRequest(ctx context.Context, profile, route, outcome string, bytes int64) {
	requestsCounter().Add(ctx, 1, metric.WithAttributes(
		attribute.String("profile", profile),
		attribute.String("route", route),
		attribute.String("outcome", outcome)))
	if bytes > 0 {
		bytesCounter().Add(ctx, bytes, metric.WithAttributes(
			attribute.String("profile", profile),
			attribute.String("route", route)))
	}
}

func countInflight(ctx context.Context, profile string, delta int64) {
	inflightCounter().Add(ctx, delta, metric.WithAttributes(attribute.String("profile", profile)))
}
