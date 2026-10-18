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
	"github.com/bitwise-media-group/patchy/internal/usagemeter"
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
// (design L3, "Fallback"): under --user-credentials enforce, a brokered run
// attributed to a person who has no Valid ModelCredential would be refused
// by the broker, so it is not created. The Finding stays Queued with
// CredentialRequired True, its message naming the person and the Account
// page (the tracking issue projects it); the spawner looks again every
// CredentialRecheck. A later approval by someone else changes whom the
// attempt is for (Attribute reads the current approval), so it is judged
// afresh. Once not held, a True condition is set False, in the same pass
// that creates the attempt. Off and attribute never hold and never write.
func (r *SpawnerReconciler) holdForCredential(ctx context.Context, fnd *v1alpha1.Finding,
	attributedTo *v1alpha1.Attribution, harness string) (bool, error) {
	if !r.UserCredentials.Enforced() {
		return false, nil
	}
	var v credgate.Verdict
	if usagemeter.Brokered(harness) {
		var err error
		if v, err = credgate.Check(ctx, r.UserCredentials, attributedTo, r.Credentials); err != nil {
			return false, err
		}
	}
	cond := metav1.Condition{
		Type: v1alpha1.ConditionCredentialRequired, Status: metav1.ConditionFalse,
		Reason: ReasonCredentialResolved, Message: "the remediation's credential is usable",
	}
	if v.Hold {
		cond.Status, cond.Reason = metav1.ConditionTrue, v.Reason
		cond.Message = credgate.Message(v, r.AccountURL, true)
	} else if !meta.IsStatusConditionTrue(fnd.Status.Conditions, v1alpha1.ConditionCredentialRequired) {
		return false, nil
	}
	if c := meta.FindStatusCondition(fnd.Status.Conditions, cond.Type); c != nil && c.Status == cond.Status &&
		c.Reason == cond.Reason && c.Message == cond.Message {
		return v.Hold, nil
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
		if !meta.SetStatusCondition(&cur.Status.Conditions, cond) {
			return nil
		}
		return r.Status().Update(ctx, &cur)
	})
	if err != nil {
		return false, err
	}
	if v.Hold {
		r.log().LogAttrs(ctx, slog.LevelInfo, "remediation held for a person's model credential",
			slog.String("finding", fnd.Name), slog.String("user", v.User.Login), slog.String("reason", v.Reason))
	} else {
		r.log().LogAttrs(ctx, slog.LevelInfo, "remediation credential hold lifted", slog.String("finding", fnd.Name))
	}
	return v.Hold, nil
}
