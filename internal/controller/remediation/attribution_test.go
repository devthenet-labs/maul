// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"reflect"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// TestSpawnerAttribution is the design §2 row for a Finding remediation:
// the approver when the approval is held (re-attributed to the org when the
// approver has no GitHub id, as every status-page and CLI approval), else
// the org; nothing at all under mode off.
func TestSpawnerAttribution(t *testing.T) {
	at := metav1.NewTime(crdClock)
	for _, tc := range []struct {
		name     string
		mode     attribution.Mode
		approval *v1alpha1.Approval
		want     *v1alpha1.Attribution
	}{
		{"off stamps nothing", attribution.ModeOff,
			&v1alpha1.Approval{By: "alice", ByID: 42, At: at}, nil},
		{"unheld is the org's", attribution.ModeAttribute, nil,
			&v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}},
		{"a GitHub approval is the approver's", attribution.ModeAttribute,
			&v1alpha1.Approval{By: "alice", ByID: 42, At: at},
			&v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "alice", ID: 42},
				Reason: v1alpha1.AttributionApprover}},
		{"a status-page approval is unregistered", attribution.ModeAttribute,
			&v1alpha1.Approval{By: "github:alice", At: at},
			&v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a CLI approval is unregistered", attribution.ModeAttribute,
			&v1alpha1.Approval{By: "arn:aws:iam::111122223333:role/dev", At: at},
			&v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a forged id without a GitHub login is unregistered", attribution.ModeAttribute,
			&v1alpha1.Approval{By: "github:alice", ByID: 42, At: at},
			&v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fnd := queuedFinding(v1alpha1.PhaseQueued)
			fnd.Spec.Approval = tc.approval
			r, c := newSpawner(t, fnd, invChild())
			r.Attribution = tc.mode
			spawnOnce(t, r)
			var rem v1alpha1.Remediation
			key := types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-1"}
			if err := c.Get(t.Context(), key, &rem); err != nil {
				t.Fatalf("Remediation not created: %v", err)
			}
			if !reflect.DeepEqual(rem.Spec.AttributedTo, tc.want) {
				t.Errorf("attributedTo = %+v, want %+v", rem.Spec.AttributedTo, tc.want)
			}
			// The usage finalizer rides with the stamp.
			if got := slices.Contains(rem.Finalizers, v1alpha1.FinalizerUsageRollup); got != (tc.want != nil) {
				t.Errorf("usage finalizer = %v, want %v (finalizers %v)", got, tc.want != nil, rem.Finalizers)
			}
		})
	}
}

// TestLaunchAttribution: the Job carries the annotation of its run's own
// stamp, "org" for an unstamped run, under attribute; none under off.
func TestLaunchAttribution(t *testing.T) {
	person := &v1alpha1.Attribution{
		User: &v1alpha1.UserRef{Login: "alice", ID: 42}, Reason: v1alpha1.AttributionApprover,
	}
	for _, tc := range []struct {
		name  string
		mode  attribution.Mode
		stamp *v1alpha1.Attribution
		want  string
	}{
		{"off", attribution.ModeOff, person, ""},
		{"a person", attribution.ModeAttribute, person, "github-id:42"},
		{"the org", attribution.ModeAttribute, attribution.Org(), "org"},
		{"unstamped", attribution.ModeAttribute, nil, "org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := runningRemediation()
			rem := objs[1].(*v1alpha1.Remediation)
			rem.Status.JobRef = nil // granted, not launched
			rem.Spec.AttributedTo = tc.stamp
			runner := &fakeCRRunner{}
			r, _ := newRemediation(t, runner, &fakeForge{}, objs...)
			r.Attribution = tc.mode
			remOnce(t, r, "finding-aa-1-rem-1")
			if len(runner.created) != 1 {
				t.Fatalf("jobs = %d, want 1", len(runner.created))
			}
			if got := runner.created[0].AttributedTo; got != tc.want {
				t.Errorf("Job attributed to %q, want %q", got, tc.want)
			}
		})
	}
}
