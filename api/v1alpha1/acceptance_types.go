// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

// EmptyListDigest is the digest of an empty acceptance list, outcomes or
// slice 1's criteria alike (the sha256 of "[]"): IntentAcceptance.Digest
// for every plan the evaluator records, and its OutcomesDigest for a plan
// that states no outcomes. internal/acceptance pins it to both of its
// digests of the empty list; the schema repeats it as a literal.
const EmptyListDigest = "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"

// VerificationHead is one previewed component's revision in the combined
// change a verification ran against: what the Preview spec runs.
type VerificationHead struct {
	// Component is the previewed repository key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Component string `json:"component"`
	// Repository is the repository's https URL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	Repository string `json:"repository"`
	// Revision is the full commit SHA the component runs.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision"`
}

// The evaluator's acceptance (DESIGN revision 3, SPEC S1-01): an approved
// plan declares outcomes, plain statements of what must hold, instead of
// criteria; the evaluator uses the change and reports, per outcome, what it
// saw, citing calls of the round's ledger ("evaluator observed").
// AcceptanceOutcome is held field for field to the plan grammar's outcome
// (internal/acceptance/crdconv's pin test).

// OutcomeKind says whether an outcome is new behaviour the change must
// introduce (change) or behaviour it must keep (keep).
// +kubebuilder:validation:Enum=change;keep
type OutcomeKind string

// Outcome kinds.
const (
	// OutcomeKindChange: the change introduces the outcome. An unset kind
	// is a change (EffectiveKind).
	OutcomeKindChange OutcomeKind = "change"
	// OutcomeKindKeep: the outcome held before the change and must still
	// hold after it.
	OutcomeKindKeep OutcomeKind = "keep"
)

// AcceptanceOutcome is one outcome of an approved plan: a statement of what
// must hold, the issue line it answers, and whether a human checks it.
type AcceptanceOutcome struct {
	// ID names the outcome, unique within its plan.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	ID string `json:"id"`
	// Outcome is the approved one-line statement of what must hold.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=500
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	Outcome string `json:"outcome"`
	// From quotes the issue line the outcome answers.
	// +optional
	// +kubebuilder:validation:MaxLength=300
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	From string `json:"from,omitempty"`
	// Kind is change or keep; unset is change.
	// +optional
	Kind OutcomeKind `json:"kind,omitempty"`
	// Manual marks an outcome only a human can check (/maul accept).
	// +optional
	Manual bool `json:"manual,omitempty"`
	// Reason says why a manual outcome needs a human.
	// +optional
	// +kubebuilder:validation:MaxLength=120
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	Reason string `json:"reason,omitempty"`
}

// EffectiveKind is the outcome's kind, change when unset.
func (o AcceptanceOutcome) EffectiveKind() OutcomeKind {
	if o.Kind == "" {
		return OutcomeKindChange
	}
	return o.Kind
}
