// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
)

// realEnv is a reconciler stepped by hand against a real API server holding
// the shipped CRDs, so every status write is held to the CRD schema. envtest
// runs no Deployment controller or kubelet; the test plays them.
type realEnv struct {
	t   *testing.T
	c   client.Client
	r   *Reconciler
	now time.Time
}

func startRealEnv(t *testing.T, s Settings) *realEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"patchy", "patchy-preview-0", "patchy-preview-1"} {
		if err := c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	e := &realEnv{t: t, c: c, now: time.Now().UTC()}
	e.r = &Reconciler{Client: c, Settings: s, Now: func() time.Time { return e.now }}
	return e
}

// createDemo creates the demo Project, an Intent in review with its PR at
// sha, and the Preview the writer derives for it.
func (e *realEnv) createDemo(sha string) (*v1alpha1.Intent, *v1alpha1.Preview) {
	e.t.Helper()
	ctx := e.t.Context()
	prefix := e.r.Settings.ImagePrefix
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "patchy"},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: prefix + "demo", Port: 8080,
				ReadinessPath: "/health"},
		}}
	if err := e.c.Create(ctx, project); err != nil {
		e.t.Fatal(err)
	}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: "patchy"},
		Spec: v1alpha1.IntentSpec{Project: "demo",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: 1}}}
	if err := e.c.Create(ctx, in); err != nil {
		e.t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{{
		Repository: "https://github.com/acme/demo", Number: 1, State: "open", HeadSHA: sha,
	}}
	if err := e.c.Status().Update(ctx, in); err != nil {
		e.t.Fatal(err)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: "patchy"},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, Project: "demo", HostLabel: in.Name,
			Components: []v1alpha1.PreviewComponent{{Name: "demo", ImageRepository: prefix + "demo",
				Revision: sha, Port: 8080, ReadinessPath: "/health"}},
			TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := e.c.Create(ctx, p); err != nil {
		e.t.Fatal(err)
	}
	return in, p
}

func (e *realEnv) step(p *v1alpha1.Preview) *v1alpha1.Preview {
	e.t.Helper()
	key := client.ObjectKeyFromObject(p)
	if _, err := e.r.Reconcile(e.t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	out := &v1alpha1.Preview{}
	if err := e.c.Get(e.t.Context(), key, out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// until steps the Preview until done holds, at most ten times.
func (e *realEnv) until(p *v1alpha1.Preview, what string, done func(*v1alpha1.Preview) bool) *v1alpha1.Preview {
	e.t.Helper()
	for range 10 {
		if p = e.step(p); done(p) {
			return p
		}
	}
	e.t.Fatalf("%s never held: status %+v", what, p.Status)
	return nil
}

// runPod plays the Deployment controller and the kubelet for the Preview's
// only component: its Deployment has rolled out, and a Ready Pod named name
// runs its current template.
func (e *realEnv) runPod(p *v1alpha1.Preview, name string) *corev1.Pod {
	e.t.Helper()
	ctx := e.t.Context()
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: componentName(p, 0)}
	if err := e.c.Get(ctx, key, &dep); err != nil {
		e.t.Fatal(err)
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
	if err := e.c.Status().Update(ctx, &dep); err != nil {
		e.t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace,
		Labels: maps.Clone(dep.Spec.Template.Labels)}, Spec: *dep.Spec.Template.Spec.DeepCopy()}
	if err := e.c.Create(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Ready: true,
		Image: pod.Spec.Containers[0].Image, ImageID: "repo@sha256:" + name}}
	if err := e.c.Status().Update(ctx, pod); err != nil {
		e.t.Fatal(err)
	}
	return pod
}

// TestEnvtestServingSnapshotLifecycle drives the snapshot through a real API
// server: written at Ready (the CRD admits it), kept through a redeploy to a
// new PR head, moved to the new generation once that is Ready, and cleared
// by a retry that deletes the Deployments.
func TestEnvtestServingSnapshotLifecycle(t *testing.T) {
	e := startRealEnv(t, testSettings())
	ctx := t.Context()
	in, p := e.createDemo(strings.Repeat("a", 40))
	p = e.until(p, "Deployment rendered", func(p *v1alpha1.Preview) bool {
		return e.c.Get(ctx, types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"},
			&appsv1.Deployment{}) == nil
	})
	old := e.runPod(p, "old")
	p = e.until(p, "Ready", func(p *v1alpha1.Preview) bool { return p.Status.Phase == v1alpha1.PreviewReady })
	first := &v1alpha1.PreviewServing{Generation: p.Generation, UID: p.UID,
		Components: []v1alpha1.PreviewServingComponent{{Name: "demo", Service: "preview-demo-1", Path: "/", Port: 80}}}
	if !reflect.DeepEqual(p.Status.Serving, first) {
		t.Fatalf("serving at Ready = %+v, want %+v", p.Status.Serving, first)
	}

	// A new PR head: Deploying, with the previous snapshot kept.
	head := strings.Repeat("b", 40)
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.PullRequests[0].HeadSHA = head
	if err := e.c.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].Revision = head
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p)
	if p.Status.Phase != v1alpha1.PreviewDeploying || !reflect.DeepEqual(p.Status.Serving, first) {
		t.Fatalf("redeploying: phase %s, serving %+v; want Deploying with %+v", p.Status.Phase, p.Status.Serving,
			first)
	}
	p = e.step(p) // the Deployment rolls to the new head
	if !reflect.DeepEqual(p.Status.Serving, first) {
		t.Fatalf("serving during the rollout = %+v, want %+v", p.Status.Serving, first)
	}
	if err := e.c.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	e.runPod(p, "new")
	p = e.until(p, "Ready at the new head", func(p *v1alpha1.Preview) bool {
		return p.Status.Phase == v1alpha1.PreviewReady
	})
	if p.Status.Serving == nil || p.Status.Serving.Generation != p.Generation || p.Generation <= first.Generation {
		t.Fatalf("serving at the new head = %+v, want generation %d", p.Status.Serving, p.Generation)
	}

	// Another head that never becomes Ready: the rollout times out and the
	// retry deletes the Deployments, and with them the snapshot.
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	head = strings.Repeat("c", 40)
	in.Status.PullRequests[0].HeadSHA = head
	if err := e.c.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].Revision = head
	if err := e.c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p = e.step(p)
	if p.Status.Serving == nil {
		t.Fatal("snapshot cleared by a redeploy")
	}
	e.now = e.now.Add(e.r.Settings.RolloutTimeout + time.Minute)
	p = e.until(p, "a retry", func(p *v1alpha1.Preview) bool { return p.Status.Retries == 1 })
	if p.Status.Serving != nil {
		t.Fatalf("serving after a retry = %+v, want none", p.Status.Serving)
	}
	if err := e.c.Get(ctx, types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"},
		&appsv1.Deployment{}); err == nil {
		// The retry's next pass recreates it; the snapshot stays cleared
		// until the attempt is Ready.
		if p = e.step(p); p.Status.Serving != nil {
			t.Fatalf("snapshot back before the retried attempt was Ready: %+v", p.Status.Serving)
		}
	}
}

// TestEnvtestGatewayMode drives a Preview with the legacy Ingress off
// through a real API server: the Ingress an earlier legacy pass rendered is
// deleted, none is created, and the Preview is Ready with its snapshot once
// its Pod is Ready, without any readiness gate.
func TestEnvtestGatewayMode(t *testing.T) {
	s := testSettings()
	s.NoLegacyIngress = true
	s.TargetHealth = true // ignored without the legacy Ingress
	e := startRealEnv(t, s)
	ctx := t.Context()
	_, p := e.createDemo(strings.Repeat("a", 40))
	p = e.until(p, "a slot", func(p *v1alpha1.Preview) bool { return p.Status.Slot != nil })
	// What a legacy controller rendered for this Preview before the switch.
	legacy := testSettings().ingress(p, 0)
	if err := e.c.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	p = e.until(p, "Deployment rendered", func(p *v1alpha1.Preview) bool {
		return e.c.Get(ctx, types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"},
			&appsv1.Deployment{}) == nil
	})
	var ings networkingv1.IngressList
	if err := e.c.List(ctx, &ings, client.InNamespace("patchy-preview-0")); err != nil {
		t.Fatal(err)
	}
	for _, ing := range ings.Items {
		if ing.DeletionTimestamp.IsZero() {
			t.Fatalf("Ingress %s present without the legacy path", ing.Name)
		}
	}
	e.runPod(p, "only")
	p = e.until(p, "Ready", func(p *v1alpha1.Preview) bool { return p.Status.Phase == v1alpha1.PreviewReady })
	want := &v1alpha1.PreviewServing{Generation: p.Generation, UID: p.UID,
		Components: []v1alpha1.PreviewServingComponent{{Name: "demo", Service: "preview-demo-1", Path: "/", Port: 80}}}
	if !reflect.DeepEqual(p.Status.Serving, want) || p.Status.URL == "" {
		t.Fatalf("status = %+v, want Ready with URL and %+v", p.Status, want)
	}
	if err := e.c.List(ctx, &ings, client.InNamespace("patchy-preview-0")); err != nil {
		t.Fatal(err)
	}
	if len(ings.Items) != 0 {
		t.Fatalf("Ingresses at Ready: %d", len(ings.Items))
	}
}
