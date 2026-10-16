// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// TestAttributedToAnnotation: a value rides on the Job and its pod template
// alike; none leaves both exactly as before attribution existed; a value
// outside the grammar is refused before anything is created.
func TestAttributedToAnnotation(t *testing.T) {
	for _, value := range []string{"org", "github-id:42"} {
		spec := testSpec()
		spec.AttributedTo = value
		job := buildJobForTest(t, testConfig(), spec)
		if got := job.Annotations[v1alpha1.AnnotationAttributedTo]; got != value {
			t.Errorf("Job annotation = %q, want %q", got, value)
		}
		if got := job.Spec.Template.Annotations[v1alpha1.AnnotationAttributedTo]; got != value {
			t.Errorf("pod template annotation = %q, want %q", got, value)
		}
	}

	off := buildJobForTest(t, testConfig(), testSpec())
	if _, ok := off.Annotations[v1alpha1.AnnotationAttributedTo]; ok {
		t.Error("a Job with no attribution carries the annotation")
	}
	if _, ok := off.Spec.Template.Annotations[v1alpha1.AnnotationAttributedTo]; ok {
		t.Error("a pod template with no attribution carries the annotation")
	}
	stamped := testSpec()
	stamped.AttributedTo = "org"
	on := buildJobForTest(t, testConfig(), stamped)
	delete(on.Annotations, v1alpha1.AnnotationAttributedTo)
	delete(on.Spec.Template.Annotations, v1alpha1.AnnotationAttributedTo)
	if !reflect.DeepEqual(on, off) {
		t.Error("the annotation changed something else on the Job")
	}

	for _, bad := range []string{"github:alice", "github-id:0", "Org", "alice"} {
		cs := fake.NewClientset()
		spec := testSpec()
		spec.AttributedTo = bad
		_, _, err := New(cs, testConfig(), nil).Create(context.Background(), spec)
		if !errors.Is(err, attribution.ErrValue) {
			t.Errorf("Create(attributed %q) = %v, want ErrValue", bad, err)
		}
		if acts := cs.Actions(); len(acts) != 0 {
			t.Errorf("Create(attributed %q) made %d API calls, want none", bad, len(acts))
		}
	}
}

// TestEvalAttributedToAnnotation is the same for an evaluation Job.
func TestEvalAttributedToAnnotation(t *testing.T) {
	get := func(spec EvalSpec) (map[string]string, map[string]string) {
		t.Helper()
		cs := fake.NewClientset()
		name, err := New(cs, testConfig(), nil).CreateEval(context.Background(), spec)
		if err != nil {
			t.Fatalf("CreateEval: %v", err)
		}
		job, err := cs.BatchV1().Jobs("patchy-agents").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		return job.Annotations, job.Spec.Template.Annotations
	}
	job, pod := get(testEvalSpec())
	if job != nil {
		t.Errorf("an unattributed eval Job has annotations %v, want none", job)
	}
	if _, ok := pod[v1alpha1.AnnotationAttributedTo]; ok {
		t.Error("an unattributed eval pod carries the annotation")
	}
	spec := testEvalSpec()
	spec.AttributedTo = "org"
	job, pod = get(spec)
	if job[v1alpha1.AnnotationAttributedTo] != "org" || pod[v1alpha1.AnnotationAttributedTo] != "org" {
		t.Errorf("attributed eval Job %v, pod %v; want org on both", job, pod)
	}
	spec.AttributedTo = "github:alice"
	if _, err := New(fake.NewClientset(), testConfig(), nil).CreateEval(context.Background(), spec); !errors.Is(err,
		attribution.ErrValue) {
		t.Errorf("CreateEval(attributed github:alice) = %v, want ErrValue", err)
	}
}
