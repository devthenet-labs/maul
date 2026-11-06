// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestLatestBuildRun (U24): the gate and the agents host pick the same
// build run, whatever order they list runs in, two created in the same
// second included: the latest by creation, then by name; only the
// Intent's own build runs of the repository count.
func TestLatestBuildRun(t *testing.T) {
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "x", UID: "intent-uid"}}
	at := metav1.NewTime(time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC))
	run := func(name string, created metav1.Time, mutate ...func(*v1alpha1.IntentRun)) *v1alpha1.IntentRun {
		r := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created,
			OwnerReferences: []metav1.OwnerReference{{UID: in.UID, Controller: new(true)}}}}
		r.Spec.Stage, r.Spec.IntentRef = v1alpha1.IntentStageBuild, v1alpha1.ObjectReference{Name: "x", UID: in.UID}
		r.Spec.Repository.URL = "https://github.com/acme/app"
		for _, m := range mutate {
			m(r)
		}
		return r
	}
	later := metav1.NewTime(at.Add(time.Minute))
	runs := []*v1alpha1.IntentRun{
		run("x-bld-r1-app-a1", at),
		run("x-bld-r2-app-a1", later),
		run("x-bld-r2-app-a2", later),
		run("x-rev-r3-a1", metav1.NewTime(at.Add(time.Hour)), func(r *v1alpha1.IntentRun) {
			r.Spec.Stage = v1alpha1.IntentStageRevise
		}),
		run("x-bld-r9-api-a1", metav1.NewTime(at.Add(time.Hour)), func(r *v1alpha1.IntentRun) {
			r.Spec.Repository.URL = "https://github.com/acme/api"
		}),
		run("x-bld-r9-app-a9", metav1.NewTime(at.Add(time.Hour)), func(r *v1alpha1.IntentRun) {
			r.Spec.IntentRef.UID = types.UID("another")
		}),
		run("x-bld-r9-app-a8", metav1.NewTime(at.Add(time.Hour)), func(r *v1alpha1.IntentRun) {
			r.OwnerReferences = nil
		}),
	}
	for i := range 2 {
		order := slices.Clone(runs)
		if i == 1 {
			slices.Reverse(order)
		}
		got := LatestBuildRun(in, "https://github.com/acme/app", order)
		if got == nil || got.Name != "x-bld-r2-app-a2" {
			t.Fatalf("order %d: latest = %+v, want x-bld-r2-app-a2", i, got)
		}
	}
	if got := LatestBuildRun(in, "https://github.com/acme/other", runs); got != nil {
		t.Errorf("a repository with no build: %s", got.Name)
	}
}
