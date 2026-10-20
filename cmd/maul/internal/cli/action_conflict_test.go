// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/cmd/maul/internal/kubecfg"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/kube"
)

// TestActionPatchIsDecidedOnFreshState proves the CLI's spec patch carries
// the resourceVersion its gating ran against. The finding goes terminal
// between the CLI's read and its write: the patch must conflict, the retry
// must re-run the gating on fresh state, and suspend must be refused rather
// than written onto a finished finding. A patch without the optimistic lock
// would go through.
func TestActionPatchIsDecidedOnFreshState(t *testing.T) {
	h := newHarness(t)
	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(testFinding("fnd-1", v1alpha1.PhaseQueued)).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption,
			) error {
				patches++
				if patches == 1 {
					var cur v1alpha1.Finding
					if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), &cur); err != nil {
						return err
					}
					cur.Status.Phase = v1alpha1.PhaseRemediated
					if err := cl.Status().Update(ctx, &cur); err != nil {
						return err
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	h.client = c
	h.opts.WithEnv(&kubecfg.Env{Client: c, Namespace: testNamespace})

	err := runAction(t.Context(), h.opts, &actionFlags{}, action.VerbSuspend, "finding", []string{"fnd-1"})
	if err == nil {
		t.Error("suspend succeeded on a finding that went terminal after the gating decision")
	}
	if patches != 1 {
		t.Errorf("patches = %d, want 1: the retry must refuse before writing again", patches)
	}
	if h.finding(t, "fnd-1").Spec.Suspend {
		t.Error("suspend written onto a terminal finding")
	}
}
