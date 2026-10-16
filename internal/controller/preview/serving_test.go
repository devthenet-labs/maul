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
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// readyDemo brings the single-component demo-1 Preview to Ready in slot 0.
func readyDemo(t *testing.T, s Settings) (*testEnv, *v1alpha1.Intent, *v1alpha1.Preview) {
	t.Helper()
	in, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	e := newTestEnv(t, in, p)
	e.r.Settings = s
	p = e.untilSlot(p.Name)
	e.step(p.Name) // Service and Deployment
	e.markReady(p, 0)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready (%s)", p.Status.Phase, p.Status.Message)
	}
	return e, in, p
}

// wantServing is the snapshot a Ready demo-1 serves at generation gen.
func wantServing(p *v1alpha1.Preview, gen int64) *v1alpha1.PreviewServing {
	return &v1alpha1.PreviewServing{Generation: gen, UID: p.UID, Components: []v1alpha1.PreviewServingComponent{{
		Name: "demo", Service: "preview-demo-1", Path: "/", Port: 80,
	}}}
}

// TestServingSnapshotKeptThroughRedeploy: the snapshot is written when the
// Preview turns Ready, stays as it was while a new PR head deploys (the
// rolling update keeps the previous revision serving), and moves to the new
// generation only once that is Ready.
func TestServingSnapshotKeptThroughRedeploy(t *testing.T) {
	e, in, p := readyDemo(t, testSettings())
	if want := wantServing(p, 1); !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("serving at Ready = %+v, want %+v", p.Status.Serving, want)
	}
	head := strings.Repeat("b", 40)
	p = e.newHead(in, p, head)
	if p.Status.Phase != v1alpha1.PreviewDeploying || p.Status.URL != "" {
		t.Fatalf("status = %+v, want Deploying with no URL", p.Status)
	}
	if want := wantServing(p, 1); !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("serving while redeploying = %+v, want the previous %+v", p.Status.Serving, want)
	}
	p = e.step(p.Name) // the Deployment rolls to the new head
	if want := wantServing(p, 1); !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("serving during the rollout = %+v, want the previous %+v", p.Status.Serving, want)
	}
	// The rolling update replaces the old Pod with the new one.
	var old corev1.Pod
	if err := e.slotObject("preview-demo-1-pod", &old); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), &old); err != nil {
		t.Fatal(err)
	}
	e.markReady(p, 0)
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s, want Ready at the new head", p.Status.Phase)
	}
	if want := wantServing(p, 2); !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("serving at the new head = %+v, want %+v", p.Status.Serving, want)
	}
}

// TestServingSnapshotClearedWhenNothingServes: a retry that deletes the
// Deployments, Failed, Expired and a deleting Preview each clear the
// snapshot, so a router stops serving with them.
func TestServingSnapshotClearedWhenNothingServes(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		e, in, p := readyDemo(t, testSettings())
		p = e.newHead(in, p, strings.Repeat("b", 40))
		e.step(p.Name)
		e.now = e.now.Add(11 * time.Minute) // past the rollout timeout
		p = e.step(p.Name)
		if p.Status.Retries != 1 || p.Status.Serving != nil {
			t.Fatalf("after a retry: retries %d, serving %+v; want 1 and none", p.Status.Retries, p.Status.Serving)
		}
	})
	t.Run("failed", func(t *testing.T) {
		e, _, p := readyDemo(t, testSettings())
		p.Spec.Components[0].ImageRepository = testSettings().ImagePrefix + "other"
		p.Generation++
		if err := e.c.Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		p = e.step(p.Name)
		if p.Status.Phase != v1alpha1.PreviewFailed || p.Status.Serving != nil {
			t.Fatalf("forged spec: phase %s, serving %+v; want Failed and none", p.Status.Phase, p.Status.Serving)
		}
	})
	t.Run("expired", func(t *testing.T) {
		e, _, p := readyDemo(t, testSettings())
		e.now = e.now.Add(73 * time.Hour)
		p = e.step(p.Name)
		if p.Status.Phase != v1alpha1.PreviewExpired || p.Status.Serving != nil {
			t.Fatalf("expiry: phase %s, serving %+v; want Expired and none", p.Status.Phase, p.Status.Serving)
		}
	})
	t.Run("deleting", func(t *testing.T) {
		e, _, p := readyDemo(t, testSettings())
		if err := e.c.Delete(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		p = e.step(p.Name)
		if p.Status.Serving != nil {
			t.Fatalf("deleting Preview still serves %+v", p.Status.Serving)
		}
	})
	t.Run("released", func(t *testing.T) {
		e, _, p := readyDemo(t, testSettings())
		// A snapshot left by an older controller on a Failed Preview goes
		// with its slot.
		p.Status.Phase = v1alpha1.PreviewFailed
		p.Status.Message = "stale"
		p.Status.Serving = wantServing(p, 1)
		if err := e.c.Status().Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		e.collectGarbage()
		for range 6 {
			if p = e.step(p.Name); p.Status.Slot == nil {
				break
			}
			e.collectGarbage()
			var pods corev1.PodList
			if err := e.c.List(context.Background(), &pods, client.InNamespace("patchy-preview-0")); err != nil {
				t.Fatal(err)
			}
			for i := range pods.Items {
				if err := e.c.Delete(context.Background(), &pods.Items[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
		if p.Status.Slot != nil || p.Status.Serving != nil {
			t.Fatalf("released: slot %v, serving %+v; want neither", p.Status.Slot, p.Status.Serving)
		}
	})
}

// TestServingSnapshotMultiComponent: every component is in the snapshot,
// in spec order, under its Service and its path written out.
func TestServingSnapshotMultiComponent(t *testing.T) {
	in, p := multiPreview(t, time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	e := newTestEnv(t, in, p, shopProject())
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady {
		t.Fatalf("phase = %s (%s), want Ready", p.Status.Phase, p.Status.Message)
	}
	want := &v1alpha1.PreviewServing{Generation: 1, UID: p.UID, Components: []v1alpha1.PreviewServingComponent{
		{Name: "web", Service: "preview-shop-1", Path: "/", Port: 80},
		{Name: "api", Service: "preview-shop-1-api", Path: "/api", Port: 80},
	}}
	if !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("serving = %+v, want %+v", p.Status.Serving, want)
	}
}

// TestServingSnapshotBackfilled: a Preview made Ready by a controller that
// wrote no snapshot gets one on the next pass, without a redeploy.
func TestServingSnapshotBackfilled(t *testing.T) {
	e, _, p := readyDemo(t, testSettings())
	p.Status.Serving = nil
	if err := e.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p.Name)
	if p.Status.Phase != v1alpha1.PreviewReady || !reflect.DeepEqual(p.Status.Serving, wantServing(p, 1)) {
		t.Fatalf("status = %+v, want Ready with the snapshot backfilled", p.Status)
	}
}
