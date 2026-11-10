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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
)

// censusIntent is an Intent in phase whose acceptance on record is acc.
func censusIntent(name string, phase v1alpha1.IntentPhase, acc *v1alpha1.IntentAcceptance) *v1alpha1.Intent {
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
	in.Status.Phase, in.Status.Acceptance = phase, acc
	return in
}

// censusPV is a PreviewVerification asking criteria when criteria is set,
// outcomes otherwise.
func censusPV(name string, criteria bool) *v1alpha1.PreviewVerification {
	pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
	if criteria {
		pv.Spec.Criteria = []v1alpha1.AcceptanceCriterion{{ID: "version-served"}}
		return pv
	}
	pv.Spec.Outcomes = []v1alpha1.AcceptanceOutcome{{ID: "version-served"}}
	return pv
}

// TestSlice1Census: the startup census (S4) counts every non-terminal
// Intent whose acceptance on record is criteria-era (criteria, criteria
// beside outcomes, or a record pruned of its criteria: a criteria digest
// with no outcomes digest) and every PreviewVerification asking criteria,
// in the controller's namespace only. Outcome records, empty records,
// records of another namespace and terminal Intents are not counted. It
// only reads: every write the client is asked fails the test.
func TestSlice1Census(t *testing.T) {
	criteria := &v1alpha1.IntentAcceptance{Digest: "sha256:" + strings.Repeat("a", 64),
		Criteria: []v1alpha1.AcceptanceCriterion{{ID: "version-served"}}}
	pruned := &v1alpha1.IntentAcceptance{Digest: "sha256:" + strings.Repeat("a", 64)}
	criteriaDigest, outcomesDigest := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	mixed := &v1alpha1.IntentAcceptance{Digest: criteriaDigest, OutcomesDigest: outcomesDigest,
		Criteria: []v1alpha1.AcceptanceCriterion{{ID: "version-served"}}}
	outcomes := &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest, OutcomesDigest: outcomesDigest}
	empty := &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest}
	other := censusIntent("elsewhere", v1alpha1.IntentInReview, criteria)
	other.Namespace = "other"
	otherPV := censusPV("elsewhere-v1", true)
	otherPV.Namespace = "other"

	tests := []struct {
		name        string
		objs        []client.Object
		wantIntents []string
		wantPVs     []string
	}{
		{name: "none"},
		{name: "outcomes only", objs: []client.Object{
			censusIntent("a", v1alpha1.IntentInReview, outcomes),
			censusIntent("b", v1alpha1.IntentRevising, empty),
			censusIntent("c", v1alpha1.IntentPending, nil),
			censusPV("a-v1", false),
		}},
		{name: "some", objs: []client.Object{
			censusIntent("crit", v1alpha1.IntentInReview, criteria),
			censusIntent("pruned", v1alpha1.IntentRevising, pruned),
			censusIntent("mixed", v1alpha1.IntentBlocked, mixed),
			censusIntent("merged", v1alpha1.IntentMerged, criteria),
			censusIntent("closed", v1alpha1.IntentClosed, pruned),
			censusIntent("new", v1alpha1.IntentInReview, outcomes),
			other, otherPV,
			censusPV("crit-v1", true),
			censusPV("new-v1", false),
		}, wantIntents: []string{"crit", "mixed", "pruned"}, wantPVs: []string{"crit-v1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := censusClient(t, tc.objs...)
			got, err := TakeSlice1Census(context.Background(), r, testNS)
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			if !slices.Equal(got.Intents, tc.wantIntents) || !slices.Equal(got.Verifications, tc.wantPVs) {
				t.Errorf("census = %+v, want intents %v and verifications %v", got, tc.wantIntents, tc.wantPVs)
			}
			if got.Count() != len(tc.wantIntents)+len(tc.wantPVs) {
				t.Errorf("count = %d", got.Count())
			}
		})
	}
}

// TestSlice1CensusListFailure: a list the API server refuses is the
// census's error, naming what it could not list; nothing is counted.
func TestSlice1CensusListFailure(t *testing.T) {
	refused := errors.New("forbidden")
	for _, kind := range []string{"intents", "previewverifications"} {
		r := censusClient(t)
		r = interceptor.NewClient(r.(client.WithWatch), interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				_, intents := list.(*v1alpha1.IntentList)
				if intents == (kind == "intents") {
					return refused
				}
				return c.List(ctx, list, opts...)
			},
		})
		if got, err := TakeSlice1Census(context.Background(), r, testNS); !errors.Is(err, refused) ||
			got.Count() != 0 {
			t.Errorf("%s refused: census %+v, %v; want the refusal and nothing counted", kind, got, err)
		}
	}
}

// TestSlice1CensusVerificationsForbidden: without the evaluator the chart
// grants intent-controller nothing on PreviewVerifications, so their list
// is Forbidden: the Intents are still counted, and the verifications are
// unlisted (unknown), never reported as none.
func TestSlice1CensusVerificationsForbidden(t *testing.T) {
	r := censusClient(t, censusIntent("crit", v1alpha1.IntentInReview, &v1alpha1.IntentAcceptance{
		Digest: "sha256:" + strings.Repeat("a", 64), Criteria: []v1alpha1.AcceptanceCriterion{{ID: "x"}}}))
	r = interceptor.NewClient(r.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.PreviewVerificationList); ok {
				return kerrors.NewForbidden(schema.GroupResource{Group: "maul.run",
					Resource: "previewverifications"}, "", errors.New("no grant"))
			}
			return c.List(ctx, list, opts...)
		},
	})
	got, err := TakeSlice1Census(context.Background(), r, testNS)
	if err != nil || !got.VerificationsUnlisted || !slices.Equal(got.Intents, []string{"crit"}) ||
		got.Verifications != nil {
		t.Errorf("census = %+v, %v; want the intent counted and the verifications unlisted", got, err)
	}
}

// censusClient is a fake client holding objs whose every write fails the
// test: the census must never write.
func censusClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	write := func() error {
		t.Error("the census wrote")
		return errors.New("write refused")
	}

	return fake.NewClientBuilder().WithScheme(kube.Scheme()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return write()
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return write()
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return write()
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return write()
			},
			DeleteAllOf: func(context.Context, client.WithWatch, client.Object, ...client.DeleteAllOfOption) error {
				return write()
			},
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object,
				...client.SubResourceUpdateOption) error {
				return write()
			},
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch,
				...client.SubResourcePatchOption) error {
				return write()
			},
		}).Build()
}

// TestSlice1CensusGauge: Report publishes the census as
// maul.intent.slice1_records, one point per kind.
func TestSlice1CensusGauge(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	Slice1Census{Intents: []string{"a", "b"}, Verifications: []string{"a-v1"}}.Report()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "maul.intent.slice1_records" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("maul.intent.slice1_records is %T, want an int64 gauge", m.Data)
			}
			for _, dp := range g.DataPoints {
				kind, _ := dp.Attributes.Value(attribute.Key("kind"))
				got[kind.AsString()] = dp.Value
			}
		}
	}
	if want := map[string]int64{"intent": 2, "previewverification": 1}; !maps.Equal(got, want) {
		t.Errorf("gauge = %v, want %v", got, want)
	}
}
