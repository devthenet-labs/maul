// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package investigation

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// TestGateAttribution: an investigation has no human behind it, so under
// mode attribute the gate stamps it the org's, whoever approved or expedited
// the finding; under off it stamps nothing.
func TestGateAttribution(t *testing.T) {
	for _, tc := range []struct {
		mode attribution.Mode
		want *v1alpha1.Attribution
	}{
		{attribution.ModeOff, nil},
		{"", nil},
		{attribution.ModeAttribute, &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			fnd := enhancedFinding()
			// A human expedited it: still nobody's investigation.
			fnd.Spec.Expedite = &v1alpha1.ActionRequest{By: "alice", ByID: 7, At: metav1.NewTime(clock)}
			repo := &v1alpha1.Repository{
				ObjectMeta: metav1.ObjectMeta{Name: "finding-aa-1-src", Namespace: "patchy"},
				Spec:       v1alpha1.RepositorySpec{URL: "https://github.com/acme/orders"},
			}
			meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "ArtifactReady",
			})
			repo.Status.ResolvedSHA = "abc123"
			repo.Status.Artifact = &v1alpha1.Artifact{URL: "http://arts/x.tar.gz", Digest: "d"}
			r, c := newGate(t, fnd, testForge(), repo)
			r.Attribution = tc.mode
			gateOnce(t, r)

			var inv v1alpha1.Investigation
			key := types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-inv-1"}
			if err := c.Get(t.Context(), key, &inv); err != nil {
				t.Fatalf("Investigation not created: %v", err)
			}
			if !reflect.DeepEqual(inv.Spec.AttributedTo, tc.want) {
				t.Errorf("attributedTo = %+v, want %+v", inv.Spec.AttributedTo, tc.want)
			}
		})
	}
}

// TestLaunchAttribution: under attribute every investigation Job carries
// attributed-to "org", even for a spec that names a person (the admission
// policy admits nothing else from this controller); under off none.
func TestLaunchAttribution(t *testing.T) {
	person := &v1alpha1.Attribution{
		User: &v1alpha1.UserRef{Login: "alice", ID: 7}, Reason: v1alpha1.AttributionApprover,
	}
	for _, tc := range []struct {
		name  string
		mode  attribution.Mode
		stamp *v1alpha1.Attribution
		want  string
	}{
		{"off", attribution.ModeOff, attribution.Org(), ""},
		{"attribute, org", attribution.ModeAttribute, attribution.Org(), "org"},
		{"attribute, unstamped", attribution.ModeAttribute, nil, "org"},
		{"attribute, a person on the spec", attribution.ModeAttribute, person, "org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := launchable(srcRepo(nil, readyCond()))
			objs[1].(*v1alpha1.Investigation).Spec.AttributedTo = tc.stamp
			runner := &fakeRunner{}
			r, _ := newInvestigation(t, runner, objs...)
			r.Attribution = tc.mode
			applyOnce(t, r)
			if len(runner.created) != 1 {
				t.Fatalf("jobs created = %d, want 1", len(runner.created))
			}
			if got := runner.created[0].AttributedTo; got != tc.want {
				t.Errorf("Job attributed to %q, want %q", got, tc.want)
			}
		})
	}
}
