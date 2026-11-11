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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
)

// censusSettings is a controller whose agent evaluator verifies the
// previewed Projects of censusProject.
var censusSettings = Settings{Namespace: testNS, Verification: true, Previews: true, Evaluator: true}

// censusProject is a previewed Project, verified when verified is set.
func censusProject(name string, verified bool) *v1alpha1.Project {
	p := testProject()
	p.Name, p.UID = name, types.UID("u-"+name)
	p.Spec.Preview = &appPreview
	if verified {
		p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	}
	return p
}

// censusIntent is an Intent of Project target in phase whose acceptance on
// record is acc.
func censusIntent(name string, phase v1alpha1.IntentPhase, acc *v1alpha1.IntentAcceptance) *v1alpha1.Intent {
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: v1alpha1.IntentSpec{Project: "target"}}
	in.Status.Phase, in.Status.Acceptance = phase, acc
	return in
}

// censusPV is an outcome PreviewVerification when outcomes is set, else one
// as the API server keeps slice 1's criteria request now that its criteria
// fields are removed: no outcomes.
func censusPV(name string, outcomes bool) *v1alpha1.PreviewVerification {
	pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
	if outcomes {
		pv.Spec.Outcomes = []v1alpha1.AcceptanceOutcome{{ID: "version-served"}}
	}
	return pv
}

// deleting is obj being deleted: a deletion timestamp and the finalizer
// that holds it (the fake client refuses the first without the second).
func deleting[T client.Object](obj T) T {
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	obj.SetFinalizers([]string{"example.com/hold"})
	return obj
}

// TestSlice1Census: the startup census (S4) counts every non-terminal
// Intent, of a Project the agent evaluator verifies, whose acceptance on
// record is criteria-era (a criteria digest with no outcomes digest: its
// criteria pruned, or never recorded), and every PreviewVerification
// evaluating no outcomes (slice 1's criteria request, pruned), in the
// controller's namespace only. Outcome records, empty records, records of
// another namespace, terminal Intents, Intents of a Project the evaluator
// does not verify or that is gone, and objects being deleted are not
// counted (#394's note: such a record is recorded by its digest alone, which
// nothing runs and the pruning does not change, and a delete is the fix the
// census names). It only reads: every write the client is asked fails the
// test.
func TestSlice1Census(t *testing.T) {
	sliceOne := &v1alpha1.IntentAcceptance{Digest: "sha256:" + strings.Repeat("a", 64)}
	outcomes := &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest,
		OutcomesDigest: "sha256:" + strings.Repeat("b", 64)}
	empty := &v1alpha1.IntentAcceptance{Digest: emptyCriteriaDigest}
	other := censusIntent("elsewhere", v1alpha1.IntentInReview, sliceOne)
	other.Namespace = "other"
	otherPV := censusPV("elsewhere-v1", false)
	otherPV.Namespace = "other"
	ofProject := func(in *v1alpha1.Intent, project string) *v1alpha1.Intent {
		in.Spec.Project = project
		return in
	}

	tests := []struct {
		name        string
		set         Settings
		objs        []client.Object
		wantIntents []string
		wantPVs     []string
	}{
		{name: "none", set: censusSettings},
		{name: "outcomes only", set: censusSettings, objs: []client.Object{
			censusProject("target", true),
			censusIntent("a", v1alpha1.IntentInReview, outcomes),
			censusIntent("b", v1alpha1.IntentRevising, empty),
			censusIntent("c", v1alpha1.IntentPending, nil),
			censusPV("a-v1", true),
		}},
		{name: "some", set: censusSettings, objs: []client.Object{
			censusProject("target", true), censusProject("plain", false),
			censusIntent("open", v1alpha1.IntentInReview, sliceOne),
			censusIntent("revising", v1alpha1.IntentRevising, sliceOne),
			censusIntent("merged", v1alpha1.IntentMerged, sliceOne),
			censusIntent("closed", v1alpha1.IntentClosed, sliceOne),
			censusIntent("new", v1alpha1.IntentInReview, outcomes),
			deleting(censusIntent("going", v1alpha1.IntentInReview, sliceOne)),
			ofProject(censusIntent("unverified", v1alpha1.IntentInReview, sliceOne), "plain"),
			ofProject(censusIntent("orphan", v1alpha1.IntentInReview, sliceOne), "gone"),
			other, otherPV,
			censusPV("open-v1", false),
			deleting(censusPV("going-v1", false)),
			censusPV("new-v1", true),
		}, wantIntents: []string{"open", "revising"}, wantPVs: []string{"open-v1"}},
		{name: "the evaluator off", set: Settings{Namespace: testNS}, objs: []client.Object{
			censusProject("target", true),
			censusIntent("open", v1alpha1.IntentInReview, sliceOne),
			censusPV("open-v1", false),
		}, wantPVs: []string{"open-v1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := censusClient(t, tc.objs...)
			got, err := TakeSlice1Census(context.Background(), r, tc.set)
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			if !slices.Equal(got.Intents, tc.wantIntents) || !slices.Equal(got.Verifications, tc.wantPVs) {
				t.Errorf("census = %+v, want intents %v and verifications %v", got, tc.wantIntents, tc.wantPVs)
			}
			if got.Count() != len(tc.wantIntents)+len(tc.wantPVs) {
				t.Errorf("count = %d", got.Count())
			}
			if (got.Err() == nil) != (got.Count() == 0) {
				t.Errorf("Err() = %v with %d records", got.Err(), got.Count())
			}
		})
	}
}

// TestSlice1CensusErr: the refusal names every record by namespace and
// name with the command that clears it, and is errSlice1Records.
func TestSlice1CensusErr(t *testing.T) {
	err := Slice1Census{Namespace: "maul", Intents: []string{"beatbattle-48"},
		Verifications: []string{"beatbattle-48-v1-a1"}}.Err()
	if !errors.Is(err, errSlice1Records) {
		t.Fatalf("Err() = %v, want errSlice1Records", err)
	}
	for _, want := range []string{"(2)", "Intent maul/beatbattle-48", "kubectl -n maul delete intent beatbattle-48",
		"/maul cancel", "PreviewVerification maul/beatbattle-48-v1-a1",
		"kubectl -n maul delete previewverification beatbattle-48-v1-a1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %q, want it to name %q", err, want)
		}
	}
	if err := (Slice1Census{Namespace: "maul", VerificationsUnlisted: true}).Err(); err != nil {
		t.Errorf("Err() of an empty census = %v", err)
	}
}

// TestSlice1CensusListFailure: a list the API server refuses is the
// census's error, naming what it could not list; nothing is counted.
func TestSlice1CensusListFailure(t *testing.T) {
	refused := errors.New("forbidden")
	for _, kind := range []string{"projects", "intents", "previewverifications"} {
		r := censusClient(t)
		r = interceptor.NewClient(r.(client.WithWatch), interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				var this string
				switch list.(type) {
				case *v1alpha1.ProjectList:
					this = "projects"
				case *v1alpha1.IntentList:
					this = "intents"
				case *v1alpha1.PreviewVerificationList:
					this = "previewverifications"
				}
				if this == kind {
					return refused
				}
				return c.List(ctx, list, opts...)
			},
		})
		got, err := TakeSlice1Census(context.Background(), r, censusSettings)
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), kind) || got.Count() != 0 {
			t.Errorf("%s refused: census %+v, %v; want the refusal naming it and nothing counted", kind, got, err)
		}
	}
}

// TestSlice1CensusVerificationsForbidden: without the evaluator the chart
// grants intent-controller nothing on PreviewVerifications, so their list
// is Forbidden: the Intents are still counted, and the verifications are
// unlisted (unknown), never reported as none.
func TestSlice1CensusVerificationsForbidden(t *testing.T) {
	r := censusClient(t, censusProject("target", true), censusIntent("crit", v1alpha1.IntentInReview,
		&v1alpha1.IntentAcceptance{Digest: "sha256:" + strings.Repeat("a", 64)}))
	r = interceptor.NewClient(r.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.PreviewVerificationList); ok {
				return kerrors.NewForbidden(schema.GroupResource{Group: "maul.run",
					Resource: "previewverifications"}, "", errors.New("no grant"))
			}
			return c.List(ctx, list, opts...)
		},
	})
	got, err := TakeSlice1Census(context.Background(), r, censusSettings)
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

// TestSlice1CensusPointsUnlisted: PreviewVerifications that could not be
// listed get no gauge point, never a zero that reads as none found.
func TestSlice1CensusPointsUnlisted(t *testing.T) {
	got := Slice1Census{Intents: []string{"a"}, VerificationsUnlisted: true}.points()
	if want := map[string]int64{"intent": 1}; !maps.Equal(got, want) {
		t.Errorf("points = %v, want %v", got, want)
	}
	got = Slice1Census{}.points()
	if want := map[string]int64{"intent": 0, "previewverification": 0}; !maps.Equal(got, want) {
		t.Errorf("points of an empty listed census = %v, want %v", got, want)
	}
}
