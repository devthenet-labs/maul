// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// TestPreviewGatewayModeRuntime drives the shipped binary with the legacy
// slot Ingress off and generic node isolation: the slot Pods select the
// operator's node label and tolerate its taint, no Ingress is ever rendered,
// the Preview is Ready with its serving snapshot once its Pod is Ready, a
// new PR head keeps that snapshot while it deploys, and the Preview is
// cleaned up when its Intent ends. envtest has no Deployment controller or
// kubelet; the test writes their status at exactly those edges.
func TestPreviewGatewayModeRuntime(t *testing.T) {
	cl := startCluster(t)
	ctx := context.Background()
	for _, name := range []string{"patchy-preview-0", "patchy-preview-1"} {
		if err := cl.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	cl.controller(t, "preview-controller",
		"--preview-slot-count", "2",
		"--preview-image-prefix", "registry.example/patchy/previews/",
		"--preview-host-suffix", "preview.patchy.example.com",
		"--preview-node-selector", `{"node-role.example.com/preview":"true"}`,
		"--preview-taint-key", "node-role.example.com/preview",
		"--preview-poll-interval", "1s", "--preview-legacy-ingress=false", "--preview-target-health")
	sha := strings.Repeat("a", 40)
	project := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: namespace},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "demo", URL: "https://github.com/acme/demo"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: "registry.example/patchy/previews/demo",
				Port: 8080, ReadinessPath: "/health"},
		}}
	if err := cl.client.Create(ctx, project); err != nil {
		t.Fatal(err)
	}
	in := &v1alpha1.Intent{ObjectMeta: metav1.ObjectMeta{Name: "demo-1", Namespace: namespace},
		Spec: v1alpha1.IntentSpec{Project: "demo",
			Issue:       v1alpha1.IntentIssue{Repository: "https://github.com/acme/intents", Number: 1},
			RequestedBy: v1alpha1.IntentRequest{Login: "octocat", At: metav1.Now(), EventID: 1}}}
	if err := cl.client.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentInReview
	in.Status.PullRequests = []v1alpha1.IntentPullRequest{{
		Repository: "https://github.com/acme/demo", Number: 1, State: "open", HeadSHA: sha,
	}}
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Preview{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: namespace},
		Spec: v1alpha1.PreviewSpec{
			IntentRef: v1alpha1.ObjectReference{Name: in.Name, UID: in.UID}, Project: "demo", HostLabel: in.Name,
			Components: []v1alpha1.PreviewComponent{{Name: "demo",
				ImageRepository: "registry.example/patchy/previews/demo", Revision: sha,
				Port: 8080, ReadinessPath: "/health"}}, TTL: metav1.Duration{Duration: 72 * time.Hour},
		}}
	if err := cl.client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "patchy-preview-0", Name: "preview-demo-1"}
	var dep appsv1.Deployment
	eventually(t, "the Deployment", func() bool { return cl.client.Get(ctx, key, &dep) == nil })
	podSpec := dep.Spec.Template.Spec
	if !maps.Equal(podSpec.NodeSelector, map[string]string{"node-role.example.com/preview": "true"}) ||
		len(podSpec.Tolerations) != 1 || podSpec.Tolerations[0].Key != "node-role.example.com/preview" ||
		podSpec.Tolerations[0].Effect != corev1.TaintEffectNoExecute {
		t.Fatalf("slot Pod placement = %v, %v; want the generic selector and its taint", podSpec.NodeSelector,
			podSpec.Tolerations)
	}
	noIngress := func(when string) {
		t.Helper()
		var list networkingv1.IngressList
		if err := cl.client.List(ctx, &list, client.InNamespace(key.Namespace)); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("%s: %d slot Ingresses without the legacy path", when, len(list.Items))
		}
	}
	noIngress("deploying")
	// runPod plays the Deployment controller and the kubelet: a Ready Pod,
	// with no readiness gate, of the Deployment's current template.
	runPod := func(name, imageID string) *corev1.Pod {
		t.Helper()
		if err := cl.client.Get(ctx, key, &dep); err != nil {
			t.Fatal(err)
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
		if err := cl.client.Status().Update(ctx, &dep); err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace,
			Labels: dep.Spec.Template.Labels}, Spec: dep.Spec.Template.Spec}
		if err := cl.client.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "demo", Ready: true, ImageID: imageID}}
		if err := cl.client.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		return pod
	}
	old := runPod("preview-demo-1-old", "repo@sha256:123")
	var current v1alpha1.Preview
	eventually(t, "Ready with its serving snapshot", func() bool {
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) == nil &&
			current.Status.Phase == v1alpha1.PreviewReady && current.Status.Serving != nil
	})
	first := *current.Status.Serving
	if first.UID != current.UID || first.Generation != current.Generation || len(first.Components) != 1 ||
		first.Components[0] != (v1alpha1.PreviewServingComponent{Name: "demo", Service: "preview-demo-1",
			Path: "/", Port: 80}) {
		t.Fatalf("serving = %+v", first)
	}
	noIngress("Ready")

	// A new PR head deploys while the previous revision keeps serving.
	head := strings.Repeat("b", 40)
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.PullRequests[0].HeadSHA = head
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Components[0].Revision = head
	if err := cl.client.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Deployment at the new head", func() bool {
		return cl.client.Get(ctx, key, &dep) == nil &&
			strings.HasSuffix(dep.Spec.Template.Spec.Containers[0].Image, "sha-"+head)
	})
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.PreviewDeploying || current.Status.Serving == nil ||
		current.Status.Serving.Generation != first.Generation {
		t.Fatalf("redeploying: %+v, want Deploying with the previous snapshot", current.Status)
	}
	if err := cl.client.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	newPod := runPod("preview-demo-1-new", "repo@sha256:456")
	eventually(t, "Ready at the new head", func() bool {
		return cl.client.Get(ctx, client.ObjectKeyFromObject(p), &current) == nil &&
			current.Status.Phase == v1alpha1.PreviewReady && current.Status.Serving != nil &&
			current.Status.Serving.Generation > first.Generation
	})
	noIngress("Ready at the new head")

	// The Intent ends: the Preview and its slot objects go.
	if err := cl.client.Delete(ctx, newPod); err != nil {
		t.Fatal(err)
	}
	if err := cl.client.Get(ctx, client.ObjectKeyFromObject(in), in); err != nil {
		t.Fatal(err)
	}
	in.Status.Phase = v1alpha1.IntentMerged
	if err := cl.client.Status().Update(ctx, in); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Preview finalizer and slot cleanup", func() bool {
		return apierrors.IsNotFound(cl.client.Get(ctx, client.ObjectKeyFromObject(p), &v1alpha1.Preview{})) &&
			apierrors.IsNotFound(cl.client.Get(ctx, key, &appsv1.Deployment{})) &&
			apierrors.IsNotFound(cl.client.Get(ctx, key, &corev1.Service{}))
	})
}
