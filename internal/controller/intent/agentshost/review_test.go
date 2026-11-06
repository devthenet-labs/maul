// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// TestTeardownDeletesALoneSecret: a Secret left with no Job (the
// controller stopped between CreateToolPod's Secret and its Job) goes with
// its settled verification; another verification's is never touched.
func TestTeardownDeletesALoneSecret(t *testing.T) {
	for name, tc := range map[string]struct {
		uid    string
		delete bool
	}{"its own": {string(pvUID), true}, "another's": {"other-uid", false}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.head.UID = "recreated" // the request no longer holds: Stale
			h := newHarness(t, w)
			h.tools.keys[pvName], h.tools.keyUIDs[pvName] = strings.Repeat("ab", 32), tc.uid
			h.reconcile(t)
			if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationStale {
				t.Fatalf("phase = %s, want Stale", pv.Status.Phase)
			}
			if got := slices.Contains(h.tools.deleted, pvName); got != tc.delete {
				t.Errorf("deleted %v, want the Secret deleted: %v", h.tools.deleted, tc.delete)
			}
		})
	}
}

// failingForges is a Forge list that cannot be read.
type failingForges struct{}

func (failingForges) WebHosts(context.Context, string) ([]string, error) {
	return nil, errors.New("the Forge cache is not synced")
}

// TestForgeListFailureRetries: a Forge list that cannot be read is retried,
// never the end of the attempt, until past the verification's own timeout
// (Infra, never a verdict).
func TestForgeListFailureRetries(t *testing.T) {
	set, err := netprofile.Parse([]byte(`[{"name":"py","route":"tunnel","port":8092,"repositories":["octo/render"],` +
		`"modules":[],"bakedModCache":"","hosts":["pypi.org"],"clients":["pip"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://maul-egress-proxy.maul.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.pv.Spec.Workspace.NetworkProfile = "py"
	h := newHarness(t, w)
	h.r.Images.ProfilesEnabled, h.r.Images.Profiles, h.r.Images.ProxyBase = true, set, base
	h.r.Images.Forges = failingForges{}
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: ns, Name: pvName}}); err == nil {
		t.Error("no error to retry on")
	}
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationPending || len(h.tools.created) != 0 {
		t.Errorf("phase = %s, created %d; want it Pending, to retry", pv.Status.Phase, len(h.tools.created))
	}
	h.now = t0.Add(45*time.Minute + grace)
	h.reconcile(t)
	if pv := h.pv(t); pv.Status.Phase != v1alpha1.VerificationInfra ||
		pv.Status.Reason != v1alpha1.VerificationReasonToolsNotReady {
		t.Errorf("status = %+v, want Infra past the timeout", pv.Status)
	}
}
