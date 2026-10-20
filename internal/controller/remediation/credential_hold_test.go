// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"errors"
	"strings"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/credgate"
	"github.com/devthenet-labs/maul/internal/credgate/kubecred"
)

const accountURL = "https://status.example.com/#/account"

func credential(id int64, health v1alpha1.ModelCredentialHealth) *v1alpha1.ModelCredential {
	return credentialFor(v1alpha1.ModelCredentialProviderAnthropic, id, health)
}

func credentialFor(p v1alpha1.ModelCredentialProvider, id int64,
	health v1alpha1.ModelCredentialHealth) *v1alpha1.ModelCredential {
	owner := v1alpha1.ModelCredentialOwner(id)
	return &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{Namespace: "maul", Name: v1alpha1.ModelCredentialName(owner, p)},
		Spec:       v1alpha1.ModelCredentialSpec{Owner: owner, Provider: p, Kind: v1alpha1.ModelCredentialAPIKey},
		Status:     v1alpha1.ModelCredentialStatus{Health: health},
	}
}

func enforcingSpawner(t *testing.T, mode credgate.Mode, objs ...client.Object) (*SpawnerReconciler, client.Client) {
	t.Helper()
	r, c := newSpawner(t, objs...)
	r.Attribution = attribution.ModeAttribute
	r.UserCredentials = mode
	r.Credentials = kubecred.Reader{Client: c, Namespace: "maul"}
	r.AccountURL = accountURL
	return r, c
}

func remediationExists(t *testing.T, c client.Client) bool {
	t.Helper()
	var rem v1alpha1.Remediation
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: "finding-aa-1-rem-1"}, &rem)
	if err != nil && !kerrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func reconcileSpawner(t *testing.T, r *SpawnerReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "maul", Name: "finding-aa-1"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// checkHeld fails unless fnd is held, Queued with no attempt spent, for
// alice with reason, naming her and the Account page.
func checkHeld(t *testing.T, fnd *v1alpha1.Finding, reason string) {
	t.Helper()
	cond := meta.FindStatusCondition(fnd.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reason ||
		!strings.Contains(cond.Message, "@alice") || !strings.Contains(cond.Message, accountURL) {
		t.Fatalf("CredentialRequired = %+v", cond)
	}
	if h := fnd.Status.CredentialHold; h == nil || h.Login != "alice" || h.ID != 42 {
		t.Fatalf("credentialHold = %+v, want alice", h)
	}
	if fnd.Status.Phase != v1alpha1.PhaseQueued || fnd.Status.Attempts.Remediation != 0 {
		t.Fatalf("phase %s, attempts %d: a hold must leave the Finding Queued with no attempt spent",
			fnd.Status.Phase, fnd.Status.Attempts.Remediation)
	}
}

// setHealth writes the broker's verdict on id's record.
func setHealth(t *testing.T, c client.Client, id int64, h v1alpha1.ModelCredentialHealth) {
	t.Helper()
	var mc v1alpha1.ModelCredential
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(credential(id, "")), &mc); err != nil {
		t.Fatal(err)
	}
	mc.Status.Health = h
	if err := c.Update(t.Context(), &mc); err != nil { // no status subresource in this fake
		t.Fatal(err)
	}
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
	checkHeld(t, got, credgate.ReasonMissing)
	if got.Status.CredentialHoldProvider != v1alpha1.ModelCredentialProviderAnthropic {
		t.Errorf("credentialHoldProvider = %q, want anthropic", got.Status.CredentialHoldProvider)
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
	checkHeld(t, findingNow(t, c), credgate.ReasonUnverified)

	// The broker found it Valid: the attempt is created, attributed to alice,
	// and the condition is resolved.
	setHealth(t, c, 42, v1alpha1.ModelCredentialValid)
	if res := reconcileSpawner(t, r); res.RequeueAfter != 0 {
		t.Errorf("requeue after the hold lifted = %v", res.RequeueAfter)
	}
	if !remediationExists(t, c) {
		t.Fatal("the attempt was not created once the credential was valid")
	}
	cond := meta.FindStatusCondition(findingNow(t, c).Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("CredentialRequired after lifting = %+v, want False", cond)
	}
	if h := findingNow(t, c).Status.CredentialHold; h != nil {
		t.Errorf("credentialHold after lifting = %+v, want none", h)
	}
	var rem v1alpha1.Remediation
	_ = c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: "finding-aa-1-rem-1"}, &rem)
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
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: "finding-aa-1-rem-1"},
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

// TestSpawnerHoldsACodexRunOnItsOpenAIRecord: a codex remediation is
// judged on the person's OpenAI record alone. Alice's Valid Anthropic
// record does not let it through; it is held with credentialHoldProvider
// openai and a message naming OpenAI, and created once her OpenAI key is
// Valid. A claude remediation of hers is not held on the missing OpenAI key.
func TestSpawnerHoldsACodexRunOnItsOpenAIRecord(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	inv := invChild()
	inv.Status.RemediationParameters.Model = "openai/gpt-5.3-codex"
	r, c := enforcingSpawner(t, credgate.ModeEnforce, fnd, inv, credential(42, v1alpha1.ModelCredentialValid))
	reconcileSpawner(t, r)
	if remediationExists(t, c) {
		t.Fatal("a codex remediation ran on an Anthropic record")
	}
	got := findingNow(t, c)
	checkHeld(t, got, credgate.ReasonMissing)
	if got.Status.CredentialHoldProvider != v1alpha1.ModelCredentialProviderOpenAI {
		t.Errorf("credentialHoldProvider = %q, want openai", got.Status.CredentialHoldProvider)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if !strings.Contains(cond.Message, "no OpenAI credential") || strings.Contains(cond.Message, "setup-token") {
		t.Errorf("message = %q, want OpenAI named", cond.Message)
	}

	if err := c.Create(t.Context(), credentialFor(v1alpha1.ModelCredentialProviderOpenAI, 42,
		v1alpha1.ModelCredentialValid)); err != nil {
		t.Fatal(err)
	}
	reconcileSpawner(t, r)
	if !remediationExists(t, c) {
		t.Fatal("not created once the OpenAI key was Valid")
	}
	if p := findingNow(t, c).Status.CredentialHoldProvider; p != "" {
		t.Errorf("credentialHoldProvider after lifting = %q, want none", p)
	}

	// A claude remediation of hers runs on her Anthropic record.
	fnd2 := queuedFinding(v1alpha1.PhaseQueued)
	fnd2.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	r2, c2 := enforcingSpawner(t, credgate.ModeEnforce, fnd2, invChild(), credential(42, v1alpha1.ModelCredentialValid))
	reconcileSpawner(t, r2)
	if !remediationExists(t, c2) {
		t.Fatal("a claude remediation was held although her Anthropic record is Valid")
	}
}

// approveAs writes an approval onto the stored Finding as a direct API
// writer would (or, with apply, through action.ApplyWithID as the tracking
// issue and the status page do).
func approveAs(t *testing.T, c client.Client, by string, id int64, apply bool) {
	t.Helper()
	cur := findingNow(t, c)
	if apply {
		if changed, err := action.ApplyWithID(cur, action.VerbApprove, by, id, "", crdClock.Add(1)); err != nil ||
			!changed {
			t.Fatalf("approve as %s: changed=%v err=%v", by, changed, err)
		}
	} else {
		cur.Spec.Approval = &v1alpha1.Approval{By: by, ByID: id, At: metav1.NewTime(crdClock.Add(1))}
	}
	if err := c.Update(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
}

// TestSpawnerReapprovalByKeylessApproverKeepsHold: while alice's attempt is
// held (her credential stored, not yet verified), a later approval by bob,
// who has no credential, does not move the hold onto him: the condition and
// status.credentialHold still name alice, and once her credential is Valid
// the attempt runs as hers.
func TestSpawnerReapprovalByKeylessApproverKeepsHold(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	r, c := enforcingSpawner(t, credgate.ModeEnforce, fnd, invChild(),
		credential(42, v1alpha1.ModelCredentialUnknown))
	reconcileSpawner(t, r)
	checkHeld(t, findingNow(t, c), credgate.ReasonUnverified)

	approveAs(t, c, "bob", 7, true)
	reconcileSpawner(t, r)
	if remediationExists(t, c) {
		t.Fatal("created after a keyless approver's approval")
	}
	got := findingNow(t, c)
	checkHeld(t, got, credgate.ReasonUnverified)
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionCredentialRequired); strings.Contains(
		cond.Message, "@bob") {
		t.Errorf("the hold moved onto the keyless approver: %s", cond.Message)
	}

	setHealth(t, c, 42, v1alpha1.ModelCredentialValid)
	reconcileSpawner(t, r)
	var rem v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: "finding-aa-1-rem-1"},
		&rem); err != nil {
		t.Fatalf("not created once the held person's credential was valid: %v", err)
	}
	if u := rem.Spec.AttributedTo.User; u == nil || u.Login != "alice" || u.ID != 42 {
		t.Errorf("attributedTo = %+v, want alice", rem.Spec.AttributedTo)
	}
}

// TestSpawnerReapprovalWithoutIDKeepsHold: while held, an approval with no
// GitHub id (refused by action.Apply, so only a direct API writer makes
// one) would make the run the org's; it neither lifts the hold nor spends
// the org credential.
func TestSpawnerReapprovalWithoutIDKeepsHold(t *testing.T) {
	fnd := queuedFinding(v1alpha1.PhaseQueued)
	fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
	r, c := enforcingSpawner(t, credgate.ModeEnforce, fnd, invChild())
	reconcileSpawner(t, r)
	checkHeld(t, findingNow(t, c), credgate.ReasonMissing)

	cur := findingNow(t, c)
	if _, err := action.Apply(cur, action.VerbApprove, "github:carol", "", crdClock.Add(1)); !errors.Is(err,
		action.ErrUnavailable) {
		t.Fatalf("a status-page approval while held: %v, want ErrUnavailable", err)
	}
	approveAs(t, c, "github:carol", 0, false)
	reconcileSpawner(t, r)
	if remediationExists(t, c) {
		t.Fatal("an approval with no GitHub id lifted the hold onto the org credential")
	}
	checkHeld(t, findingNow(t, c), credgate.ReasonMissing)
}

// TestSpawnerHoldClearedWhenEnforceTurnedOff: a Finding held under enforce
// whose controller then runs under attribute (or off) gets its attempt and
// the hold resolved, so the tracking issue's comment is edited to say so
// and approve stops being taken outside its usual phases.
func TestSpawnerHoldClearedWhenEnforceTurnedOff(t *testing.T) {
	for _, mode := range []credgate.Mode{credgate.ModeAttribute, credgate.ModeOff} {
		t.Run(string(mode), func(t *testing.T) {
			fnd := queuedFinding(v1alpha1.PhaseQueued)
			fnd.Spec.Approval = &v1alpha1.Approval{By: "alice", ByID: 42, At: metav1.NewTime(crdClock)}
			r, c := enforcingSpawner(t, credgate.ModeEnforce, fnd, invChild())
			reconcileSpawner(t, r)
			checkHeld(t, findingNow(t, c), credgate.ReasonMissing)

			r.UserCredentials = mode
			reconcileSpawner(t, r)
			if !remediationExists(t, c) {
				t.Fatal("still held once enforce was turned off")
			}
			got := findingNow(t, c)
			if cond := meta.FindStatusCondition(got.Status.Conditions,
				v1alpha1.ConditionCredentialRequired); cond == nil || cond.Status != metav1.ConditionFalse {
				t.Errorf("CredentialRequired = %+v, want False", cond)
			}
			if got.Status.CredentialHold != nil {
				t.Errorf("credentialHold = %+v, want none", got.Status.CredentialHold)
			}
		})
	}
}
