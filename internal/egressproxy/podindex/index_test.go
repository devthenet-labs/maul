// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package podindex

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/egressproxy"
)

const ns = "maul-agents"

var epoch = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// agentPod is an agent pod as internal/jobs shapes it, with a spec and env
// the index must never keep.
func agentPod(name, ip string, phase corev1.PodPhase, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(epoch.Add(-age)),
			Labels: map[string]string{
				labelApp:                       appName,
				"app.kubernetes.io/managed-by": "maul",
				v1alpha1.LabelFinding:          "f-" + name,
				v1alpha1.LabelOwner:            "devthenet-labs",
				v1alpha1.LabelRunKind:          "remediation",
				v1alpha1.LabelAttempt:          "1",
				v1alpha1.LabelHarness:          "claude",
				"batch.kubernetes.io/job-name": "job-" + name,
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationRepo:                     "devthenet-labs/maul-target",
				"maul.run/runner-image":                     "registry.example/app@sha256:abc",
				"kubectl.kubernetes.io/last-applied-config": "{}",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "agent",
			Env:  []corev1.EnvVar{{Name: "SECRET_THING", Value: "do-not-cache"}},
		}}},
		Status: corev1.PodStatus{PodIP: ip, Phase: phase},
	}
}

// started runs an index over cs until the test ends and waits for its
// first list.
func started(t *testing.T, cs *fake.Clientset) *Index {
	t.Helper()
	x, err := New(cs, ns)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	x.Start(ctx, slog.New(slog.DiscardHandler))
	waitFor(t, x.Synced)
	return x
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLookupAttributesAnAgentPod(t *testing.T) {
	x := started(t, fake.NewClientset(agentPod("a", "10.1.0.5", corev1.PodRunning, time.Minute)))
	got, ok := x.Lookup("10.1.0.5")
	want := egressproxy.PodInfo{Name: "a", Finding: "f-a", Owner: "devthenet-labs", RunKind: "remediation",
		Attempt: "1", Harness: "claude", Repo: "devthenet-labs/maul-target"}
	if !ok || got != want {
		t.Errorf("Lookup = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := x.Lookup("10.1.0.6"); ok {
		t.Error("Lookup of an unknown IP succeeded")
	}
}

// TestOnlyPendingAndRunningAgentPods: finished pods and pods that are not
// agent pods are never attributed.
func TestOnlyPendingAndRunningAgentPods(t *testing.T) {
	other := agentPod("other", "10.1.0.9", corev1.PodRunning, time.Minute)
	other.Labels[labelApp] = "something-else"
	x := started(t, fake.NewClientset(
		agentPod("pending", "10.1.0.1", corev1.PodPending, time.Minute),
		agentPod("done", "10.1.0.2", corev1.PodSucceeded, time.Minute),
		agentPod("failed", "10.1.0.3", corev1.PodFailed, time.Minute),
		agentPod("no-ip", "", corev1.PodPending, time.Minute),
		other,
	))
	if got, ok := x.Lookup("10.1.0.1"); !ok || got.Name != "pending" {
		t.Errorf("pending pod = %+v, %v", got, ok)
	}
	for _, ip := range []string{"10.1.0.2", "10.1.0.3", "10.1.0.9", ""} {
		if got, ok := x.Lookup(ip); ok {
			t.Errorf("Lookup(%q) = %+v, want no attribution", ip, got)
		}
	}
}

// TestIPReuseNewestWins: two live pods on one IP attribute to the newer;
// once it is gone, the older is attributed again; once it finishes,
// nothing is.
func TestIPReuseNewestWins(t *testing.T) {
	cs := fake.NewClientset(
		agentPod("old", "10.1.0.5", corev1.PodRunning, time.Hour),
		agentPod("new", "10.1.0.5", corev1.PodRunning, time.Minute),
	)
	x := started(t, cs)
	if got, _ := x.Lookup("10.1.0.5"); got.Name != "new" {
		t.Errorf("Lookup with two pods on one IP = %q, want new", got.Name)
	}
	ctx := context.Background()
	if err := cs.CoreV1().Pods(ns).Delete(ctx, "new", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { got, _ := x.Lookup("10.1.0.5"); return got.Name == "old" })
	old := agentPod("old", "10.1.0.5", corev1.PodSucceeded, time.Hour)
	if _, err := cs.CoreV1().Pods(ns).UpdateStatus(ctx, old, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, ok := x.Lookup("10.1.0.5"); return !ok })
}

// TestTransformKeepsOnlyAttribution: the spec, env and every other label
// and annotation are dropped before the cache.
func TestTransformKeepsOnlyAttribution(t *testing.T) {
	x := started(t, fake.NewClientset(agentPod("a", "10.1.0.5", corev1.PodRunning, time.Minute)))
	objs := x.informer.GetStore().List()
	if len(objs) != 1 {
		t.Fatalf("cache holds %d objects, want 1", len(objs))
	}
	p := objs[0].(*corev1.Pod)
	if len(p.Spec.Containers) != 0 || p.Spec.ServiceAccountName != "" {
		t.Errorf("cached pod kept its spec: %+v", p.Spec)
	}
	if _, ok := p.Labels["batch.kubernetes.io/job-name"]; ok || len(p.Labels) != len(keptLabels) {
		t.Errorf("cached labels = %v", p.Labels)
	}
	if len(p.Annotations) != 1 || p.Annotations[v1alpha1.AnnotationRepo] == "" {
		t.Errorf("cached annotations = %v", p.Annotations)
	}
	if p.Name != "a" || p.Status.PodIP != "10.1.0.5" || p.Status.Phase != corev1.PodRunning ||
		p.CreationTimestamp.IsZero() {
		t.Errorf("cached pod lost what attribution reads: %+v", p)
	}
	// Anything that is not a pod passes through untouched.
	tomb := "not a pod"
	if got, err := Transform(tomb); err != nil || got != tomb {
		t.Errorf("Transform(%v) = %v, %v", tomb, got, err)
	}
}

// TestSelectorListsAgentPodsOnly: the informer asks the API server for agent
// pods only, in the agent namespace only.
func TestSelectorListsAgentPodsOnly(t *testing.T) {
	cs := fake.NewClientset()
	started(t, cs)
	var listed bool
	for _, a := range cs.Actions() {
		if a.GetVerb() != "list" || a.GetResource().Resource != "pods" {
			continue
		}
		listed = true
		if a.GetNamespace() != ns {
			t.Errorf("listed pods in %q, want %q", a.GetNamespace(), ns)
		}
		if got := a.(k8stesting.ListAction).GetListRestrictions().Labels.String(); got != Selector {
			t.Errorf("listed pods with selector %q, want %q", got, Selector)
		}
	}
	if !listed {
		t.Error("the index never listed pods")
	}
	if Selector != "app.kubernetes.io/name=maul-agent" {
		t.Errorf("Selector = %q", Selector)
	}
}
