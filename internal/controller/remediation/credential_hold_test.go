// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"strings"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/action"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/credgate"
	"github.com/bitwise-media-group/patchy/internal/credgate/kubecred"
)

const accountURL = "https://status.example.com/#/account"

func credential(id int64, health v1alpha1.ModelCredentialHealth) *v1alpha1.ModelCredential {
	owner := v1alpha1.ModelCredentialOwner(id)
	return &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Namespace: "patchy",
			Name: v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderAnthropic)},
		Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: v1alpha1.ModelCredentialProviderAnthropic,
			Kind: v1alpha1.ModelCredentialAPIKey},
		Status: v1alpha1.ModelCredentialStatus{Health: health},
	}
}

func enforcingSpawner(t *testing.T, mode credgate.Mode, objs ...client.Object) (*SpawnerReconciler, client.Client) {
	t.Helper()
	r, c := newSpawner(t, objs...)
	r.Attribution = attribution.ModeAttribute
	r.UserCredentials = mode
	r.Credentials = kubecred.Reader{Client: c, Namespace: "patchy"}
	r.AccountURL = accountURL
	return r, c
}

func remediationExists(t *testing.T, c client.Client) bool {
	t.Helper()
	var rem v1alpha1.Remediation
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-1"}, &rem)
	if err != nil && !kerrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func reconcileSpawner(t *testing.T, r *SpawnerReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// TestSpawnerHoldsForCredential: under enforce, an attempt attributed to an
// approver with no Valid credential is not created; the Finding stays
// Queued with CredentialRequired naming them and the Account page, and the
// attempt is created, the condition resolved, once the credential is Valid.
func TestSpawnerHoldsForCredential(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	r, c := enforcingSpawner(t, credgate.ModeEnforce, fnd, invChild())

	res := reconcileSpawner(t, r)
	if remediationExists(t, c) {
		t.Fatal("a remediation was created for a person with no credential")
	}
	if res.RequeueAfter != CredentialRecheck {
		t.Errorf("requeue = %v, want %v", res.RequeueAfter, CredentialRecheck)
	}
	got := findingNow(t, c)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != credgate.ReasonMissing ||
		!strings.Contains(cond.Message, "@alice") || !strings.Contains(cond.Message, accountURL) {
		t.Fatalf("CredentialRequired = %+v", cond)
	}
	if got.Status.Phase != v1alpha1.PhaseQueued || got.Status.Attempts.Remediation != 0 {
		t.Fatalf("phase %s, attempts %d: a hold must leave the Finding Queued with no attempt spent",
			got.Status.Phase, got.Status.Attempts.Remediation)
	}
	rv := got.ResourceVersion
	reconcileSpawner(t, r)
	if findingNow(t, c).ResourceVersion != rv {
		t.Error("an unchanged hold was written again")
	}

	// Stored but not yet probed: still held, now saying so.
	if err := c.Create(t.Context(), credential(42, v1alpha1.ModelCredentialUnknown)); err != nil {
		t.Fatal(err)
	}
	reconcileSpawner(t, r)
	cond = meta.FindStatusCondition(findingNow(t, c).Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if remediationExists(t, c) || cond.Reason != credgate.ReasonUnverified {
		t.Fatalf("unverified: created=%v cond=%+v", remediationExists(t, c), cond)
	}

	// The broker found it Valid: the attempt is created, attributed to alice,
	// and the condition is resolved.
	var mc v1alpha1.ModelCredential
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(credential(42, "")), &mc); err != nil {
		t.Fatal(err)
	}
	mc.Status.Health = v1alpha1.ModelCredentialValid
	if err := c.Update(t.Context(), &mc); err != nil { // no status subresource in this fake
		t.Fatal(err)
	}
	if res := reconcileSpawner(t, r); res.RequeueAfter != 0 {
		t.Errorf("requeue after the hold lifted = %v", res.RequeueAfter)
	}
	if !remediationExists(t, c) {
		t.Fatal("the attempt was not created once the credential was valid")
	}
	cond = meta.FindStatusCondition(findingNow(t, c).Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("CredentialRequired after lifting = %+v, want False", cond)
	}
	var rem v1alpha1.Remediation
	_ = c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-1"}, &rem)
	if u := rem.Spec.AttributedTo.User; u == nil || u.ID != 42 {
		t.Errorf("attributedTo = %+v, want alice", rem.Spec.AttributedTo)
	}
}

// TestSpawnerReapprovalReplacesAttribution: while held, a later approval
// (accepted by action.Apply only while the hold stands) by an approver who
// holds a Valid credential replaces whom the attempt is for, so it runs.
func TestSpawnerReapprovalReplacesAttribution(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	r, c := enforcingSpawner(t, credgate.ModeEnforce, fnd, invChild(), credential(7, v1alpha1.ModelCredentialValid))
	reconcileSpawner(t, r)
	if remediationExists(t, c) {
		t.Fatal("not held")
	}
	cur := findingNow(t, c)
	changed, err := action.ApplyWithID(cur, action.VerbApprove, "bob", 7, "", crdClock.Add(1))
	if err != nil || !changed {
		t.Fatalf("a re-approval while held: changed=%v err=%v", changed, err)
	}
	if err := c.Update(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileSpawner(t, r)
	var rem v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: "finding-aa-1-rem-1"},
		&rem); err != nil {
		t.Fatalf("not created after a key holder's approval: %v", err)
	}
	if u := rem.Spec.AttributedTo.User; u == nil || u.Login != "bob" || u.ID != 7 {
		t.Errorf("attributedTo = %+v, want bob", rem.Spec.AttributedTo)
	}
}

// TestSpawnerNeverHoldsOutsideEnforce: off and attribute, an org run, a
// non-brokered harness: created as before, no condition written.
func TestSpawnerNeverHoldsOutsideEnforce(t *testing.T) {
	approved := &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	for _, tc := range []struct {
		name     string
		mode     credgate.Mode
		approval *v1alpha1.Approval
		model    string
	}{
		{"off", credgate.ModeOff, approved, ""},
		{"attribute", credgate.ModeAttribute, approved, ""},
		{"enforce, an org run", credgate.ModeEnforce, nil, ""},
		{"enforce, a status-page approval (no id)", credgate.ModeEnforce,
			&v1alpha1.Approval{By: "github:alice", At: metav1.NewTime(crdClock)}, ""},
		{"enforce, a non-brokered harness", credgate.ModeEnforce, approved, "openai/gpt-5.3-codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fnd := queuedFinding(v1alpha1.PhaseQueued)
			fnd.Spec.Approval = tc.approval
			inv := invChild()
			if tc.model != "" {
				inv.Status.RemediationParameters.Model = tc.model
			}
			r, c := enforcingSpawner(t, tc.mode, fnd, inv)
			reconcileSpawner(t, r)
			if !remediationExists(t, c) {
				t.Fatal("held")
			}
			if meta.FindStatusCondition(findingNow(t, c).Status.Conditions,
				v1alpha1.ConditionCredentialRequired) != nil {
				t.Error("a CredentialRequired condition was written")
			}
		})
	}
}
