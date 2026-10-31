// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Bug 4: intent-controller deletes a running outcome verification whose
// evaluate run failed, and preview-controller never sees it again; its
// evaluator ingress policy has no owner (the request is in the release
// namespace), so the sweep is what removes it. One whose verification is
// gone is deleted once past orphanPolicyGrace, logged; a young one, a live
// verification's, and the Preview's own edge policies are kept.
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
	orphan.CreationTimestamp = metav1.NewTime(now.Add(-orphanPolicyGrace))
	young := pv.DeepCopy()
	young.UID = "pv-young"
	fresh := s.evaluatorPolicy(live, young, 0)
	fresh.CreationTimestamp = metav1.NewTime(now.Add(-orphanPolicyGrace + time.Second))
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

// A burst of failed attempts (three verifications created and deleted
// within a minute, each leaving its evaluator ingress policy) leaves no
// policy behind after the sweep: they cannot pile up into the slot's
// NetworkPolicy quota.
func TestSweepClearsABurstOfDeletedVerifications(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, live := verifyPreview(t, start)
	slot := int32(0)
	live.Status.Slot = &slot
	s := evalSettings()
	objs := make([]client.Object, 0, 4)
	objs = append(objs, live)
	policies := make([]*networkingv1.NetworkPolicy, 0, 3)
	for i := range 3 {
		pv := toolVerification(t, live)
		pv.UID = types.UID(fmt.Sprintf("pv-attempt-%d", i+1))
		np := s.evaluatorPolicy(live, pv, 0)
		np.CreationTimestamp = metav1.NewTime(start.Add(time.Duration(i) * 15 * time.Second))
		policies = append(policies, np)
		objs = append(objs, np)
	}
	e := newTestEnv(t, objs...)
	sweep := &Sweeper{Client: e.c, Settings: s, Now: func() time.Time { return start.Add(time.Minute) }}
	if err := sweep.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, np := range policies {
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(np), &networkingv1.NetworkPolicy{}); err == nil {
			t.Errorf("policy %s of a deleted verification survived the sweep", np.Name)
		}
	}
}
