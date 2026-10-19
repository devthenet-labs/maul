// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/credgate"
)

// CredentialRecheck is how often a remediation held for a credential is
// looked at again: the broker probes a newly stored credential within
// moments, so the hold lifts within about this long of it being found
// Valid.
const CredentialRecheck = time.Minute

// ReasonCredentialResolved is the CredentialRequired reason once the hold
// lifted.
const ReasonCredentialResolved = "Resolved"

// holdForCredential reports whether the next attempt of fnd, attributed by
// attributedTo and run on harness, must wait for a person's credential
// (design L3, "Fallback"), and whom the attempt is for: under
// --user-credentials enforce, a brokered run attributed to a person who has
// no Valid ModelCredential for the harness's provider would be refused by
// the broker, so it is not created. The Finding stays Queued with
// CredentialRequired True, its message naming the person, the provider and
// the Account page, and status.credentialHold and credentialHoldProvider
// recording them (the tracking issue projects them); the spawner looks
// again every CredentialRecheck.
//
// While held, a later approval (Attribute reads the current one) moves the
// attempt only to an approver who holds a Valid credential (design revision
// 1: no keyless approver's approval can park the work, or re-park it on
// themselves). Any other approval, including one that would make the run
// the org's, leaves the attempt with the person held, so it runs as theirs
// once their credential is Valid.
//
// Once not held, a True condition is set False and the person cleared, in
// the same pass that creates the attempt; that includes a mode that is no
// longer enforce, so turning enforce off never strands a hold. Off and
// attribute otherwise never hold and never write.
func (r *SpawnerReconciler) holdForCredential(ctx context.Context, fnd *v1alpha1.Finding,
	attributedTo *v1alpha1.Attribution, harness string) (*v1alpha1.Attribution, bool, error) {
	var v credgate.Verdict
	// The attempt is judged on its harness's provider (credgate.ProviderFor:
	// claude anthropic, codex openai), so a codex run is held on, and paid
	// by, the person's OpenAI record and never their Anthropic one. A
	// harness no person's credential pays for (copilot) is never held.
	if provider, ok := credgate.ProviderFor(harness); ok && r.UserCredentials.Enforced() {
		var err error
		if attributedTo, v, err = r.judgeHeld(ctx, fnd, attributedTo, provider); err != nil {
			return attributedTo, false, err
		}
	}
	cond := metav1.Condition{
		Type: v1alpha1.ConditionCredentialRequired, Status: metav1.ConditionFalse,
		Reason: ReasonCredentialResolved, Message: "the remediation's credential is usable",
	}
	if v.Hold {
		cond.Status, cond.Reason = metav1.ConditionTrue, v.Reason
		cond.Message = credgate.Message(v, r.AccountURL, true)
	} else if !meta.IsStatusConditionTrue(fnd.Status.Conditions, v1alpha1.ConditionCredentialRequired) &&
		fnd.Status.CredentialHold == nil {
		return attributedTo, false, nil
	}
	if c := meta.FindStatusCondition(fnd.Status.Conditions, cond.Type); c != nil && c.Status == cond.Status &&
		c.Reason == cond.Reason && c.Message == cond.Message && sameUser(fnd.Status.CredentialHold, v.User) &&
		fnd.Status.CredentialHoldProvider == v.Provider {
		return attributedTo, v.Hold, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.Finding
		if err := r.Get(ctx, client.ObjectKeyFromObject(fnd), &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if cur.Status.Phase != v1alpha1.PhaseQueued {
			return nil
		}
		cond.ObservedGeneration = cur.Generation
		changed := meta.SetStatusCondition(&cur.Status.Conditions, cond)
		if !sameUser(cur.Status.CredentialHold, v.User) {
			cur.Status.CredentialHold = v.User.DeepCopy()
			changed = true
		}
		if cur.Status.CredentialHoldProvider != v.Provider {
			cur.Status.CredentialHoldProvider = v.Provider
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Status().Update(ctx, &cur)
	})
	if err != nil {
		return attributedTo, false, err
	}
	if v.Hold {
		r.log().LogAttrs(ctx, slog.LevelInfo, "remediation held for a person's model credential",
			slog.String("finding", fnd.Name), slog.String("user", v.User.Login), slog.String("reason", v.Reason),
			slog.String("provider", string(v.Provider)))
	} else {
		r.log().LogAttrs(ctx, slog.LevelInfo, "remediation credential hold lifted", slog.String("finding", fnd.Name))
	}
	return attributedTo, v.Hold, nil
}

// judgeHeld is Check for the attempt, attributed by attributedTo and paid
// by provider's credential, under enforce. On a Finding already held for
// someone else (status.credentialHold), the current approval's attribution
// stands only when it is a person whose credential for provider is Valid:
// otherwise the attempt stays the held person's, and is judged as theirs.
func (r *SpawnerReconciler) judgeHeld(ctx context.Context, fnd *v1alpha1.Finding,
	attributedTo *v1alpha1.Attribution, provider v1alpha1.ModelCredentialProvider,
) (*v1alpha1.Attribution, credgate.Verdict, error) {
	v, err := credgate.Check(ctx, r.UserCredentials, provider, attributedTo, r.Credentials)
	if err != nil {
		return attributedTo, v, err
	}
	held := fnd.Status.CredentialHold
	if held == nil || held.ID < 1 ||
		!meta.IsStatusConditionTrue(fnd.Status.Conditions, v1alpha1.ConditionCredentialRequired) {
		return attributedTo, v, nil
	}
	if p := credgate.Person(attributedTo); p != nil && (p.ID == held.ID || !v.Hold) {
		return attributedTo, v, nil
	}
	keep := r.Attribution.Stamp(&v1alpha1.Attribution{User: held.DeepCopy(), Reason: v1alpha1.AttributionApprover})
	v, err = credgate.Check(ctx, r.UserCredentials, provider, keep, r.Credentials)
	return keep, v, err
}

func sameUser(a, b *v1alpha1.UserRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
