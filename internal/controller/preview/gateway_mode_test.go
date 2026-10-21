// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// gatewaySettings render no legacy slot Ingress.
func gatewaySettings() Settings {
	s := testSettings()
	s.NoLegacyIngress = true
	return s
}

// slotIngresses are every Ingress in slot 0.
func (e *testEnv) slotIngresses() []networkingv1.Ingress {
	e.t.Helper()
	var list networkingv1.IngressList
	if err := e.c.List(context.Background(), &list, client.InNamespace("maul-preview-0")); err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

// TestGatewayModeReadyWithoutIngress: without the legacy Ingress a Preview
// is Ready once its component has a Ready Pod of its current spec with an
// image ID, with its URL and serving snapshot, and no Ingress is ever
// rendered.
func TestGatewayModeReadyWithoutIngress(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	e := newTestEnv(t, in, p)
	e.r.Settings = gatewaySettings()
	p = e.untilSlot(p.Name)
	if p = e.step(p.Name); p.Status.Phase == v1alpha1.PreviewReady {
		t.Fatal("Ready before any Pod")
	}
	if ings := e.slotIngresses(); len(ings) != 0 {
		t.Fatalf("Ingress rendered without the legacy path: %v", ings)
	}
	e.markReady(p, 0)
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.URL != "https://demo-1."+e.r.Settings.HostSuffix ||
		!reflect.DeepEqual(p.Status.Serving, wantServing(p, 1)) || p.Status.Components[0].ImageID == "" {
		t.Fatalf("status = %+v, want Ready with URL, image ID and snapshot", p.Status)
	}
	if ings := e.slotIngresses(); len(ings) != 0 {
		t.Fatalf("Ingress rendered at Ready: %v", ings)
	}
}

// TestGatewayModeIgnoresTargetHealth: with no load balancer to wait for, a
// Ready Pod without a readiness gate makes the Preview Ready even with
// TargetHealth set, and the Ingress never precedes the Pods.
func TestGatewayModeIgnoresTargetHealth(t *testing.T) {
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	e := newTestEnv(t, in, p)
	e.r.Settings = gatewaySettings()
	e.r.Settings.TargetHealth = true
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	if ings := e.slotIngresses(); len(ings) != 0 {
		t.Fatalf("Ingress rendered before the Pods: %v", ings)
	}
	e.startPod(p, 0, "ungated", false, corev1.ConditionTrue)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s (%s), want Ready on an ungated Ready Pod", p.Status.Phase, p.Status.Message)
	}
}

// TestGatewayModeDeletesItsLegacyIngress: switching the legacy Ingress off
// deletes the Ingress a Ready Preview was served by, on its next pass and
// without restarting it, and leaves a foreign Ingress alone.
func TestGatewayModeDeletesItsLegacyIngress(t *testing.T) {
	e, _, p := readyDemo(t, testSettings())
	ctx := context.Background()
	var legacy networkingv1.Ingress
	if err := e.slotObject(resourceName(p), &legacy); err != nil {
		t.Fatalf("legacy Ingress: %v", err)
	}
	foreign := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "someone-else",
		Namespace: "maul-preview-0"}}
	if err := e.c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	e.r.Settings = gatewaySettings()
	p = e.step(p.Name)
	if err := e.slotObject(resourceName(p), &networkingv1.Ingress{}); !kerrors.IsNotFound(err) {
		t.Fatalf("legacy Ingress after the switch: %v", err)
	}
	if err := e.slotObject("someone-else", &networkingv1.Ingress{}); err != nil {
		t.Fatalf("foreign Ingress touched: %v", err)
	}
	if p.Status.Phase != v1alpha1.PreviewReady || p.Status.Retries != 0 ||
		!reflect.DeepEqual(p.Status.Serving, wantServing(p, 1)) {
		t.Fatalf("status = %+v, want still Ready and serving", p.Status)
	}
}

// TestGatewayModeRedeployKeepsServing: a new PR head keeps the snapshot
// while it deploys and renders no Ingress at any point.
func TestGatewayModeRedeployKeepsServing(t *testing.T) {
	e, in, p := readyDemo(t, gatewaySettings())
	p = e.newHead(in, p, strings.Repeat("b", 40))
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewDeploying || !reflect.DeepEqual(p.Status.Serving, wantServing(p, 1)) {
		t.Fatalf("redeploying: %+v, want Deploying with the previous snapshot", p.Status)
	}
	if ings := e.slotIngresses(); len(ings) != 0 {
		t.Fatalf("Ingress rendered during a redeploy: %v", ings)
	}
}

// TestGatewayModeReleaseDeletesIngress: a slot is freed only once the
// Preview's own Ingresses are gone with its workload.
func TestGatewayModeReleaseDeletesIngress(t *testing.T) {
	e, _, p := readyDemo(t, gatewaySettings())
	// A legacy Ingress written by a controller rolled back meanwhile.
	if err := e.c.Create(context.Background(), testSettings().ingress(p, 0)); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(73 * time.Hour)
	for range 6 {
		p = e.step(p.Name)
		e.collectGarbage()
		if p.Status.Slot == nil {
			break
		}
	}
	if p.Status.Phase != v1alpha1.PreviewExpired || p.Status.Slot != nil {
		t.Fatalf("status = %+v, want Expired with its slot released", p.Status)
	}
	if ings := e.slotIngresses(); len(ings) != 0 {
		t.Fatalf("Ingress left in a released slot: %v", ings)
	}
}

// TestGatewayModeSweepDeletesManagedIngresses: the sweep deletes every
// managed slot Ingress without the legacy path, a live Preview's too, and
// keeps a live Preview's on it.
func TestGatewayModeSweepDeletesManagedIngresses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     Settings
		keeps bool
	}{
		{"legacy", testSettings(), true},
		{"gateway", gatewaySettings(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, p := readyDemo(t, testSettings())
			sweep := &Sweeper{Client: e.c, Settings: tc.s}
			if err := sweep.SweepOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := e.slotObject(resourceName(p), &networkingv1.Ingress{})
			if kept := err == nil; kept != tc.keeps {
				t.Fatalf("live Preview's Ingress kept = %v (%v), want %v", kept, err, tc.keeps)
			}
			// The workload of a live Preview is never swept.
			if err := e.slotObject(componentName(p, 0), &corev1.Service{}); err != nil {
				t.Fatalf("live Preview's Service swept: %v", err)
			}
		})
	}
}
