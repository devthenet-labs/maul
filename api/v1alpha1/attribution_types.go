// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

// AnnotationAttributedTo records, on an agent Job and its pod template, who
// the run is attributed to: "org", or "github-id:<numeric id>" for a person
// (internal/attribution.Value). It is stamped by the launching controller
// from the run's spec.attributedTo only under --attribution-mode attribute,
// and the chart's attribution admission policies refuse it from anyone else.
// An annotation rather than a label, because a value naming a person must
// never become a selector anyone could match pods by.
const AnnotationAttributedTo = "maul.run/attributed-to"

// UserRef identifies a person by their GitHub account. The numeric id is the
// identity: a login can be renamed, and a freed login taken by someone else,
// so a login alone never says who a run was for. The login is kept for
// display.
type UserRef struct {
	// Login is the GitHub login when the reference was recorded, for display
	// only. Never a Bot: no bot is ever attributed a run.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	Login string `json:"login"`
	// ID is GitHub's numeric account id: the identity.
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id"`
}

// AttributionReason says why a run is attributed to whom it is.
// +kubebuilder:validation:Enum=org;requester;approver;commenter;reviewer;submitter;approver-unregistered;submitter-unregistered
type AttributionReason string

// Attribution reasons. The person reasons carry a user; the others do not.
const (
	// AttributionOrg: no human is behind the run (a Finding investigation,
	// an unheld remediation, a plan triggered by someone who is not an
	// approver).
	AttributionOrg AttributionReason = "org"
	// AttributionRequester: an intent plan, triggered by an approver.
	AttributionRequester AttributionReason = "requester"
	// AttributionApprover: an intent build or check-fix run, by the plan's
	// approver; a Finding remediation, by the Finding's approver.
	AttributionApprover AttributionReason = "approver"
	// AttributionCommenter: an intent revise round started by an approver's
	// /maul revise (or retry) comment, by its commenter.
	AttributionCommenter AttributionReason = "commenter"
	// AttributionReviewer: an intent revise round started by approvers'
	// reviews, by the author of the newest review the round consumed.
	AttributionReviewer AttributionReason = "reviewer"
	// AttributionSubmitter: an evaluation unit, by the Evaluation's
	// submitter.
	AttributionSubmitter AttributionReason = "submitter"
	// AttributionApproverUnregistered: the human the table names (an
	// approver, requester, commenter or reviewer) has no maul identity (no
	// GitHub numeric id is known for them), so under mode attribute the run
	// is the org's.
	AttributionApproverUnregistered AttributionReason = "approver-unregistered"
	// AttributionSubmitterUnregistered: an evaluation unit whose submitter
	// has no maul identity, so the unit is the org's.
	AttributionSubmitterUnregistered AttributionReason = "submitter-unregistered"
)

// PersonReason reports whether reason attributes a run to a person, whose
// UserRef the Attribution then carries.
func PersonReason(reason AttributionReason) bool {
	switch reason {
	case AttributionRequester, AttributionApprover, AttributionCommenter, AttributionReviewer,
		AttributionSubmitter:
		return true
	}
	return false
}

// Attribution is who an agent run is for, stamped once, at creation, by the
// run's own controller under --attribution-mode attribute, on the run's
// immutable spec. A person reason carries the person; every other reason
// carries none, so the run is the org's.
// +kubebuilder:validation:XValidation:rule="has(self.user) == (self.reason in ['requester', 'approver', 'commenter', 'reviewer', 'submitter'])",message="attributedTo.user is set exactly when the reason names a person"
type Attribution struct {
	// User is the person the run is attributed to; nil for the org.
	// +optional
	User *UserRef `json:"user,omitempty"`
	// Reason says why.
	Reason AttributionReason `json:"reason"`
}
