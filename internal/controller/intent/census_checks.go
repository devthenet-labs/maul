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
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The check-era census. 0.14.18 retired saved checks, their replays and
// the check reviewer, and withdrew, once per open intent, a verification an
// earlier release had judged under them (the supersede); this release
// removes their fields from the CRDs (the API server prunes them from every
// stored object) and no longer runs the supersede. So an install must pass
// through 0.14.18. Before the manager starts, intent-controller counts what
// 0.14.18 would still have had to settle, through a client.Reader (it
// cannot write), reports the count as maul.intent.check_era_records, and
// refuses to start while anything is counted (CheckEraCensus.Err), naming
// each object and its fix (docs/intents/evaluator.md#migrating-from-saved-checks).
// Every shape it keys on survives the pruning: the judge marker, what a
// judgement holds beside the retired fields, the VerificationSuperseded
// condition, and a run's stage spelling, which the CRD keeps.

// retiredStageCheckReview is the check reviewer's stage spelling: retired
// with saved checks, kept in the IntentRun stage enum only so a stored run
// still validates (v1alpha1.RetiredIntentStages). No run of it is created
// or launched; one an earlier release left unfinished is the census's.
const retiredStageCheckReview v1alpha1.IntentStage = "checkreview"

// CheckEraCensus is what the census found in the controller's namespace,
// each list sorted by name.
type CheckEraCensus struct {
	// Namespace is the namespace the census was taken in.
	Namespace string
	// Intents are the open Intents whose verification an earlier release
	// judged and 0.14.18 has not superseded (heldByEarlierRules): read by
	// this release, its result would stand as if judged under today's
	// rules. Whether the evaluator verifies the Project today does not
	// matter: it is judged again once it does. A terminal Intent's record
	// is never judged again (its comments keep the earlier release's
	// words), and one being deleted is going: neither is counted.
	Intents []string
	// Runs are the checkreview IntentRuns not yet settled: 0.14.18 stops
	// them (its Job deleted, the run Aborted); this release does not know
	// the stage. A settled one is kept for its spend and read as an
	// evaluation's; one being deleted is not counted.
	Runs []string
}

// Count is how many check-era records the census found.
func (c CheckEraCensus) Count() int { return len(c.Intents) + len(c.Runs) }

// errCheckEraRecords is the census's refusal: check-era records remain.
var errCheckEraRecords = errors.New("records of saved checks remain that only 0.14.18 settles")

// Err is the census's refusal to start, nil when it found nothing: each
// record by namespace and name, with what clears it. Both are cleared by
// running 0.14.18 until it has settled them, or by a delete; an Intent
// 0.14.18 cannot supersede (its Project's evaluator not verifying it, or
// no evaluable outcome, or its fresh round not started) can be cancelled
// there instead. Neither kind holds a finalizer that needs
// intent-controller running, but a run's usage-rollup one: a deleted run
// waits for it, and is not counted meanwhile.
func (c CheckEraCensus) Err() error {
	if c.Count() == 0 {
		return nil
	}
	var b strings.Builder
	for _, name := range c.Intents {
		fmt.Fprintf(&b, "; Intent %s/%s holds a verification an earlier release judged with saved checks, which "+
			"only 0.14.18 withdraws: run 0.14.18 until it has (its comments then say the earlier result was "+
			"withdrawn and a fresh evaluation starts; that needs the evaluator verifying its Project and an "+
			"evaluable outcome), or /maul cancel it under 0.14.18, or delete it (kubectl -n %s delete intent %s)",
			c.Namespace, name, c.Namespace, name)
	}
	for _, name := range c.Runs {
		fmt.Fprintf(&b, "; IntentRun %s/%s is an unfinished check review, a stage this release does not know: "+
			"run 0.14.18, which stops it, or delete it (kubectl -n %s delete intentrun %s)",
			c.Namespace, name, c.Namespace, name)
	}
	return fmt.Errorf("%w (%d)%s", errCheckEraRecords, c.Count(), b.String())
}

// heldByEarlierRules reports a verification an earlier release judged and
// 0.14.18 has not superseded: no judge marker, and either something it
// judged (what supersede withdrew: a round in flight, outcomes, their
// round, heads or environment, anomalies, a pass or fail count) or the
// withdrawal done and its fresh round not started (the
// VerificationSuperseded condition). The retired ladder's own records
// (rung, model round, escalation, disputes, suspicion, quarantine) are
// pruned and read by nothing, so they are not looked for: alone, they
// mean nothing judged.
func heldByEarlierRules(in *v1alpha1.Intent) bool {
	v := in.Status.Verification
	if v == nil || v.Judge == v1alpha1.JudgeObserved {
		return false
	}
	judged := v.Ref != nil || len(v.Outcomes) > 0 || v.OutcomesRound > 0 || v.OutcomesHeadSetDigest != "" ||
		v.OutcomesEnvironment != "" || len(v.Anomalies) > 0 || v.Passed > 0 || v.Failed > 0
	return judged || meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}

// TakeCheckEraCensus lists namespace's Intents and IntentRuns through r and
// returns the check-era records among them. It only reads. A failed list is
// its error, naming what it could not list: both are intent-controller's
// own grants.
func TakeCheckEraCensus(ctx context.Context, r client.Reader, namespace string) (CheckEraCensus, error) {
	var intents v1alpha1.IntentList
	if err := r.List(ctx, &intents, client.InNamespace(namespace)); err != nil {
		return CheckEraCensus{}, fmt.Errorf("list intents for the check-era census: %w", err)
	}
	var runs v1alpha1.IntentRunList
	if err := r.List(ctx, &runs, client.InNamespace(namespace)); err != nil {
		return CheckEraCensus{}, fmt.Errorf("list intentruns for the check-era census: %w", err)
	}
	c := CheckEraCensus{Namespace: namespace}
	for i := range intents.Items {
		in := &intents.Items[i]
		if in.Namespace == namespace && !v1alpha1.IntentTerminal(in.Status.Phase) && in.DeletionTimestamp.IsZero() &&
			heldByEarlierRules(in) {
			c.Intents = append(c.Intents, in.Name)
		}
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Namespace == namespace && run.Spec.Stage == retiredStageCheckReview && run.DeletionTimestamp.IsZero() &&
			!runSettled(run) {
			c.Runs = append(c.Runs, run.Name)
		}
	}
	slices.Sort(c.Intents)
	slices.Sort(c.Runs)
	return c, nil
}

// checkEraMetrics is the check-era census gauge, on censusScope's meter.
var checkEraMetrics = sync.OnceValue(func() censusMetrics {
	m := otel.Meter(censusScope)
	g, err := m.Int64ObservableGauge("maul.intent.check_era_records",
		metric.WithDescription("check-era records intent-controller's startup census found, by kind (intent: "+
			"an open Intent whose verification an earlier release judged with saved checks and 0.14.18 has not "+
			"superseded; intentrun: an unfinished check review); intent-controller does not start while any "+
			"is found"))
	if err != nil {
		otel.Handle(err)
	}
	return censusMetrics{meter: m, gauge: g}
})

// Report publishes c as the maul.intent.check_era_records gauge, by kind,
// for the life of the process: the census is taken once, at startup.
func (c CheckEraCensus) Report() {
	cm := checkEraMetrics()
	points := map[string]int64{"intent": int64(len(c.Intents)), "intentrun": int64(len(c.Runs))}
	if _, err := cm.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, kind := range slices.Sorted(maps.Keys(points)) {
			o.ObserveInt64(cm.gauge, points[kind], metric.WithAttributes(attribute.String("kind", kind)))
		}
		return nil
	}, cm.gauge); err != nil {
		otel.Handle(err)
	}
}
