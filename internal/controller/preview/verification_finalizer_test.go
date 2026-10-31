// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// deleteAsIntentController deletes the verification as intent-controller
// drops one in flight: pinned by UID, so its finalizer holds it.
func (e *verifyEnv) deleteAsIntentController() *v1alpha1.PreviewVerification {
	e.t.Helper()
	pv := e.currentPV()
	uid := pv.UID
	if err := e.c.Delete(context.Background(), pv,
		&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		e.t.Fatal(err)
	}
	return e.currentPV()
}

// pvGone reports whether the verification is gone from the API server.
func (e *verifyEnv) pvGone() bool {
	var pv v1alpha1.PreviewVerification
	return kerrors.IsNotFound(e.c.Get(context.Background(), client.ObjectKeyFromObject(e.pv), &pv))
}

// Bug 4: a running outcome verification intent-controller deletes (its
// evaluate run failed) has everything it put in the slot torn down, its
// evaluator ingress policy above all (it has no owner, so nothing else would
// remove it and the slot's NetworkPolicy quota fills), and only then goes.
func TestDeletedRunningVerificationTearsDownItsSlot(t *testing.T) {
	e := newToolEnv(t)
	pv, _ := e.verify()
	if pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Fatalf("phase %s, want Running", pv.Status.Phase)
	}
	if !slices.Contains(pv.Finalizers, v1alpha1.FinalizerPreviewVerification) {
		t.Fatalf("finalizers %v: a verification that renders into a slot does not hold its cleanup", pv.Finalizers)
	}
	if len(e.jobs()) != 1 {
		t.Fatal("no tool Job")
	}
	for _, tc := range []struct {
		name string
		obj  client.Object
	}{
		{evaluatorPolicyName(e.p, pv), &networkingv1.NetworkPolicy{}},
		{toolName(e.p, pv, toolKeyInfix), &corev1.Secret{}},
		{toolName(e.p, pv, toolConfigInfix), &corev1.ConfigMap{}},
	} {
		if err := e.slotObject(tc.name, tc.obj); err != nil {
			t.Fatalf("%T %s before deletion: %v", tc.obj, tc.name, err)
		}
	}
	if held := e.deleteAsIntentController(); held.DeletionTimestamp.IsZero() {
		t.Fatal("the verification went before its slot was torn down")
	}
	e.verify2()
	if !e.pvGone() {
		t.Error("the torn-down verification still holds its finalizer")
	}
	if len(e.jobs()) != 0 {
		t.Error("the deleted verification's tool Job survived")
	}
	if !e.slotGone(evaluatorPolicyName(e.p, pv), &networkingv1.NetworkPolicy{}) {
		t.Error("the deleted verification's evaluator ingress policy leaked")
	}
	if !e.slotGone(toolName(e.p, pv, toolKeyInfix), &corev1.Secret{}) ||
		!e.slotGone(toolName(e.p, pv, toolConfigInfix), &corev1.ConfigMap{}) {
		t.Error("the deleted verification's key Secret or tool ConfigMap survived")
	}
}

// verify2 reconciles the verification, expecting no error, when it may
// already be gone afterwards.
func (e *verifyEnv) verify2() {
	e.t.Helper()
	if _, err := e.v.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.pv)}); err != nil {
		e.t.Fatalf("reconcile verification: %v", err)
	}
}

// A deletion never hangs: while the slot cannot be torn down the
// verification is held and the reconcile retried, but once
// verificationFinalizeGrace has passed since it was deleted its finalizer is
// released anyway, logged, and the sweep is left to remove the rest.
func TestVerificationDeletionIsBounded(t *testing.T) {
	failing := false
	e := newToolEnvWith(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch,
		obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*networkingv1.NetworkPolicy); ok && failing {
			return kerrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"},
				obj.GetName(), nil)
		}
		return c.Delete(ctx, obj, opts...)
	}})
	var logs bytes.Buffer
	e.v.Log = slog.New(slog.NewTextHandler(&logs, nil))
	e.verify()
	failing = true
	held := e.deleteAsIntentController()
	e.now = held.DeletionTimestamp.Add(time.Minute)
	if _, err := e.v.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(e.pv)}); err == nil {
		t.Error("a teardown that failed inside the grace was not retried")
	}
	if e.pvGone() {
		t.Fatal("the verification went while its slot could still be torn down")
	}
	e.now = held.DeletionTimestamp.Add(verificationFinalizeGrace + time.Second)
	e.verify2()
	if !e.pvGone() {
		t.Error("a teardown failing past the grace holds the verification's deletion forever")
	}
	if !strings.Contains(logs.String(), "finalizer released") {
		t.Errorf("the release past the grace is not logged: %s", logs.String())
	}
}

// A verification whose slot namespace is gone is not held: deleting what is
// already gone is no error.
func TestVerificationDeletionWithItsSlotGone(t *testing.T) {
	e := newToolEnvWith(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch,
		obj client.Object, opts ...client.DeleteOption) error {
		if obj.GetNamespace() == "maul-preview-0" {
			return kerrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, obj.GetNamespace())
		}
		return c.Delete(ctx, obj, opts...)
	}})
	e.verify()
	e.deleteAsIntentController()
	e.verify2()
	if !e.pvGone() {
		t.Error("a verification whose slot is gone is held")
	}
}

// A settled verification, torn down, releases its finalizer: retention's
// deletion of it later waits for nothing.
func TestSettledVerificationReleasesItsFinalizer(t *testing.T) {
	e := newVerifyEnv(t, interceptor.Funcs{})
	pv, _ := e.verify()
	if !slices.Contains(pv.Finalizers, v1alpha1.FinalizerPreviewVerification) {
		t.Fatalf("finalizers %v, want the cleanup finalizer while it runs", pv.Finalizers)
	}
	pv.Status.Phase = v1alpha1.VerificationStale
	pv.Status.Reason = v1alpha1.VerificationReasonStalePreview
	e.mustStatus(pv)
	pv, _ = e.verify()
	if slices.Contains(pv.Finalizers, v1alpha1.FinalizerPreviewVerification) {
		t.Errorf("finalizers %v on a settled, torn-down verification", pv.Finalizers)
	}
	if len(e.jobs()) != 0 {
		t.Error("the settled verification's Job is still there")
	}
}

// The sweep is the backstop for a policy the finalizer missed (one left by
// an older controller, or released past the grace): an evaluator ingress
// policy whose verification is gone is deleted once it is older than the
// grace, logged; a young one, and a live verification's, are kept.
func TestSweepEvaluatorPolicyOfGoneVerification(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, live := verifyPreview(t, now)
	slot := int32(0)
	live.Status.Slot = &slot
	s := evalSettings()
	pv := toolVerification(t, live)
	kept := s.evaluatorPolicy(live, pv, 0)
	gone := pv.DeepCopy()
	gone.UID = "pv-gone"
	orphan := s.evaluatorPolicy(live, gone, 0)
	orphan.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	young := pv.DeepCopy()
	young.UID = "pv-young"
	fresh := s.evaluatorPolicy(live, young, 0)
	fresh.CreationTimestamp = metav1.NewTime(now.Add(-time.Second))
	edge := s.networkPolicies(live, 0)[0]
	e := newTestEnv(t, live, pv, kept, orphan, fresh, edge)
	var logs bytes.Buffer
	sweep := &Sweeper{Client: e.c, Settings: s, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Now: func() time.Time { return now }}
	if err := sweep.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		obj  *networkingv1.NetworkPolicy
		want bool
	}{{kept, true}, {orphan, false}, {fresh, true}, {edge, true}} {
		err := e.c.Get(ctx, client.ObjectKeyFromObject(tc.obj), &networkingv1.NetworkPolicy{})
		if got := err == nil; got != tc.want {
			t.Errorf("%s present = %v, want %v (%v)", tc.obj.Name, got, tc.want, err)
		}
	}
	if !strings.Contains(logs.String(), orphan.Name) {
		t.Errorf("the swept policy is not logged: %s", logs.String())
	}
}
