// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/rbactest"
)

const (
	// egressPodDenied is the pod policy's refusal, distinct from the Job
	// policy's so a case cannot pass on the wrong policy.
	egressPodDenied = "only the Kubernetes Job controller may create a pod carrying one"
	// saJobController is the identity kube-controller-manager's Job
	// controller writes as under --use-service-account-credentials (kubeadm,
	// kind, k3s, EKS, EKS Auto Mode, GKE, AKS).
	saJobController = "system:serviceaccount:kube-system:job-controller"
	// saReplicaSet is another controller of the same manager, which must not
	// borrow the Job controller's admission.
	saReplicaSet = "system:serviceaccount:kube-system:replicaset-controller"
)

// TestEgressProfilePodsAgainstAPIServer: a Pod carrying a network profile
// label in the agent namespace is admitted only when the Job controller
// creates it for its controlling batch/v1 Job, so the Job-level policy stays
// the one gate on who may grant a profile. A bare Pod (or one from any other
// controller) with the label would otherwise get the profile's
// NetworkPolicy route to the egress proxy without any of the launch checks.
func TestEgressProfilePodsAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	applyEgressRender(t, admin)

	waitFor(t, func() error {
		err := admin.Create(ctx, profilePod("probe", nil), client.DryRunAll)
		if deniedWith(err, egressPodDenied) {
			return nil
		}
		return &waitError{err: err, want: egressPodDenied}
	})

	// The owning Job, admitted through the Job policy as its launcher.
	rbactest.WaitAllowed(t, admin,
		rbactest.Review(saRemediation, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Group: "batch", Resource: "jobs", Verb: "create",
		}),
		rbactest.Review(saJobController, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Resource: "pods", Verb: "create",
		}),
		rbactest.Review(saReplicaSet, nil, authorizationv1.ResourceAttributes{
			Namespace: egressAgents, Resource: "pods", Verb: "create",
		}),
	)
	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}
	job := profileJob("owner", "remediation", true)
	if err := as(saRemediation).Create(ctx, job); err != nil {
		t.Fatalf("create the owning Job as remediation-controller: %v", err)
	}
	jobRef := func(controller bool) []metav1.OwnerReference {
		return []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
			Controller: &controller, BlockOwnerDeletion: &controller,
		}}
	}
	rsController := true
	rsRef := []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "00000000-0000-0000-0000-000000000001",
		Controller: &rsController,
	}}

	for _, tc := range []struct {
		name  string
		user  string
		pod   *corev1.Pod
		admit bool
	}{
		{"the Job controller, for its Job", saJobController, profilePod("jc", jobRef(true)), true},
		{"a bare pod as cluster-admin", "", profilePod("bare-admin", nil), false},
		{"a bare pod as the Job controller", saJobController, profilePod("bare-jc", nil), false},
		{"a Job owner reference forged by cluster-admin", "", profilePod("forged", jobRef(true)), false},
		{"the Job controller with a non-controller Job reference", saJobController,
			profilePod("jc-noctl", jobRef(false)), false},
		{"the Job controller for a ReplicaSet", saJobController, profilePod("jc-rs", rsRef), false},
		{"the ReplicaSet controller", saReplicaSet, profilePod("rs", rsRef), false},
		{"the ReplicaSet controller claiming the Job", saReplicaSet, profilePod("rs-job", jobRef(true)), false},
		{"an unlabelled bare pod as cluster-admin", "", unlabelledPod(profilePod("plain", nil)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := admin
			if tc.user != "" {
				c = as(tc.user)
			}
			err := c.Create(ctx, tc.pod, client.DryRunAll)
			switch {
			case tc.admit && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case !tc.admit && !deniedWith(err, egressPodDenied):
				t.Errorf("got %v, want refused with %q", err, egressPodDenied)
			}
		})
	}

	checkProfilePodUpdates(t, admin, as(saJobController), jobRef(true))
}

// checkProfilePodUpdates: on UPDATE only a change to the profile labels is
// judged, no one may add one to a running pod, and removing one is free.
func checkProfilePodUpdates(t *testing.T, admin, jobController client.Client, owners []metav1.OwnerReference) {
	t.Helper()
	ctx := t.Context()
	live := profilePod("live", owners)
	if err := jobController.Create(ctx, live); err != nil {
		t.Fatalf("create as the Job controller: %v", err)
	}
	live.Annotations = map[string]string{"example.com/note": "unrelated"}
	if err := admin.Update(ctx, live); err != nil {
		t.Errorf("an update leaving the profile labels alone was refused: %v", err)
	}
	relabel := live.DeepCopy()
	relabel.Labels[netprofile.LabelKey("npm")] = netprofile.LabelValue
	if err := admin.Update(ctx, relabel); !deniedWith(err, egressPodDenied) {
		t.Errorf("adding a profile label to a live pod: got %v, want refused", err)
	}
	// The Job controller holds patch, not update, on pods.
	err := jobController.Patch(ctx, relabel.DeepCopy(), client.MergeFrom(live))
	if !deniedWith(err, egressPodDenied) {
		t.Errorf("adding a profile label to a live pod as the Job controller: got %v, want refused", err)
	}
	plain := unlabelledPod(profilePod("plain-live", nil))
	if err := admin.Create(ctx, plain); err != nil {
		t.Fatalf("create an unlabelled pod: %v", err)
	}
	plain.Labels[netprofile.LabelKey("go")] = netprofile.LabelValue
	if err := admin.Update(ctx, plain); !deniedWith(err, egressPodDenied) {
		t.Errorf("labelling an unlabelled pod: got %v, want refused", err)
	}
	cleared := live.DeepCopy()
	delete(cleared.Labels, netprofile.LabelKey("go"))
	if err := admin.Update(ctx, cleared); err != nil {
		t.Errorf("removing the profile label was refused: %v", err)
	}
}

// profilePod is an agent pod carrying profile go, as the Job controller
// stamps it from profileJob's template, with the given owner references.
func profilePod(name string, owners []metav1.OwnerReference) *corev1.Pod {
	noToken, no, yes := false, false, true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: egressAgents, OwnerReferences: owners,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "patchy-agent",
				"app.kubernetes.io/managed-by": "patchy",
				egressKind:                     "remediation",
				netprofile.LabelKey("go"):      netprofile.LabelValue,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "patchy-agent",
			AutomountServiceAccountToken: &noToken,
			RestartPolicy:                corev1.RestartPolicyNever,
			// Restricted, as the agent namespace's Pod Security level requires.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: "agent", Image: "example.com/agent:v1",
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// unlabelledPod drops the profile label.
func unlabelledPod(p *corev1.Pod) *corev1.Pod {
	delete(p.Labels, netprofile.LabelKey("go"))
	return p
}
