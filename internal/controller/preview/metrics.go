// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// scopeName identifies this package's meter.
const scopeName = "github.com/devthenet-labs/maul/internal/controller/preview"

// Instruments are made once, on first use, from the global meter provider,
// which telemetry.Init has installed by then.
var (
	ingressRefusedCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("maul.preview.ingress.refused",
			metric.WithDescription("preview Ingress writes refused by admission (or RBAC), by slot; "+
				"each waits and is retried at the next poll, never spending a deploy attempt"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	unauthenticatedGauge = sync.OnceValue(func() metric.Int64Gauge {
		g, err := otel.Meter(scopeName).Int64Gauge("maul.preview.ingress.unauthenticated",
			metric.WithDescription("slot Ingresses of the preview class without the pinned sign-in "+
				"annotations of the current or previous key generation, by slot, at the last sweep "+
				"(recorded only while preview auth is required)"))
		if err != nil {
			otel.Handle(err)
		}
		return g
	})
	unauthenticatedDeletedCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("maul.preview.ingress.unauthenticated.deleted",
			metric.WithDescription("slot Ingresses the sweeper deleted after they stayed without the "+
				"pinned sign-in annotations past the grace period, by slot"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	toolsSettledCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("maul.preview.tools.settled",
			metric.WithDescription("outcome verifications whose slot tool pod finished and was collected, "+
				"by phase and reason (ledger-mismatch: a log that did not hold its summary)"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
	evidenceDroppedCounter = sync.OnceValue(func() metric.Int64Counter {
		c, err := otel.Meter(scopeName).Int64Counter("maul.preview.evidence.dropped",
			metric.WithDescription("evidence images of a tool pod's ledger not stored, by reason: malformed "+
				"(bytes not their sha256), uncited (no artifact of its entry), not-png, too-large"))
		if err != nil {
			otel.Handle(err)
		}
		return c
	})
)

func recordToolsSettled(ctx context.Context, phase v1alpha1.PreviewVerificationPhase,
	reason v1alpha1.PreviewVerificationReason) {
	if c := toolsSettledCounter(); c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attribute.String("phase", string(phase)),
			attribute.String("reason", string(reason))))
	}
}

func recordEvidenceDropped(ctx context.Context, reason string, n int) {
	if c := evidenceDroppedCounter(); c != nil {
		c.Add(ctx, int64(n), metric.WithAttributes(attribute.String("reason", reason)))
	}
}

func slotAttr(slot int32) metric.MeasurementOption {
	return metric.WithAttributes(attribute.Int("slot", int(slot)))
}

func recordIngressRefused(ctx context.Context, slot int32) {
	if c := ingressRefusedCounter(); c != nil {
		c.Add(ctx, 1, slotAttr(slot))
	}
}

func recordUnauthenticated(ctx context.Context, slot int32, n int) {
	if g := unauthenticatedGauge(); g != nil {
		g.Record(ctx, int64(n), slotAttr(slot))
	}
}

func recordUnauthenticatedDeleted(ctx context.Context, slot int32) {
	if c := unauthenticatedDeletedCounter(); c != nil {
		c.Add(ctx, 1, slotAttr(slot))
	}
}
