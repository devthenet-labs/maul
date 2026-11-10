// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The slice-1 census (S4): slice 1's acceptance criteria no longer run, and
// a later release removes their CRD fields, which prunes them from every
// stored object. Before the manager starts, intent-controller counts what
// still records them, through a client.Reader (it cannot write), logs the
// names and reports the count as maul.intent.slice1_records, and starts
// anyway: mode() already stops such an Intent for a replan, and a
// criteria PreviewVerification is never run again. An operator reads the
// census before upgrading past the release that removes the fields
// (docs/intents/evaluator.md#migrating-from-slice-1).

// Slice1Census is what the census found in the controller's namespace,
// each list sorted by name.
type Slice1Census struct {
	// Intents are the non-terminal Intents whose acceptance on record is
	// criteria-era: criteria, criteria beside outcomes, or a record pruned
	// of its criteria (a criteria digest with no outcomes digest). A
	// terminal Intent's record is never judged again; a Failed one revived
	// meets mode()'s replan.
	Intents []string
	// Verifications are the PreviewVerifications asking criteria
	// (spec.criteria), whatever their phase: their spec is immutable, so
	// only deleting them clears them.
	Verifications []string
	// VerificationsUnlisted: the controller may not list
	// PreviewVerifications (an install without the evaluator grants it
	// nothing on them), so Verifications is unknown, not empty.
	VerificationsUnlisted bool
}

// Count is how many slice-1 records the census found.
func (c Slice1Census) Count() int { return len(c.Intents) + len(c.Verifications) }

// TakeSlice1Census lists namespace's Intents and PreviewVerifications
// through r and returns the slice-1 records among them. It only reads. A
// PreviewVerification list RBAC forbids is VerificationsUnlisted; any other
// failed list is its error, naming what it could not list.
func TakeSlice1Census(ctx context.Context, r client.Reader, namespace string) (Slice1Census, error) {
	var intents v1alpha1.IntentList
	if err := r.List(ctx, &intents, client.InNamespace(namespace)); err != nil {
		return Slice1Census{}, fmt.Errorf("list intents for the slice-1 census: %w", err)
	}
	var c Slice1Census
	var pvs v1alpha1.PreviewVerificationList
	switch err := r.List(ctx, &pvs, client.InNamespace(namespace)); {
	case kerrors.IsForbidden(err):
		c.VerificationsUnlisted = true
	case err != nil:
		return Slice1Census{}, fmt.Errorf("list previewverifications for the slice-1 census: %w", err)
	}
	for i := range intents.Items {
		in := &intents.Items[i]
		if in.Namespace != namespace || v1alpha1.IntentTerminal(in.Status.Phase) {
			continue
		}
		switch recordOf(in.Status.Acceptance) {
		case recordCriteria, recordPrunedCriteria, recordMixed:
			c.Intents = append(c.Intents, in.Name)
		}
	}
	for i := range pvs.Items {
		if pv := &pvs.Items[i]; pv.Namespace == namespace && len(pv.Spec.Criteria) > 0 {
			c.Verifications = append(c.Verifications, pv.Name)
		}
	}
	slices.Sort(c.Intents)
	slices.Sort(c.Verifications)
	return c, nil
}

// censusScope identifies the census gauge's meter.
const censusScope = "github.com/devthenet-labs/maul/internal/controller/intent"

// censusMetrics is the slice-1 census gauge and the meter it came from: a
// callback must be registered on the meter that made its instrument.
type censusMetrics struct {
	meter metric.Meter
	gauge metric.Int64ObservableGauge
}

var slice1Metrics = sync.OnceValue(func() censusMetrics {
	m := otel.Meter(censusScope)
	g, err := m.Int64ObservableGauge("maul.intent.slice1_records",
		metric.WithDescription("slice-1 acceptance records intent-controller's startup census found, by kind "+
			"(intent: a non-terminal Intent recording criteria; previewverification: one asking criteria)"))
	if err != nil {
		otel.Handle(err)
	}
	return censusMetrics{meter: m, gauge: g}
})

// Report publishes c as the maul.intent.slice1_records gauge, by kind, for
// the life of the process: the census is taken once, at startup. Unlisted
// PreviewVerifications get no point rather than a zero.
func (c Slice1Census) Report() {
	cm := slice1Metrics()
	points := c.points()
	if _, err := cm.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, kind := range slices.Sorted(maps.Keys(points)) {
			o.ObserveInt64(cm.gauge, points[kind], metric.WithAttributes(attribute.String("kind", kind)))
		}
		return nil
	}, cm.gauge); err != nil {
		otel.Handle(err)
	}
}

// points are the gauge's values by kind: no previewverification point
// while they could not be listed, which a zero would misreport as none.
func (c Slice1Census) points() map[string]int64 {
	points := map[string]int64{"intent": int64(len(c.Intents))}
	if !c.VerificationsUnlisted {
		points["previewverification"] = int64(len(c.Verifications))
	}
	return points
}
