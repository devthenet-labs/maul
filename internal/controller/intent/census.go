// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The slice-1 census (S4): slice 1's acceptance criteria no longer run, and
// their CRD fields are removed, so the API server prunes them from every
// stored object. Before the manager starts, intent-controller counts what
// is still of slice 1's era, through a client.Reader (it cannot write),
// reports the count as maul.intent.slice1_records, and refuses to start
// while anything is counted (Slice1Census.Err), naming each object and its
// fix (docs/intents/evaluator.md#migrating-from-slice-1). Both shapes it
// keys on survive the pruning: an Intent's criteria Digest with no outcomes
// digest, and a PreviewVerification with no outcomes.

// Slice1Census is what the census found in the controller's namespace,
// each list sorted by name.
type Slice1Census struct {
	// Namespace is the namespace the census was taken in.
	Namespace string
	// Intents are the non-terminal Intents, of a Project the agent
	// evaluator verifies, whose acceptance on record is criteria-era
	// (recordCriteriaEra: a criteria digest with no outcomes digest, its
	// criteria pruned or never recorded). Such an Intent is never verified
	// again: it stops for a replan. A terminal Intent's record is never
	// judged again, and one of a Project the evaluator does not verify is
	// recorded by its digest alone, which nothing runs and the pruning does
	// not change: neither is counted. Nor is an Intent being deleted.
	Intents []string
	// Verifications are the PreviewVerifications evaluating no outcomes:
	// slice 1's criteria requests (spec.criteria, pruned), whatever their
	// phase. Their spec is immutable, so only deleting them clears them. One
	// being deleted is not counted.
	Verifications []string
	// VerificationsUnlisted: the controller may not list
	// PreviewVerifications (an install without the evaluator grants it
	// nothing on them), so Verifications is unknown, not empty. The chart's
	// pre-upgrade guard lists them instead.
	VerificationsUnlisted bool
}

// Count is how many slice-1 records the census found.
func (c Slice1Census) Count() int { return len(c.Intents) + len(c.Verifications) }

// errSlice1Records is the census's refusal: slice-1 records remain.
var errSlice1Records = errors.New("slice-1 acceptance records remain")

// Err is the census's refusal to start, nil when it found nothing: each
// record by namespace and name, with what clears it. None can be cleared
// through intent-controller, which does not start while they remain, so
// each fix is a delete, or going back to how intent-controller last ran
// (the previous release, or without the evaluator verifying the intent's
// Project) to cancel an intent there. Neither kind holds a finalizer of
// intent-controller's, so a delete completes without it.
func (c Slice1Census) Err() error {
	if c.Count() == 0 {
		return nil
	}
	var b strings.Builder
	for _, name := range c.Intents {
		fmt.Fprintf(&b, "; Intent %s/%s records slice 1's acceptance criteria, which nothing runs, under the "+
			"evaluator: delete it (kubectl -n %s delete intent %s), or go back to how intent-controller last ran "+
			"(the previous release, or without the evaluator verifying its Project), /maul cancel it there (a new "+
			"issue starts over with outcomes) and try again",
			c.Namespace, name, c.Namespace, name)
	}
	for _, name := range c.Verifications {
		fmt.Fprintf(&b, "; PreviewVerification %s/%s asks slice 1's criteria and its spec is immutable: "+
			"delete it (kubectl -n %s delete previewverification %s)", c.Namespace, name, c.Namespace, name)
	}
	return fmt.Errorf("%w (%d)%s", errSlice1Records, c.Count(), b.String())
}

// TakeSlice1Census lists s.Namespace's Projects, Intents and
// PreviewVerifications through r and returns the slice-1 records among
// them. It only reads. A PreviewVerification list RBAC forbids is
// VerificationsUnlisted; any other failed list is its error, naming what it
// could not list.
func TakeSlice1Census(ctx context.Context, r client.Reader, s Settings) (Slice1Census, error) {
	namespace := s.Namespace
	var projects v1alpha1.ProjectList
	if err := r.List(ctx, &projects, client.InNamespace(namespace)); err != nil {
		return Slice1Census{}, fmt.Errorf("list projects for the slice-1 census: %w", err)
	}
	var intents v1alpha1.IntentList
	if err := r.List(ctx, &intents, client.InNamespace(namespace)); err != nil {
		return Slice1Census{}, fmt.Errorf("list intents for the slice-1 census: %w", err)
	}
	c := Slice1Census{Namespace: namespace}
	var pvs v1alpha1.PreviewVerificationList
	switch err := r.List(ctx, &pvs, client.InNamespace(namespace)); {
	case kerrors.IsForbidden(err):
		c.VerificationsUnlisted = true
	case err != nil:
		return Slice1Census{}, fmt.Errorf("list previewverifications for the slice-1 census: %w", err)
	}
	evaluated := map[string]bool{}
	for i := range projects.Items {
		if p := &projects.Items[i]; p.Namespace == namespace {
			evaluated[p.Name] = s.evaluates(p)
		}
	}
	for i := range intents.Items {
		in := &intents.Items[i]
		if in.Namespace != namespace || v1alpha1.IntentTerminal(in.Status.Phase) || !in.DeletionTimestamp.IsZero() ||
			!evaluated[in.Spec.Project] {
			continue
		}
		if recordOf(in.Status.Acceptance) == recordCriteriaEra {
			c.Intents = append(c.Intents, in.Name)
		}
	}
	for i := range pvs.Items {
		if pv := &pvs.Items[i]; pv.Namespace == namespace && pv.DeletionTimestamp.IsZero() &&
			len(pv.Spec.Outcomes) == 0 {
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
			"(intent: a non-terminal Intent the evaluator verifies recording criteria; previewverification: "+
			"one asking criteria); intent-controller does not start while any is found"))
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
