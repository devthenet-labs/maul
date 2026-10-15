// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"sync"
	"time"

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

// countRequest records one request. extra is the /r/ routes' ecosystem
// and registry; a /go request passes none, so its data point keeps step
// 1's attribute set.
func countRequest(ctx context.Context, profile, route, outcome string, bytes int64, extra ...attribute.KeyValue) {
	attrs := append([]attribute.KeyValue{
		attribute.String("profile", profile),
		attribute.String("route", route),
		attribute.String("outcome", outcome)}, extra...)
	requestsCounter().Add(ctx, 1, metric.WithAttributes(attrs...))
	if bytes > 0 {
		bytesCounter().Add(ctx, bytes, metric.WithAttributes(
			attribute.String("profile", profile),
			attribute.String("route", route)))
	}
}

func countInflight(ctx context.Context, profile string, delta int64) {
	inflightCounter().Add(ctx, delta, metric.WithAttributes(attribute.String("profile", profile)))
}

// The registry instruments (step 2).
var (
	refreshesCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.egress_proxy.credential.refreshes",
			metric.WithDescription("registry credential loads, reloads, mints and failures by registry, kind "+
				"and result"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	linksCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("patchy.egress_proxy.index.links",
			metric.WithDescription("pip simple-page links rewritten or dropped by registry"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
)

// CountCredentialRefresh records one credential refresh outcome: the
// credfile and awsca sources' OnRefresh.
func CountCredentialRefresh(ctx context.Context, registry string, kind CredentialKind, result string) {
	refreshesCounter().Add(ctx, 1, metric.WithAttributes(
		attribute.String("registry", registry),
		attribute.String("kind", string(kind)),
		attribute.String("result", result)))
}

// countLinks records a pip page's rewritten and dropped links.
func countLinks(ctx context.Context, registry string, rewritten, dropped int) {
	for result, n := range map[string]int{"rewritten": rewritten, "dropped": dropped} {
		if n > 0 {
			linksCounter().Add(ctx, int64(n), metric.WithAttributes(
				attribute.String("registry", registry),
				attribute.String("result", result)))
		}
	}
}

// ExpiryFunc reports a minted credential's expiry; ok is false before the
// first mint.
type ExpiryFunc func(registry string) (expires time.Time, ok bool)

// RegisterCredentialExpiry registers patchy.egress_proxy.credential.expiry,
// the seconds until each listed registry's current token expires, observed
// on every collection through expiry (static credentials are not listed:
// they do not expire). The returned function unregisters it.
func RegisterCredentialExpiry(registries []string, expiry ExpiryFunc, now func() time.Time) (func() error, error) {
	m := otel.Meter(scopeName)
	g, err := m.Int64ObservableGauge("patchy.egress_proxy.credential.expiry",
		metric.WithDescription("seconds until the registry's current minted credential expires"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, r := range registries {
			if exp, ok := expiry(r); ok {
				o.ObserveInt64(g, int64(exp.Sub(now()).Seconds()), metric.WithAttributes(
					attribute.String("registry", r)))
			}
		}
		return nil
	}, g)
	if err != nil {
		return nil, err
	}
	return reg.Unregister, nil
}
