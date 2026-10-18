// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

func holdComments(tr *fakeTracker) []string {
	var out []string
	for _, c := range tr.issueComments[7] {
		if strings.HasPrefix(c.Body, templates.CredentialHoldMarker) {
			out = append(out, c.Body)
		}
	}
	return out
}

func setHold(t *testing.T, c client.Client, status metav1.ConditionStatus, reason string) {
	t.Helper()
	var f v1alpha1.Finding
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"}, &f); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionCredentialRequired,
		Status: status, Reason: reason, Message: "free text from a status writer: @mallory"})
	if err := c.Status().Update(t.Context(), &f); err != nil {
		t.Fatal(err)
	}
}

// TestProjectCredentialHold: a finding whose remediation is held for its
// approver's credential gets one sticky comment naming them (from the
// approval, never the condition's free text) and linking the Account page;
// it is edited, not re-posted, when the reason changes and when the hold
// lifts; a finding never held gets nothing.
func TestProjectCredentialHold(t *testing.T) {
	fnd := projectable(v1alpha1.PhaseQueued)
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{
		Integration: "gh", IssueNumber: 7, URL: "https://github.com/acme/orders/issues/7", State: "open",
	}
	fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(testClock)}
	tracker := newFakeTracker()
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
	r, c := newProjector(t, tracker, testIntegration(), fnd)
	r.AccountURL = "https://status.example.com/#/account"

	reconcileFinding(t, r)
	if got := holdComments(tracker); len(got) != 0 {
		t.Fatalf("a finding never held got %q", got)
	}

	setHold(t, c, metav1.ConditionTrue, "CredentialMissing")
	reconcileFinding(t, r)
	got := holdComments(tracker)
	if len(got) != 1 || !strings.Contains(got[0], "@alice") ||
		!strings.Contains(got[0], "(https://status.example.com/#/account)") || strings.Contains(got[0], "mallory") {
		t.Fatalf("hold comments = %q", got)
	}
	reconcileFinding(t, r)
	if n := len(holdComments(tracker)); n != 1 || tracker.commentEdits != 0 {
		t.Fatalf("an unchanged hold was written again: %d comments, %d edits", n, tracker.commentEdits)
	}

	setHold(t, c, metav1.ConditionTrue, "CredentialUnverified")
	reconcileFinding(t, r)
	if got := holdComments(tracker); len(got) != 1 || !strings.Contains(got[0], "not verified") {
		t.Fatalf("after the reason changed: %q", got)
	}

	setHold(t, c, metav1.ConditionFalse, "Resolved")
	reconcileFinding(t, r)
	if got := holdComments(tracker); len(got) != 1 || !strings.Contains(got[0], "no longer waits") {
		t.Fatalf("after the hold lifted: %q", got)
	}
}

// TestProjectCredentialHoldNamesThePersonHeld: after a later approval by
// someone the hold did not move to (no valid credential of their own), the
// comment still names the person held (status.credentialHold), not the
// newest approver.
func TestProjectCredentialHoldNamesThePersonHeld(t *testing.T) {
	fnd := projectable(v1alpha1.PhaseQueued)
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{
		Integration: "gh", IssueNumber: 7, URL: "https://github.com/acme/orders/issues/7", State: "open",
	}
	fnd.Spec.Approval = &v1alpha1.Approval{By: "bob", ByID: 7, At: metav1.NewTime(testClock)}
	fnd.Status.CredentialHold = &v1alpha1.UserRef{Login: "alice", ID: 42}
	fnd.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionCredentialRequired,
		Status: metav1.ConditionTrue, Reason: "CredentialUnverified", Message: "x",
		LastTransitionTime: metav1.NewTime(testClock)}}
	tracker := newFakeTracker()
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
	r, _ := newProjector(t, tracker, testIntegration(), fnd)

	reconcileFinding(t, r)
	got := holdComments(tracker)
	if len(got) != 1 || !strings.Contains(got[0], "@alice") || strings.Contains(got[0], "@bob") {
		t.Fatalf("hold comments = %q, want alice named", got)
	}
}
