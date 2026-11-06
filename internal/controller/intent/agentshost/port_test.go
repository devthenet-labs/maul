// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// TestKube: the adapter reads a Job and its newest pod, lists only tool
// Jobs, reads the key from the Secret, and deletes the Job and Secret,
// idempotently.
func TestKube(t *testing.T) {
	ctx := context.Background()
	job := toolJob(pvName, pvUID)
	job.UID = "job-uid"
	other := toolJob("evaluator", "x")
	other.Labels[v1alpha1.LabelRole] = v1alpha1.RoleEvaluator
	pod := func(name string, at time.Time) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentsNS,
			CreationTimestamp: metav1.NewTime(at), Labels: map[string]string{batchv1.ControllerUidLabel: "job-uid"}}}
	}
	key := strings.Repeat("ab", 32)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pvName + jobs.ToolsSecretSuffix, Namespace: agentsNS,
		Labels: map[string]string{v1alpha1.LabelPreviewVerification: string(pvUID)}},
		Data: map[string][]byte{jobs.ToolsKeyFile: []byte(key)}}
	cs := k8sfake.NewClientset(job, other, pod("old", t0), pod("new", t0.Add(time.Minute)), secret)
	k := Kube{CS: cs, Reader: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(secret.DeepCopy()).Build(),
		Namespace: agentsNS}
	gotJob, gotPod, err := k.Get(ctx, pvName)
	if err != nil || gotJob == nil || gotPod == nil || gotPod.Name != "new" {
		t.Fatalf("Get = %v, %v, %v; want the Job and its newest pod", gotJob, gotPod, err)
	}
	if j, p, err := k.Get(ctx, "none"); j != nil || p != nil || err != nil {
		t.Errorf("Get(none) = %v, %v, %v", j, p, err)
	}
	list, err := k.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != pvName {
		t.Errorf("List = %v, %v; want only the tool Job", list, err)
	}
	if got, uid, found, err := k.Key(ctx, pvName); err != nil || !found || got != key || uid != string(pvUID) {
		t.Errorf("Key = %q, %q, %v, %v", got, uid, found, err)
	}
	for range 2 {
		if err := k.Delete(ctx, pvName); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if j, _, _ := k.Get(ctx, pvName); j != nil {
		t.Error("the Job is still there")
	}
	if _, err := cs.CoreV1().Secrets(agentsNS).Get(ctx, secret.Name, metav1.GetOptions{}); err == nil {
		t.Error("the Secret is still there")
	}
}
