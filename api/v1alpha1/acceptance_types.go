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
// criteria; the evaluator judges them against the live preview and saves
// deterministic checks that a later round replays. AcceptanceOutcome is
// held field for field to the plan grammar's outcome
// (internal/acceptance/crdconv's pin test); a SavedCheck carries a check of
// internal/checks as its canonical JSON, which the schema stores opaque and
// bounded: the grammar, its validation and its digest live in internal/.

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

// CheckForm is how a saved check runs (internal/checks.Form).
// +kubebuilder:validation:Enum=http;browser;exec;run
type CheckForm string

// Check forms.
const (
	CheckFormHTTP    CheckForm = "http"
	CheckFormBrowser CheckForm = "browser"
	CheckFormExec    CheckForm = "exec"
	// CheckFormRun is a program maul compiles against the tree and runs
	// (slice S2); internal/acceptance.CheckFormRun is its twin.
	CheckFormRun CheckForm = "run"
)

// MaxSavedCheckBytes bounds a saved check's canonical JSON, in bytes, for
// every form. internal/checks admits an exec check whose program files reach
// 64 KiB, but a saved check travels inline in a PreviewVerification (up to
// MaxSavedChecks of them, beside their confirmed copies on its status) and in
// the Intent's check store, a ConfigMap of at most 1 MiB, and neither could
// hold 40 such checks: an exec check over this bound is not saved
// (intent-controller refuses the proposal), so saved programs stay small.
// The schema markers repeat it as a literal; keep them in lockstep.
const MaxSavedCheckBytes = 8 << 10

// MaxSavedChecks bounds the saved checks of one Intent, and so of one
// PreviewVerification. The schema markers repeat it as a literal.
const MaxSavedChecks = 40

// SavedCheck is one confirmed, reviewed check of an approved outcome: its
// canonical JSON (internal/checks.Canonical) and digest. The body is the
// agent's, validated by internal/checks before it was ever run, and is
// untrusted text: it is shown only as quoted data.
// +kubebuilder:validation:XValidation:rule="size(bytes(self.body)) <= 8192",message="a saved check's body is at most 8 KiB (MaxSavedCheckBytes)"
type SavedCheck struct {
	// ID names the check: its outcome's id with an optional .<name>.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}(\.[a-z0-9][a-z0-9-]{0,31})?$`
	ID string `json:"id"`
	// Outcome is the id of the outcome the check is bound to.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	Outcome string `json:"outcome"`
	// Form is how the check runs.
	Form CheckForm `json:"form"`
	// Body is the check's canonical JSON, at most MaxSavedCheckBytes bytes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=8192
	Body string `json:"body"`
	// Digest is internal/checks.Digest of the body: what every dry run,
	// confirmation, replay and review is recorded against.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Expect is fail for a check saved to show a defect: proposed for a fail
	// verdict, its dry run and its confirmation red by assertion, reviewed
	// with the red question. It is the check of the outcome as approved all
	// the same, so it passes once the defect is fixed. Unset for a check
	// saved green.
	// +optional
	Expect CheckExpectation `json:"expect,omitempty"`
	// FailedStep is, for a check saved red, the step (1-based) at which its
	// confirmation failed by assertion.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	FailedStep int32 `json:"failedStep,omitempty"`
	// FailedAssertion is, for a check saved red, which assertion of
	// FailedStep failed (CheckRun.AssertionID). Until the check replays green
	// once, only a run red by this same assertion reproduces its defect.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	FailedAssertion string `json:"failedAssertion,omitempty"`
}

// CheckExpectation is what a check was confirmed to do when it was saved.
// +kubebuilder:validation:Enum=fail
type CheckExpectation string

// CheckExpectFail is a check saved red: it showed a defect of its outcome.
const CheckExpectFail CheckExpectation = "fail"

// CheckStoreRef names the Intent's check store: an immutable ConfigMap
// owned by the Intent, in the release namespace, holding the saved checks
// of one approved plan revision. It is the store of record; a
// PreviewVerification carries a copy of its checks inline (SPEC Q1).
type CheckStoreRef struct {
	// ConfigMap names the store.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// Digest is the digest of the stored checks: what a
	// PreviewVerification's checksDigest must equal.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Count is how many checks the store holds.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=40
	Count int32 `json:"count"`
}

// ReviewAnswer is the check reviewer's answer on whether a check proves the
// outcome it is bound to.
// +kubebuilder:validation:Enum=proves;does_not_prove;unclear
type ReviewAnswer string

// Review answers.
const (
	ReviewProves       ReviewAnswer = "proves"
	ReviewDoesNotProve ReviewAnswer = "does_not_prove"
	ReviewUnclear      ReviewAnswer = "unclear"
)

// CheckReviewRecord is one cached check review: the answer a checkreview
// run gave for one check against one outcome statement, keyed by both
// digests, so a changed check or outcome is reviewed again.
type CheckReviewRecord struct {
	// CheckDigest is the reviewed check's digest.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CheckDigest string `json:"checkDigest"`
	// OutcomeDigest is the digest of the outcome it was reviewed against.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	OutcomeDigest string `json:"outcomeDigest"`
	// Answer is the reviewer's.
	Answer ReviewAnswer `json:"answer"`
	// Expect is fail when the reviewer was asked the red question: the
	// check failed, and the evaluator proposed it to show a defect. Only
	// such a review lets a check saved red support a fail.
	// +optional
	Expect CheckExpectation `json:"expect,omitempty"`
	// FailedStep and FailedAssertion are, for the red question, the
	// failing assertion it judged: a red review counts only for that one.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	FailedStep int32 `json:"failedStep,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=512
	FailedAssertion string `json:"failedAssertion,omitempty"`
	// Run is the checkreview IntentRun that answered.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Run string `json:"run"`
	// Check is the reviewed check's id, and Reason the reviewer's one-line
	// reason for its answer, bounded and made visible: a later round's
	// evaluator is told why a check was refused, by outcome and check id,
	// so it strengthens the check rather than proposes it again. Unset on
	// a review recorded before they were kept.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}(\.[a-z0-9][a-z0-9-]{0,31})?$`
	Check string `json:"check,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=600
	Reason string `json:"reason,omitempty"`
}
