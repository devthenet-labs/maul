// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// actionFinding is a minimal finding in the given phase.
func actionFinding(phase v1alpha1.Phase, mutate ...func(*v1alpha1.Finding)) *v1alpha1.Finding {
	f := &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: "fnd-1", Namespace: "maul"},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "gh"},
			Source:         "github-code-scanning",
			Advisories:     []string{"CVE-2026-0001"},
		},
		Status: v1alpha1.FindingStatus{Phase: phase},
	}
	for _, m := range mutate {
		m(f)
	}
	return f
}

// postAction drives one action through the full handler stack.
func postAction(t *testing.T, s *Server, name, verb string) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	res, err := http.Post(ts.URL+"/api/findings/"+name+"/actions/"+verb, "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res, strings.TrimSpace(string(body))
}

func getFinding(t *testing.T, c client.Client, name string) *v1alpha1.Finding {
	t.Helper()
	var f v1alpha1.Finding
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: name}, &f); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return &f
}

// actionCase is one action-handler scenario runActionCases drives.
type actionCase struct {
	name       string
	finding    *v1alpha1.Finding
	verb       string
	wantStatus int
	wantBody   string
	check      func(t *testing.T, f *v1alpha1.Finding)
}

// runActionCases drives each case through the full handler stack as a fully
// granted operator.
func runActionCases(t *testing.T, cases []actionCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, tc.finding)
			s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}
			res, body := postAction(t, s, tc.finding.Name, tc.verb)
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", res.StatusCode, body, tc.wantStatus)
			}
			if tc.wantBody != "" && body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if tc.wantStatus == http.StatusOK {
				var v map[string]any
				if err := json.Unmarshal([]byte(body), &v); err != nil {
					t.Errorf("success body %q is not JSON: %v", body, err)
				}
			}
			if tc.check != nil {
				tc.check(t, getFinding(t, mustClient(s), tc.finding.Name))
			}
		})
	}
}

func TestHandleAction(t *testing.T) {
	staleAt := metav1.NewTime(testClock.Add(-2 * time.Hour))
	doneAt := metav1.NewTime(testClock.Add(-time.Hour))
	freshAt := metav1.NewTime(testClock.Add(-30 * time.Minute))

	cases := []actionCase{
		{
			name:       "approve awaiting approval",
			finding:    actionFinding(v1alpha1.PhaseAwaitingApproval),
			verb:       "approve",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Approval == nil || f.Spec.Approval.By != "op@acme.test" {
					t.Errorf("approval = %+v", f.Spec.Approval)
				}
				// The phase edge belongs to remediation-controller.
				if f.Status.Phase != v1alpha1.PhaseAwaitingApproval {
					t.Errorf("phase moved to %q", f.Status.Phase)
				}
			},
		},
		{
			name: "approve replaces stale HandedOff approval",
			finding: actionFinding(v1alpha1.PhaseHandedOff, func(f *v1alpha1.Finding) {
				f.Spec.Approval = &v1alpha1.Approval{By: "old", At: staleAt}
				f.Status.CompletedAt = &doneAt
			}),
			verb:       "approve",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Approval.By != "op@acme.test" {
					t.Errorf("approval.by = %q, want replacement", f.Spec.Approval.By)
				}
			},
		},
		{
			name: "approve keeps fresh approval",
			finding: actionFinding(v1alpha1.PhaseHandedOff, func(f *v1alpha1.Finding) {
				f.Spec.Approval = &v1alpha1.Approval{By: "old", At: freshAt}
				f.Status.CompletedAt = &doneAt
			}),
			verb:       "approve",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Approval.By != "old" {
					t.Errorf("approval.by = %q, want first approval kept", f.Spec.Approval.By)
				}
			},
		},
		{
			name:       "approve outside approval phases",
			finding:    actionFinding(v1alpha1.PhaseInvestigating),
			verb:       "approve",
			wantStatus: http.StatusForbidden,
			wantBody:   "Action approve is not available for finding fnd-1 (phase Investigating).",
		},
		{
			name:       "suspend non-terminal",
			finding:    actionFinding(v1alpha1.PhaseQueued),
			verb:       "suspend",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if !f.Spec.Suspend {
					t.Error("suspend not set")
				}
			},
		},
		{
			name:       "suspend terminal",
			finding:    actionFinding(v1alpha1.PhaseRemediated),
			verb:       "suspend",
			wantStatus: http.StatusForbidden,
			wantBody:   "Action suspend is not available for finding fnd-1 (phase Remediated).",
		},
		{
			name: "suspend already suspended is a no-op",
			finding: actionFinding(v1alpha1.PhaseQueued, func(f *v1alpha1.Finding) {
				f.Spec.Suspend = true
			}),
			verb:       "suspend",
			wantStatus: http.StatusOK,
		},
		{
			name: "resume suspended",
			finding: actionFinding(v1alpha1.PhaseQueued, func(f *v1alpha1.Finding) {
				f.Spec.Suspend = true
			}),
			verb:       "resume",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Suspend {
					t.Error("suspend not cleared")
				}
			},
		},
		{
			name:       "resume not suspended is a no-op",
			finding:    actionFinding(v1alpha1.PhaseQueued),
			verb:       "resume",
			wantStatus: http.StatusOK,
		},
		{
			name:       "unknown verb",
			finding:    actionFinding(v1alpha1.PhaseQueued),
			verb:       "escalate",
			wantStatus: http.StatusNotFound,
		},
	}
	runActionCases(t, cases)
}

func TestHandleActionRetryExpedite(t *testing.T) {
	staleAt := metav1.NewTime(testClock.Add(-2 * time.Hour))
	doneAt := metav1.NewTime(testClock.Add(-time.Hour))
	freshAt := metav1.NewTime(testClock.Add(-30 * time.Minute))

	cases := []actionCase{
		{
			name: "retry failed finding",
			finding: actionFinding(v1alpha1.PhaseFailed, func(f *v1alpha1.Finding) {
				f.Status.CompletedAt = &doneAt
				f.Status.PhaseTimes = []v1alpha1.PhaseTime{
					{Phase: v1alpha1.PhaseInvestigating, At: doneAt},
					{Phase: v1alpha1.PhaseFailed, At: doneAt},
				}
			}),
			verb:       "retry",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Retry == nil || f.Spec.Retry.By != "op@acme.test" {
					t.Errorf("retry = %+v", f.Spec.Retry)
				}
				// The phase edge belongs to the consuming controller.
				if f.Status.Phase != v1alpha1.PhaseFailed {
					t.Errorf("phase moved to %q", f.Status.Phase)
				}
			},
		},
		{
			name: "retry keeps a pending request",
			finding: actionFinding(v1alpha1.PhaseFailed, func(f *v1alpha1.Finding) {
				f.Status.CompletedAt = &doneAt
				f.Status.PhaseTimes = []v1alpha1.PhaseTime{
					{Phase: v1alpha1.PhaseRemediating, At: doneAt},
					{Phase: v1alpha1.PhaseFailed, At: doneAt},
				}
				f.Spec.Retry = &v1alpha1.ActionRequest{By: "old", At: freshAt}
			}),
			verb:       "retry",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Retry.By != "old" {
					t.Errorf("retry.by = %q, want pending request kept", f.Spec.Retry.By)
				}
			},
		},
		{
			name: "retry replaces a consumed request after a new failure",
			finding: actionFinding(v1alpha1.PhaseFailed, func(f *v1alpha1.Finding) {
				f.Status.CompletedAt = &doneAt
				f.Status.PhaseTimes = []v1alpha1.PhaseTime{
					{Phase: v1alpha1.PhaseInvestigating, At: staleAt},
					{Phase: v1alpha1.PhaseFailed, At: staleAt},
					{Phase: v1alpha1.PhaseEnhanced, At: staleAt},
					{Phase: v1alpha1.PhaseInvestigating, At: staleAt},
					{Phase: v1alpha1.PhaseFailed, At: doneAt},
				}
				f.Spec.Retry = &v1alpha1.ActionRequest{By: "old", At: staleAt}
			}),
			verb:       "retry",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Retry.By != "op@acme.test" {
					t.Errorf("retry.by = %q, want replacement", f.Spec.Retry.By)
				}
			},
		},
		{
			name:       "retry outside failed",
			finding:    actionFinding(v1alpha1.PhaseQueued),
			verb:       "retry",
			wantStatus: http.StatusForbidden,
			wantBody:   "Action retry is not available for finding fnd-1 (phase Queued).",
		},
		{
			name: "retry failed without retryable history",
			finding: actionFinding(v1alpha1.PhaseFailed, func(f *v1alpha1.Finding) {
				f.Status.CompletedAt = &doneAt
			}),
			verb:       "retry",
			wantStatus: http.StatusForbidden,
			wantBody:   "Action retry is not available for finding fnd-1 (phase Failed).",
		},
		{
			name:       "expedite queued",
			finding:    actionFinding(v1alpha1.PhaseQueued),
			verb:       "expedite",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Expedite == nil || f.Spec.Expedite.By != "op@acme.test" {
					t.Errorf("expedite = %+v", f.Spec.Expedite)
				}
			},
		},
		{
			name: "expedite already expedited is a no-op",
			finding: actionFinding(v1alpha1.PhaseOpened, func(f *v1alpha1.Finding) {
				f.Spec.Expedite = &v1alpha1.ActionRequest{By: "old", At: staleAt}
			}),
			verb:       "expedite",
			wantStatus: http.StatusOK,
			check: func(t *testing.T, f *v1alpha1.Finding) {
				if f.Spec.Expedite.By != "old" {
					t.Errorf("expedite.by = %q, want first request kept", f.Spec.Expedite.By)
				}
			},
		},
		{
			name:       "expedite in-flight remediation",
			finding:    actionFinding(v1alpha1.PhaseRemediating),
			verb:       "expedite",
			wantStatus: http.StatusForbidden,
			wantBody:   "Action expedite is not available for finding fnd-1 (phase Remediating).",
		},
		{
			name:       "expedite terminal",
			finding:    actionFinding(v1alpha1.PhaseDismissed),
			verb:       "expedite",
			wantStatus: http.StatusForbidden,
			wantBody:   "Action expedite is not available for finding fnd-1 (phase Dismissed).",
		},
	}
	runActionCases(t, cases)
}

func TestHandleActionAuth(t *testing.T) {
	cases := []struct {
		name       string
		auth       stubAuth
		granter    stubGranter
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no session",
			auth:       stubAuth{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "verb not granted",
			auth:       stubAuth{id: operator},
			granter:    stubGranter{grants: authz.Grants{View: true}},
			wantStatus: http.StatusForbidden,
			wantBody:   `Permission denied. User "Op" may not suspend findings in namespace "maul".`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, actionFinding(v1alpha1.PhaseQueued))
			s.auth, s.granter = tc.auth, tc.granter
			res, body := postAction(t, s, "fnd-1", "suspend")
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if tc.wantBody != "" && body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

func TestHandleActionMissingFinding(t *testing.T) {
	s := testServer(t)
	s.auth, s.granter = stubAuth{id: operator}, stubGranter{grants: allGrants()}
	res, _ := postAction(t, s, "ghost", "suspend")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestHandleActionRetriesConflict(t *testing.T) {
	conflicts := 1
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(actionFinding(v1alpha1.PhaseQueued)).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if conflicts > 0 {
					conflicts--
					return apierrors.NewConflict(
						schema.GroupResource{Group: "maul.run", Resource: "findings"},
						obj.GetName(), nil)
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	s := NewServer(c, "maul", stubAuth{id: operator}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }
	res, body := postAction(t, s, "fnd-1", "suspend")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 after retry", res.StatusCode, body)
	}
	if conflicts != 0 {
		t.Fatal("the injected conflict was never returned; the handler did not write through Patch")
	}
	if !getFinding(t, c, "fnd-1").Spec.Suspend {
		t.Error("suspend not applied after conflict retry")
	}
}

// TestHandleActionPatchCarriesResourceVersion proves the spec patch is
// decided on the state it was gated against: when the finding reaches a
// terminal phase between the handler's read and its write, the patch must
// conflict (its resourceVersion is stale), the retry must re-run the gating
// on fresh state, and suspend must be refused rather than written onto a
// finished finding. A patch without the optimistic lock would go through.
func TestHandleActionPatchCarriesResourceVersion(t *testing.T) {
	raced := false
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(actionFinding(v1alpha1.PhaseQueued)).
		WithStatusSubresource(&v1alpha1.Finding{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if !raced {
					raced = true
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
	s := NewServer(c, "maul", stubAuth{id: operator}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }
	res, body := postAction(t, s, "fnd-1", "suspend")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d (%s), want 403 once the retry sees the terminal phase", res.StatusCode, body)
	}
	if getFinding(t, c, "fnd-1").Spec.Suspend {
		t.Error("suspend written onto a finding that went terminal after the gating decision")
	}
}

// mustClient unwraps the server's client for post-assertion reads.
func mustClient(s *Server) client.Client { return s.client }
