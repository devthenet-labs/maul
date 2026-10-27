// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
)

// verifyingFixture is a one-repository Project and an Intent in review whose
// Preview the writer derives, with verification on or off.
func verifyingFixture(verify bool) (*v1alpha1.Project, *v1alpha1.Intent) {
	const repo = "https://github.com/acme/preview-demo"
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "maul"},
		Spec: v1alpha1.ProjectSpec{
			Repositories: []v1alpha1.ProjectRepository{{Name: "demo", URL: repo}},
			Preview: &v1alpha1.ProjectPreview{
				ImageRepository: "123456789012.dkr.ecr.us-east-1.amazonaws.com/maul/previews/preview-demo",
				Port:            8080, ReadinessPath: "/health"},
		}}
	if verify {
		p.Spec.Verification = &v1alpha1.ProjectVerification{Enabled: true}
	}
	in := &v1alpha1.Intent{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: "maul", UID: types.UID("intent-uid")},
		Spec:       v1alpha1.IntentSpec{Project: "demo"},
		Status: v1alpha1.IntentStatus{Phase: v1alpha1.IntentInReview,
			PullRequests: []v1alpha1.IntentPullRequest{{Repository: repo, State: "open",
				HeadSHA: strings.Repeat("a", 40)}}},
	}
	return p, in
}

func reconcilePreviewSource(t *testing.T, r *PreviewSourceReconciler, in *v1alpha1.Intent) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: in.Namespace, Name: in.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

// TestPreviewSourceWritesTheEnvironmentAndResetGeneration: with verification
// on the Preview is created with the shared derivation's environment and the
// Intent's reset generation; with it off, neither.
func TestPreviewSourceWritesTheEnvironmentAndResetGeneration(t *testing.T) {
	for _, verify := range []bool{true, false} {
		p, in := verifyingFixture(verify)
		in.Status.Verification = &v1alpha1.IntentVerification{Reset: v1alpha1.ResetState{Generation: 3}}
		scheme := kube.Scheme()
		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(in).WithObjects(p, in).Build()
		r := &PreviewSourceReconciler{Client: c, APIReader: c, Scheme: scheme}
		reconcilePreviewSource(t, r, in)
		var got v1alpha1.Preview
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "maul", Name: in.Name}, &got); err != nil {
			t.Fatal(err)
		}
		want, ok := v1alpha1.DesiredPreviewEnvironment(p, in)
		if ok != verify || !reflect.DeepEqual(got.Spec.Environment, want) {
			t.Fatalf("verify=%v: environment = %+v, want %+v", verify, got.Spec.Environment, want)
		}
		wantGen := int64(0)
		if verify {
			wantGen = 3
		}
		if got.Spec.ResetGeneration != wantGen {
			t.Errorf("verify=%v: resetGeneration = %d, want %d", verify, got.Spec.ResetGeneration, wantGen)
		}
	}
}

// TestPreviewSourceBumpsResetGenerationInPlace: a reset on the Intent raises
// the same Preview's resetGeneration (same object), and the writer never
// lowers it.
func TestPreviewSourceBumpsResetGenerationInPlace(t *testing.T) {
	ctx := context.Background()
	p, in := verifyingFixture(true)
	scheme := kube.Scheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(in).WithObjects(p, in).Build()
	r := &PreviewSourceReconciler{Client: c, APIReader: c, Scheme: scheme}
	reconcilePreviewSource(t, r, in)
	key := types.NamespacedName{Namespace: "maul", Name: in.Name}
	var before v1alpha1.Preview
	if err := c.Get(ctx, key, &before); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ gen, want int64 }{{2, 2}, {1, 2}} {
		var cur v1alpha1.Intent
		if err := c.Get(ctx, key, &cur); err != nil {
			t.Fatal(err)
		}
		cur.Status.Verification = &v1alpha1.IntentVerification{Reset: v1alpha1.ResetState{Generation: step.gen}}
		if err := c.Status().Update(ctx, &cur); err != nil {
			t.Fatal(err)
		}
		reconcilePreviewSource(t, r, in)
		var got v1alpha1.Preview
		if err := c.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.ResetGeneration != step.want || got.CreationTimestamp != before.CreationTimestamp ||
			got.ResourceVersion == "" {
			t.Fatalf("after generation %d: resetGeneration %d, want %d", step.gen, got.Spec.ResetGeneration, step.want)
		}
	}
}

// TestPreviewSourceRecreatesOnEnvironmentChange: spec.environment is
// immutable, so a Project change that alters it deletes the Preview and
// creates a new one carrying the new environment.
func TestPreviewSourceRecreatesOnEnvironmentChange(t *testing.T) {
	ctx := context.Background()
	p, in := verifyingFixture(true)
	scheme := kube.Scheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(in).WithObjects(p, in).Build()
	r := &PreviewSourceReconciler{Client: c, APIReader: c, Scheme: scheme}
	reconcilePreviewSource(t, r, in)
	key := types.NamespacedName{Namespace: "maul", Name: in.Name}

	var cur v1alpha1.Project
	if err := c.Get(ctx, types.NamespacedName{Namespace: "maul", Name: "demo"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Preview.Port = 9090 // moves the tester's edge
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	reconcilePreviewSource(t, r, in) // deletes
	var gone v1alpha1.Preview
	if err := c.Get(ctx, key, &gone); err == nil {
		t.Fatalf("the Preview with the stale environment survived: %+v", gone.Spec.Environment)
	}
	reconcilePreviewSource(t, r, in) // creates
	var got v1alpha1.Preview
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if e := got.Spec.Environment; e == nil || e.Edges[0].Port != 9090 {
		t.Fatalf("environment = %+v, want the tester edge on 9090", e)
	}
}
