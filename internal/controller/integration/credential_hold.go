// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/credgate"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// AnnotationProjectedCredentialHold is the hash of the last projected
// credential-hold sticky comment, "" while none was ever owed.
const AnnotationProjectedCredentialHold = "patchy.bitwisemedia.uk/projected-credential-hold"

// projectCredentialHold keeps the credential-hold sticky comment current
// (per-user credentials design, L3 "Fallback"): while remediation-
// controller holds the finding's next attempt for its approver's model
// credential (CredentialRequired True, under enforce only), one comment
// names the approver and links the Account page; once the hold lifts it is
// edited to say so. A finding never held gets no comment and no write. The
// comment is rendered from patchy's own facts (the person held,
// status.credentialHold, else the approval's login; the provider,
// status.credentialHoldProvider; the condition's reason; --account-url),
// never from the condition's message. The person held, not
// the newest approval, is named: an approval by someone without a valid
// credential leaves the hold where it was.
func (r *FindingReconciler) projectCredentialHold(
	ctx context.Context, fnd *v1alpha1.Finding, comments *issueComments,
) error {
	cond := meta.FindStatusCondition(fnd.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	projected := fnd.GetAnnotations()[AnnotationProjectedCredentialHold]
	if cond == nil || (cond.Status != metav1.ConditionTrue && projected == "") {
		return nil
	}
	c := templates.CredentialHoldComment{
		Reason: cond.Reason, AccountURL: r.AccountURL, Resolved: cond.Status != metav1.ConditionTrue,
		Provider: credgate.ProviderName(fnd.Status.CredentialHoldProvider),
		Kinds:    credgate.CredentialKinds(fnd.Status.CredentialHoldProvider),
	}
	if h := fnd.Status.CredentialHold; h != nil && h.ID > 0 && h.Login != "" {
		c.Login = h.Login
	} else if ap := fnd.Spec.Approval; ap != nil {
		if a := attribution.To(ap.By, ap.ByID, v1alpha1.AttributionApprover); a.User != nil {
			c.Login = a.User.Login
		}
	}
	body := templates.RenderCredentialHoldComment(c)
	state := hashOf(body)
	if projected == state {
		return nil
	}
	if err := comments.upsert(ctx, body); err != nil {
		return err
	}
	return r.markProjected(ctx, fnd, map[string]string{AnnotationProjectedCredentialHold: state})
}
