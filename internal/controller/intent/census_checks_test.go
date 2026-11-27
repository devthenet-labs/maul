// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// checkEraIntent is an open Intent whose verification is v, as the API
// server returns it once the check-era fields are pruned.
func checkEraIntent(name string, phase v1alpha1.IntentPhase, v *v1alpha1.IntentVerification) *v1alpha1.Intent {
	in := censusIntent(name, phase, &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest,
		OutcomesDigest: "sha256:" + strings.Repeat("b", 64)})
	in.Status.Verification = v
	return in
}

// withdrawn is an Intent 0.14.18 withdrew and has not yet started the fresh
// round for: no marker, nothing judged, the VerificationSuperseded condition.
func withdrawn(in *v1alpha1.Intent) *v1alpha1.Intent {
	in.Status.Conditions = append(in.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionVerificationSuperseded, Status: metav1.ConditionTrue, Reason: reasonRulesChanged})
	return in
}

// checkEraRun is an IntentRun of stage in phase.
func checkEraRun(name string, stage v1alpha1.IntentStage, phase v1alpha1.RunPhase) *v1alpha1.IntentRun {
	run := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.IntentRunSpec{Stage: stage}}
	run.Status.Phase = phase
	return run
}

// TestCheckEraCensus: the startup census of the check-era leftovers counts
// every open Intent whose verification carries no judge marker and still
// holds what an earlier release judged (a round in flight, outcomes, their
// round, heads or environment, anomalies, a pass or fail count), or was
// withdrawn by 0.14.18 and waits for its fresh round (the
// VerificationSuperseded condition with no marker), and every unfinished
// checkreview IntentRun, in the controller's namespace only. It never
// counts: a marked verification (0.14.18 superseded it, or this release
// wrote it), one marked and still carrying the condition (its fresh round
// started), an unmarked one with nothing judged (the retired ladder's
// records alone are pruned, and nothing reads them), a terminal Intent, an
// Intent being deleted, an Intent with no verification, a finished
// checkreview run, a run of any other stage, one being deleted, or another
// namespace's. Unlike the slice-1 census it does not look at the Project:
// a record the evaluator does not verify today is judged again once it
// does.
func TestCheckEraCensus(t *testing.T) {
	ref := &v1alpha1.ObjectReference{Name: "x-v2-a1"}
	judged := func(mod func(*v1alpha1.IntentVerification)) *v1alpha1.IntentVerification {
		v := &v1alpha1.IntentVerification{Round: 2}
		mod(v)
		return v
	}
	marked := func(v *v1alpha1.IntentVerification) *v1alpha1.IntentVerification {
		v.Judge = v1alpha1.JudgeObserved
		return v
	}
	otherNS := checkEraIntent("elsewhere", v1alpha1.IntentInReview, &v1alpha1.IntentVerification{Ref: ref})
	otherNS.Namespace = "other"
	otherRun := checkEraRun("elsewhere-cr-r1-a1", retiredStageCheckReview, v1alpha1.RunRunning)
	otherRun.Namespace = "other"

	tests := []struct {
		name        string
		objs        []client.Object
		wantIntents []string
		wantRuns    []string
	}{
		{name: "a fresh install"},
		{name: "only this release's records", objs: []client.Object{
			censusProject("target", true),
			checkEraIntent("a", v1alpha1.IntentInReview, marked(&v1alpha1.IntentVerification{Ref: ref, Passed: 1})),
			withdrawn(checkEraIntent("b", v1alpha1.IntentInReview, marked(&v1alpha1.IntentVerification{Ref: ref}))),
			checkEraIntent("c", v1alpha1.IntentInReview, nil),
			checkEraIntent("d", v1alpha1.IntentRevising, &v1alpha1.IntentVerification{Round: 3}),
			checkEraRun("a-ev-r1-a1", v1alpha1.IntentStageEvaluate, v1alpha1.RunRunning),
			checkEraRun("a-plan-r1-a1", v1alpha1.IntentStagePlan, ""),
		}},
		{name: "leftovers", objs: []client.Object{
			censusProject("target", true),
			checkEraIntent("in-flight", v1alpha1.IntentInReview, judged(func(v *v1alpha1.IntentVerification) {
				v.Ref = ref
			})),
			checkEraIntent("outcomes", v1alpha1.IntentInReview, judged(func(v *v1alpha1.IntentVerification) {
				v.Outcomes = []v1alpha1.OutcomeResult{{ID: "version-served"}}
			})),
			checkEraIntent("outcomes-round", v1alpha1.IntentRevising, judged(func(v *v1alpha1.IntentVerification) {
				v.OutcomesRound = 2
			})),
			checkEraIntent("outcomes-heads", v1alpha1.IntentBlocked, judged(func(v *v1alpha1.IntentVerification) {
				v.OutcomesHeadSetDigest = "sha256:" + strings.Repeat("c", 64)
			})),
			checkEraIntent("environment", v1alpha1.IntentInReview, judged(func(v *v1alpha1.IntentVerification) {
				v.OutcomesEnvironment = "OK"
			})),
			checkEraIntent("anomalies", v1alpha1.IntentInReview, judged(func(v *v1alpha1.IntentVerification) {
				v.Anomalies = []v1alpha1.AnomalyRecord{{Summary: "slow"}}
			})),
			checkEraIntent("passed", v1alpha1.IntentInReview, judged(func(v *v1alpha1.IntentVerification) {
				v.Passed = 1
			})),
			checkEraIntent("failed", v1alpha1.IntentInReview, judged(func(v *v1alpha1.IntentVerification) {
				v.Failed = 1
			})),
			withdrawn(checkEraIntent("withdrawn", v1alpha1.IntentInReview, &v1alpha1.IntentVerification{Round: 2})),
			checkEraIntent("merged", v1alpha1.IntentMerged, &v1alpha1.IntentVerification{Ref: ref, Passed: 1}),
			checkEraIntent("closed", v1alpha1.IntentClosed, &v1alpha1.IntentVerification{Ref: ref}),
			checkEraIntent("failed-intent", v1alpha1.IntentFailed, &v1alpha1.IntentVerification{Passed: 1}),
			deleting(checkEraIntent("going", v1alpha1.IntentInReview, &v1alpha1.IntentVerification{Ref: ref})),
			otherNS, otherRun,
			checkEraRun("in-flight-cr-r1-a1", retiredStageCheckReview, v1alpha1.RunRunning),
			checkEraRun("in-flight-cr-r1-a2", retiredStageCheckReview, v1alpha1.RunPending),
			checkEraRun("in-flight-cr-r1-a3", retiredStageCheckReview, ""),
			checkEraRun("in-flight-cr-r0-a1", retiredStageCheckReview, v1alpha1.RunComplete),
			checkEraRun("in-flight-cr-r0-a2", retiredStageCheckReview, v1alpha1.RunFailed),
			deleting(checkEraRun("going-cr-r1-a1", retiredStageCheckReview, v1alpha1.RunRunning)),
		}, wantIntents: []string{"anomalies", "environment", "failed", "in-flight", "outcomes", "outcomes-heads",
			"outcomes-round", "passed", "withdrawn"},
			wantRuns: []string{"in-flight-cr-r1-a1", "in-flight-cr-r1-a2", "in-flight-cr-r1-a3"}},
		{name: "a Project the evaluator does not verify", objs: []client.Object{
			censusProject("target", false),
			checkEraIntent("off", v1alpha1.IntentInReview, &v1alpha1.IntentVerification{Passed: 2}),
		}, wantIntents: []string{"off"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TakeCheckEraCensus(context.Background(), censusClient(t, tc.objs...), testNS)
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			if !slices.Equal(got.Intents, tc.wantIntents) || !slices.Equal(got.Runs, tc.wantRuns) {
				t.Errorf("census = %+v, want intents %v and runs %v", got, tc.wantIntents, tc.wantRuns)
			}
			if got.Count() != len(tc.wantIntents)+len(tc.wantRuns) {
				t.Errorf("count = %d", got.Count())
			}
			if (got.Err() == nil) != (got.Count() == 0) {
				t.Errorf("Err() = %v with %d records", got.Err(), got.Count())
			}
		})
	}
}

// TestCheckEraCensusErr: the refusal names every record by namespace and
// name with both of its fixes (passing through 0.14.18, or a delete), and
// is errCheckEraRecords.
func TestCheckEraCensusErr(t *testing.T) {
	err := CheckEraCensus{Namespace: "maul", Intents: []string{"beatbattle-48"},
		Runs: []string{"beatbattle-48-cr-r1-a1"}}.Err()
	if !errors.Is(err, errCheckEraRecords) {
		t.Fatalf("Err() = %v, want errCheckEraRecords", err)
	}
	for _, want := range []string{"(2)", "Intent maul/beatbattle-48", "0.14.18",
		"kubectl -n maul delete intent beatbattle-48", "/maul cancel",
		"IntentRun maul/beatbattle-48-cr-r1-a1", "kubectl -n maul delete intentrun beatbattle-48-cr-r1-a1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %q, want it to name %q", err, want)
		}
	}
	if err := (CheckEraCensus{Namespace: "maul"}).Err(); err != nil {
		t.Errorf("Err() of an empty census = %v", err)
	}
}

// TestCheckEraCensusListFailure: a list the API server refuses is the
// census's error, naming what it could not list; nothing is counted. Both
// lists are the controller's own grants, so a refusal is never read as
// none found.
func TestCheckEraCensusListFailure(t *testing.T) {
	refused := errors.New("forbidden")
	for _, kind := range []string{"intents", "intentruns"} {
		r := censusClient(t, checkEraIntent("x", v1alpha1.IntentInReview, &v1alpha1.IntentVerification{Passed: 1}))
		r = interceptor.NewClient(r.(client.WithWatch), interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				var this string
				switch list.(type) {
				case *v1alpha1.IntentList:
					this = "intents"
				case *v1alpha1.IntentRunList:
					this = "intentruns"
				}
				if this == kind {
					return refused
				}
				return c.List(ctx, list, opts...)
			},
		})
		got, err := TakeCheckEraCensus(context.Background(), r, testNS)
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), kind) || got.Count() != 0 {
			t.Errorf("%s refused: census %+v, %v; want the refusal naming it and nothing counted", kind, got, err)
		}
	}
}

// TestCheckEraCensusGauge: Report publishes the census as
// maul.intent.check_era_records, one point per kind.
func TestCheckEraCensusGauge(t *testing.T) {
	got := gaugePoints(t, "maul.intent.check_era_records", func() {
		CheckEraCensus{Intents: []string{"a", "b"}, Runs: []string{"a-cr-r1-a1"}}.Report()
	})
	if want := map[string]int64{"intent": 2, "intentrun": 1}; !maps.Equal(got, want) {
		t.Errorf("gauge = %v, want %v", got, want)
	}
}
