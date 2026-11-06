// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

// resolver answers every host with one address.
type resolver struct{}

func (resolver) LookupHost(context.Context, string) ([]string, error) {
	return []string{"10.100.12.7"}, nil
}

// TestRealBuilderAcceptsTheSpec: the tool pod the host asks for is one the
// jobs builder renders and creates (shape check included), its Secret
// holding the relay's workspace config and the key; the containers the
// host observes by name are the rendered ones.
func TestRealBuilderAcceptsTheSpec(t *testing.T) {
	w := newWorld(t)
	h := newHarness(t, w)
	cs := realJobs(h)
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Fatalf("status = %+v, want Running", pv.Status)
	}
	job, err := cs.BatchV1().Jobs(agentsNS).Get(context.Background(), pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the tool Job: %v", err)
	}
	if job.Labels[v1alpha1.LabelPreviewVerification] != string(pvUID) ||
		job.Labels[v1alpha1.LabelRole] != v1alpha1.RoleTools {
		t.Errorf("labels = %v", job.Labels)
	}
	pod := job.Spec.Template.Spec
	names := map[string]bool{}
	for _, c := range append(pod.InitContainers, pod.Containers...) {
		names[c.Name] = true
	}
	for _, n := range append([]string{toolsContainer, prepareContainer}, sandboxes...) {
		if !names[n] {
			t.Errorf("no container %q in the render (%v)", n, names)
		}
	}
	secret, err := cs.CoreV1().Secrets(agentsNS).Get(context.Background(), pvName+jobs.ToolsSecretSuffix,
		metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the tools Secret: %v", err)
	}
	c, err := previewtools.ParseConfig(secret.Data[previewtools.ConfigFile])
	if err != nil || !c.IsWorkspace() {
		t.Fatalf("relay config %v, %v", c, err)
	}
	key, err := ReadKey(context.Background(), readerOf{cs}, agentsNS, w.pv)
	if err != nil || key != string(secret.Data[jobs.ToolsKeyFile]) {
		t.Errorf("ReadKey = %q, %v; want the Secret's key", key, err)
	}
	// A second pass after a lost status write adopts the same Secret.
	pv := h.pv(t)
	pv.Status.Phase = v1alpha1.VerificationPending
	if err := h.c.Status().Update(context.Background(), pv); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationRunning {
		t.Fatalf("status = %+v after a retry, want Running", pv.Status)
	}
}

// readerOf reads Secrets from a clientset, as the manager's APIReader reads
// them from the API server.
type readerOf struct{ cs *k8sfake.Clientset }

func (r readerOf) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return errors.New("readerOf reads Secrets only")
	}
	got, err := r.cs.CoreV1().Secrets(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	*s = *got
	return nil
}

func (readerOf) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("readerOf does not list")
}
