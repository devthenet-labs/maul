// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package kube

import (
	"context"
	"os"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// TestEnvtestLeaderReleasesLeaseOnStop drives a manager built from
// managerOptions against a real API server: once elected, stopping it
// hands its Lease back (no holder) before Start returns, and only after its
// leader runnable has returned, so a successor takes the Lease at once and
// never while the old leader still works.
func TestEnvtestLeaderReleasesLeaseOnStop(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping lease envtest")
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
	const ns, id = "default", "maul-lease-test"
	mgr, err := ctrl.NewManager(cfg, managerOptions(Options{LeaderElectionID: id, LeaderElectionNamespace: ns}))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	// holder is the Lease's holder, "" for none; "?" when it cannot be read.
	holder := func() string {
		var lease coordinationv1.Lease
		if err := admin.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: id}, &lease); err != nil {
			return "?" + err.Error()
		}
		if lease.Spec.HolderIdentity == nil {
			return ""
		}
		return *lease.Spec.HolderIdentity
	}
	// heldWhileStopping is the Lease's holder as the leader runnable saw it
	// on its way out: still held, so no successor could have started.
	heldWhileStopping := make(chan string, 1)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		heldWhileStopping <- holder()
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("never elected")
	}
	if h := holder(); h == "" || h[0] == '?' {
		t.Fatalf("elected, Lease holder %q", h)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the manager did not stop")
	}
	if h := <-heldWhileStopping; h == "" || h[0] == '?' {
		t.Errorf("Lease holder %q while the leader runnable was stopping: released before it returned", h)
	}
	if h := holder(); h != "" {
		t.Errorf("Lease still held by %q after the manager stopped: a successor waits out the lease", h)
	}
}
