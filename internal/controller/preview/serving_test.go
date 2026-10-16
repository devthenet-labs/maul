// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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

// dropComponent has the Project stop previewing repository repo and moves
// the Preview to the spec the writer then derives, as a new generation.
func (e *testEnv) dropComponent(project *v1alpha1.Project, in *v1alpha1.Intent, p *v1alpha1.Preview,
	repo string,
) *v1alpha1.Preview {
	e.t.Helper()
	ctx := context.Background()
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(project), project); err != nil {
		e.t.Fatal(err)
	}
	for i := range project.Spec.Repositories {
		if project.Spec.Repositories[i].Name == repo {
			project.Spec.Repositories[i].Preview = nil
		}
	}
	if err := e.c.Update(ctx, project); err != nil {
		e.t.Fatal(err)
	}
	components, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok {
		e.t.Fatalf("no components derived without %s", repo)
	}
	p.Spec.Components = components
	p.Generation++
	if err := e.c.Update(ctx, p); err != nil {
		e.t.Fatal(err)
	}
	return e.step(p.Name)
}

// TestServingSnapshotDropsPrunedServices: a redeploy that stops rendering a
// component keeps the snapshot only for the Services that survive the
// prune, so a router never sends a path to a deleted Service; the legacy
// path drops the same routes from its kept Ingress.
func TestServingSnapshotDropsPrunedServices(t *testing.T) {
	const docsSHA = "cccccccccccccccccccccccccccccccccccccccc"
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	project := shopProject()
	project.Spec.Repositories = append(project.Spec.Repositories, v1alpha1.ProjectRepository{
		Name: "docs", URL: "https://github.com/acme/docs",
		Preview: &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
			ImageRepository: testSettings().ImagePrefix + "acme-docs", Port: 8080, ReadinessPath: "/docs/healthz",
		}, Path: "/docs"},
	})
	in.Status.PullRequests = append(in.Status.PullRequests, v1alpha1.IntentPullRequest{
		Repository: "https://github.com/acme/docs", Number: 3, State: "open", HeadSHA: docsSHA,
	})
	components, ok := v1alpha1.DesiredPreviewComponents(project, in)
	if !ok || len(components) != 3 {
		t.Fatalf("derived %+v, %v; want three components", components, ok)
	}
	p.Spec.Components = components
	e := newTestEnv(t, project, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	for i := range p.Spec.Components {
		e.markReady(p, i)
	}
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.Serving == nil ||
		len(p.Status.Serving.Components) != 3 {
		t.Fatalf("phase %s, serving %+v; want Ready serving three components", p.Status.Phase, p.Status.Serving)
	}
	docs := componentName(p, slices.IndexFunc(p.Spec.Components,
		func(c v1alpha1.PreviewComponent) bool { return c.Name == "docs" }))
	want := p.Status.Serving.DeepCopy()
	want.Components = slices.DeleteFunc(want.Components,
		func(c v1alpha1.PreviewServingComponent) bool { return c.Service == docs })
	if len(want.Components) != 2 {
		t.Fatalf("snapshot %+v does not list %s", p.Status.Serving, docs)
	}

	p = e.dropComponent(project, in, p, "docs")
	if p.Status.Phase != v1alpha1.PreviewDeploying || !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("redeploying: phase %s, serving %+v; want Deploying with %+v", p.Status.Phase, p.Status.Serving,
			want)
	}
	p = e.step(p.Name) // prune deletes the docs component
	if err := e.slotObject(docs, &corev1.Service{}); !kerrors.IsNotFound(err) {
		t.Fatalf("dropped component's Service survived: %v", err)
	}
	// The remaining components' Deployments did not change, so the same pass
	// may find them Ready at the new generation.
	if p.Status.Phase == v1alpha1.PreviewReady {
		want.Generation = p.Generation
	}
	if !reflect.DeepEqual(p.Status.Serving, want) {
		t.Fatalf("serving after the prune = %+v, want %+v", p.Status.Serving, want)
	}
	for _, c := range p.Status.Serving.Components {
		if err := e.slotObject(c.Service, &corev1.Service{}); err != nil {
			t.Errorf("snapshot lists %s, whose Service is gone: %v", c.Service, err)
		}
	}
}

// TestServingSnapshotClearedWhenEveryServiceIsPruned: dropping the web
// component leaves the API first, so it takes the Preview's own Service name:
// the API's old Service is pruned and web's is repurposed to select the API's
// Pods. No entry still names its own Service, so the snapshot is cleared.
func TestServingSnapshotClearedWhenEveryServiceIsPruned(t *testing.T) {
	at := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	in, p := multiPreview(t, at)
	project := shopProject()
	e := newTestEnv(t, project, in, p)
	p = e.untilSlot(p.Name)
	e.step(p.Name)
	e.markReady(p, 0)
	e.markReady(p, 1)
	if p = e.step(p.Name); p.Status.Phase != v1alpha1.PreviewReady || p.Status.Serving == nil {
		t.Fatalf("phase %s, serving %+v; want Ready with a snapshot", p.Status.Phase, p.Status.Serving)
	}
	if p = e.dropComponent(project, in, p, "web"); p.Status.Serving != nil {
		t.Fatalf("serving after dropping web = %+v, want none", p.Status.Serving)
	}
	if p = e.step(p.Name); p.Status.Serving != nil {
		t.Fatalf("serving after the prune = %+v, want none", p.Status.Serving)
	}
	if err := e.slotObject("preview-shop-1-api", &corev1.Service{}); !kerrors.IsNotFound(err) {
		t.Errorf("the API's old Service survived the prune: %v", err)
	}
}
