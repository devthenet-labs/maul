// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
)

// The verification e2e's shared stack (formerly slice 1's suite,
// intent_verify_test.go, retired with the criteria it ran in S4): the
// environment the agent evaluator's suites (intent_evaluate*_test.go,
// intent_agents*_test.go) build on, and its helpers: the review flow, the
// preview served at a head, and the waits on the verification, the
// maul/acceptance status, the PreviewVerifications and the fix runs.

const (
	// testerImage is the digest-pinned preview-tester image
	// preview-controller renders (never run: the kubelet runs the binary).
	testerImage = "registry.example/maul/preview-tester@sha256:" +
		"5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	// acceptanceStatus is the commit status context maul posts.
	acceptanceStatus = "maul/acceptance"
	// shopLeaf is the application's preview image leaf.
	shopLeaf = "acme-shop"
)

// verifyEnv is one running verification stack.
type verifyEnv struct {
	*intentEnv
}

// shopComponent is the intent's one preview component at head.
func shopComponent(head string) v1alpha1.PreviewComponent {
	return v1alpha1.PreviewComponent{Name: appRepo, ImageRepository: previewPrefix + shopLeaf, Revision: head,
		Port: 8080, ReadinessPath: "/healthz"}
}

// toReview files an intent, approves its plan and waits for its pull
// request, returning the issue number, the Intent's name and the pull
// request's record.
func (e *verifyEnv) toReview(t *testing.T) (int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if len(in.Status.PullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want one", in.Status.PullRequests)
	}
	return number, name, in.Status.PullRequests[0]
}

// serve plays the Deployment controller and the kubelet for the intent's
// one-component preview at head, which envtest has neither of: its
// Deployment in slot 0 renders head and has rolled out, and one Pod of
// exactly its current template (image and reset annotation) is Ready with
// an image ID. Any other Pod of it is deleted, as a rollout replaces it.
func (e *verifyEnv) serve(t *testing.T, name, head string) {
	t.Helper()
	ctx := context.Background()
	c := shopComponent(head)
	e.waitPreview(t, name, "the preview at "+head, []v1alpha1.PreviewComponent{c})
	image := c.ImageRepository + ":sha-" + c.Revision
	key := types.NamespacedName{Namespace: "maul-preview-0", Name: "preview-" + name}
	var dep appsv1.Deployment
	eventually(t, "the "+c.Name+" Deployment to roll out", func() bool {
		if e.cl.client.Get(ctx, key, &dep) != nil || dep.Spec.Template.Spec.Containers[0].Image != image {
			return false
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1, 1
		return e.cl.client.Status().Update(ctx, &dep) == nil
	})
	tmpl := dep.Spec.Template
	var pods corev1.PodList
	if err := e.cl.client.List(ctx, &pods, client.InNamespace(key.Namespace),
		client.MatchingLabels(tmpl.Labels)); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.Containers[0].Image == image && maps.Equal(p.Annotations, tmpl.Annotations) {
			return // already served
		}
		if err := e.cl.client.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      fmt.Sprintf("%s-%s-g%d", key.Name, c.Revision[len(c.Revision)-8:], len(pods.Items)),
		Namespace: key.Namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations,
	}, Spec: tmpl.Spec}
	// Bound to the fake kubelet's Node, so its log (setContainerLog) is what
	// pods/log serves preview-controller's capture.
	pod.Spec.NodeName = kubeletNode
	if err := e.cl.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: c.Name, Ready: true,
		ImageID: c.ImageRepository + "@sha256:" + strings.Repeat(c.Revision[:1], 64)}}
	if err := e.cl.client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// waitVerification waits for the Intent's verification to satisfy cond,
// and returns the Intent.
func (e *verifyEnv) waitVerification(t *testing.T, name, why string,
	cond func(*v1alpha1.IntentVerification) bool) *v1alpha1.Intent {
	t.Helper()
	return e.waitFor(t, name, why, func(in *v1alpha1.Intent) bool {
		return in.Status.Verification != nil && cond(in.Status.Verification)
	})
}

// waitStatus waits for the newest maul/acceptance on head to be state,
// posted by maul, with a description containing says, and returns it.
func (e *verifyEnv) waitStatus(t *testing.T, head, state, says string) fakegithub.CommitStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, ok := e.gh.LatestStatus(head, acceptanceStatus)
		if ok && st.State == state && st.Creator == fakegithub.Bot && strings.Contains(st.Description, says) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for maul/acceptance %s (%q) on %s; latest is %+v (posted %v)", state, says,
				head, st, ok)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// verifications lists the Intent's PreviewVerifications, by name.
func (e *verifyEnv) verifications(t *testing.T, name string) map[string]v1alpha1.PreviewVerification {
	t.Helper()
	var list v1alpha1.PreviewVerificationList
	if err := e.cl.client.List(context.Background(), &list, client.InNamespace(namespace),
		client.MatchingLabels{v1alpha1.LabelIntent: name}); err != nil {
		t.Fatalf("list verifications: %v", err)
	}
	out := map[string]v1alpha1.PreviewVerification{}
	for _, pv := range list.Items {
		out[pv.Name] = pv
	}
	return out
}

// verifyRuns are the Intent's acceptance-fix runs (trigger verify), by
// name.
func (e *verifyEnv) verifyRuns(t *testing.T, name string) map[string]v1alpha1.IntentRun {
	t.Helper()
	out := map[string]v1alpha1.IntentRun{}
	for n, run := range e.runs(t, name) {
		if run.Spec.Trigger == v1alpha1.IntentRunTriggerVerify {
			out[n] = run
		}
	}
	return out
}

// waitFixRun waits for the Intent's one acceptance-fix round to launch or
// complete and returns it, failing at once, with what the runs say, if it
// ended any other way.
func (e *verifyEnv) waitFixRun(t *testing.T, name string) v1alpha1.IntentRun {
	t.Helper()
	var fix v1alpha1.IntentRun
	eventually(t, "an acceptance-fix round to launch", func() bool {
		for _, run := range e.verifyRuns(t, name) {
			fix = run
		}
		switch fix.Status.Phase {
		case v1alpha1.RunRunning, v1alpha1.RunComplete:
			return true
		case v1alpha1.RunFailed:
			t.Fatalf("acceptance-fix round %s failed: %s", fix.Name, e.describeRuns(t, name))
		}
		return false
	})
	return fix
}

// runOfTrigger waits for one of the Intent's runs of trigger to satisfy
// cond, and returns it.
func (e *verifyEnv) runOfTrigger(t *testing.T, name string, trigger v1alpha1.IntentRunTrigger,
	why string, cond func(v1alpha1.IntentRun) bool) v1alpha1.IntentRun {
	t.Helper()
	var found v1alpha1.IntentRun
	eventually(t, why, func() bool {
		for _, run := range e.runs(t, name) {
			if run.Spec.Trigger == trigger && cond(run) {
				found = run
				return true
			}
		}
		return false
	})
	return found
}

// botSays reports whether any of maul's comments on number contains s.
func (e *verifyEnv) botSays(number int64, s string) bool {
	for _, c := range e.gh.IssueComments(int(number)) {
		if c.User == fakegithub.Bot && strings.Contains(c.Body, s) {
			return true
		}
	}
	return false
}
