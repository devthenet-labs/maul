// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

type namedCheck struct {
	name string
	ok   bool
}

// TestGoldenVerificationPreview pins every object a verification-capable
// two-component Preview renders into its slot (its tool pod's are pinned by
// TestGoldenToolPod).
func TestGoldenVerificationPreview(t *testing.T) {
	_, p := verifyPreview(t, time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC))
	p.Spec.ResetGeneration = 2
	s := verifySettings()
	if err := s.validatePreview(p); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, 0, 2*len(p.Spec.Components)+4)
	for i := range p.Spec.Components {
		objects = append(objects, s.deployment(p, i, 0), s.service(p, i, 0))
	}
	for _, np := range s.networkPolicies(p, 0) {
		objects = append(objects, np)
	}
	docs := make([]string, 0, len(objects))
	for _, obj := range objects {
		docs = append(docs, renderYAML(t, obj))
	}
	got := strings.Join(docs, "---\n")
	path := filepath.Join("testdata", "verification.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("verification render differs from %s:\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// legacyTesterJob is a tester Job of verification pv in Preview p's slot 0 as
// preview-controller rendered one for slice 1's criteria before S4 (its
// name, labels and run-once shape), which it no longer renders: one may be
// left in a slot by an earlier release, for the settle, the sweep and the
// slot cleanup to remove.
func legacyTesterJob(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification) *batchv1.Job {
	meta := labelsFor(p)
	meta[labelVerificationUID] = string(pv.UID)
	pod := labelsFor(p)
	pod[labelVerificationUID] = string(pv.UID)
	pod[labelPart] = v1alpha1.PreviewPartTester
	pod[labelRole] = string(v1alpha1.PreviewRoleTester)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: testerJobName(p, pv), Namespace: testSettings().slotName(0),
			Labels: meta},
		Spec: batchv1.JobSpec{BackoffLimit: ptr(int32(0)), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: pod},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr(false),
				Containers: []corev1.Container{{Name: "tester", Image: testTesterImage}}},
		}},
	}
}

// testerPod is the pod the Job controller would start for job.
func testerPod(job *batchv1.Job, name string) *corev1.Pod {
	l := map[string]string{"batch.kubernetes.io/job-name": job.Name}
	for k, v := range job.Spec.Template.Labels {
		l[k] = v
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace, Labels: l,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name,
			UID: job.UID, Controller: ptr(true)}}},
		Spec: job.Spec.Template.Spec}
}

// Cleanup deletes the Preview's Jobs (in the background, so no Pod is left
// behind by an orphaning delete), a tester Job an earlier release left
// included, and policies, and holds the slot until the tester's Pod is gone
// too.
func TestCleanupDeletesJobsAndPolicies(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	var propagation []metav1.DeletionPropagation
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object,
		opts ...client.DeleteOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			if o.PropagationPolicy == nil {
				propagation = append(propagation, "")
			} else {
				propagation = append(propagation, *o.PropagationPolicy)
			}
		}
		return c.Delete(ctx, obj, opts...)
	}}
	e := newTestEnvWith(t, funcs, verifyProject(), in, p)
	e.r.Settings = verifySettings()
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	ctx := context.Background()
	job := legacyTesterJob(p, testVerification(p))
	if err := e.c.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := testerPod(job, job.Name+"-x1")
	if err := e.c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.step(p.Name)
	if err := e.slotObject(job.Name, &batchv1.Job{}); !kerrors.IsNotFound(err) {
		t.Errorf("tester Job survived cleanup: %v", err)
	}
	if len(propagation) == 0 || propagation[0] != metav1.DeletePropagationBackground {
		t.Errorf("Job deleted with propagation %q, want Background", propagation)
	}
	var nps networkingv1.NetworkPolicyList
	if err := e.c.List(ctx, &nps, client.InNamespace("maul-preview-0")); err != nil || len(nps.Items) != 0 {
		t.Errorf("policies left: %d, %v", len(nps.Items), err)
	}
	// The tester's Pod (the garbage collector has yet to remove it) holds
	// the finalizer.
	var live v1alpha1.Preview
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), &live); err != nil {
		t.Fatalf("Preview released with its tester Pod left: %v", err)
	}
	if err := e.c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(ctx, reconcileRequest(p.Name)); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(p), &live); !kerrors.IsNotFound(err) {
		t.Errorf("Preview not finalized once its slot was empty: %v", err)
	}
}

// A slot is not handed to the next Preview while a leftover tester Job or
// policy of another remains.
func TestLeftoverJobOrPolicyHoldsSlot(t *testing.T) {
	_, p := verifyPreview(t, time.Now())
	s := verifySettings()
	job := legacyTesterJob(p, testVerification(p))
	for _, obj := range []client.Object{job, s.networkPolicies(p, 0)[0]} {
		e := newTestEnv(t, obj)
		e.r.Settings = s
		held, err := e.r.slotHasManagedObjects(context.Background(), 0)
		if err != nil || !held {
			t.Errorf("leftover %T: held = %v, %v", obj, held, err)
		}
	}
}

// The sweep removes what a vanished Preview or verification left: its
// policies and Jobs, a Job whose verification is gone, and a Job's Pod whose
// Job is gone. A live Preview's live verification keeps its Job, a Job's Pod
// with its Job stays, and a component's Pod is never the sweep's.
func TestSweepVerificationOrphans(t *testing.T) {
	ctx := context.Background()
	_, live := verifyPreview(t, time.Now())
	slot := int32(0)
	live.Status.Slot = &slot
	_, dead := testPreview("gone-1", time.Now())
	dead.Spec.Environment = &v1alpha1.PreviewEnvironment{Verification: true, Edges: []v1alpha1.PreviewEdge{
		{From: v1alpha1.PreviewPartTester, To: "demo", Port: 8080}}}
	s := verifySettings()
	livePV := testVerification(live)
	goneLivePV := testVerification(live)
	goneLivePV.UID = "pv-gone"
	mk := func(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, uid types.UID) *batchv1.Job {
		job := legacyTesterJob(p, pv)
		job.UID = uid
		return job
	}
	keptJob := mk(live, livePV, "job-kept")
	orphanedByPV := mk(live, goneLivePV, "job-pv-gone")
	orphanedByPreview := mk(dead, testVerification(dead), "job-preview-gone")
	keptPod := testerPod(keptJob, keptJob.Name+"-a")
	ghostJob := mk(live, livePV, "job-ghost")
	ghostPod := testerPod(ghostJob, ghostJob.Name+"-b")
	component := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "preview-shop-1-abc", Namespace: "maul-preview-0",
		Labels: s.deployment(live, 0, 0).Spec.Template.Labels}}
	deadPolicy := s.networkPolicies(dead, 0)[0]
	livePolicy := s.networkPolicies(live, 0)[0]
	e := newTestEnv(t, live, livePV, keptJob, orphanedByPV, orphanedByPreview, keptPod, ghostPod, component,
		deadPolicy, livePolicy)
	sweep := &Sweeper{Client: e.c, Settings: s}
	if err := sweep.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		obj  client.Object
		want bool
	}{
		{keptJob, true}, {orphanedByPV, false}, {orphanedByPreview, false}, {keptPod, true}, {ghostPod, false},
		{component, true}, {deadPolicy, false}, {livePolicy, true},
	} {
		err := e.c.Get(ctx, client.ObjectKeyFromObject(tc.obj), tc.obj)
		if got := err == nil; got != tc.want {
			t.Errorf("%T %s present = %v, want %v (%v)", tc.obj, tc.obj.GetName(), got, tc.want, err)
		}
	}
}

// The teardown hook runs before a Failed Preview's slot is cleaned, so the
// verification running in it can record what the slot showed.
func TestBeforeReleaseRunsBeforeCleanup(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := verifyPreview(t, at)
	e := newTestEnv(t, verifyProject(), in, p)
	e.r.Settings = verifySettings()
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	var calls []string
	e.r.BeforeRelease = func(ctx context.Context, got *v1alpha1.Preview) error {
		var nps networkingv1.NetworkPolicyList
		if err := e.c.List(ctx, &nps, client.InNamespace("maul-preview-0")); err != nil {
			return err
		}
		calls = append(calls, got.Name)
		if len(nps.Items) == 0 {
			t.Error("hook ran after cleanup")
		}
		return nil
	}
	p.Status.Phase = v1alpha1.PreviewFailed
	if err := e.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.step(p.Name)
	if len(calls) == 0 || calls[0] != p.Name {
		t.Errorf("hook calls = %v", calls)
	}
}
