// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// The acceptance criteria an approved plan declares, as the CRDs store them:
// the Intent's approved copy (status.acceptance.criteria) and the automatable
// subset a PreviewVerification runs (spec.criteria).
//
// AcceptanceCriterion mirrors internal/acceptance.Criterion field for field,
// with identical JSON tags: api/ never imports internal/, so the grammar,
// its static validation and the judging live there, and a seeded round-trip
// test (internal/acceptance/crd_pin_test.go) holds the two shapes together.
// The schema bounds below are a backstop, never the grammar: every criterion
// stored here was parsed out of digest-bound plan bytes and passed
// acceptance.Validate first. A value whose JSON shape varies by matcher (a
// request body, an expected status or JSON value) is stored as raw JSON,
// which the API server re-encodes (object keys sorted, insignificant space
// dropped): compare stored criteria by their canonical JSON
// (acceptance.Digest), never by their bytes.

// AcceptanceKind is how a criterion is checked: http by the in-slot tester,
// command in a component's or the repository's image (slice 2), manual by a
// human's /maul accept. browser and agent are reserved for later slices, and
// the schema refuses them until then.
// +kubebuilder:validation:Enum=http;command;manual
type AcceptanceKind string

// Acceptance kinds.
const (
	AcceptanceHTTP    AcceptanceKind = "http"
	AcceptanceCommand AcceptanceKind = "command"
	AcceptanceManual  AcceptanceKind = "manual"
)

// AcceptanceCriterion is one acceptance criterion of an approved plan.
// +kubebuilder:validation:XValidation:rule="self.kind != 'http' || (has(self.target) && has(self.steps))",message="an http criterion names its target and its steps"
// +kubebuilder:validation:XValidation:rule="self.kind != 'command' || (has(self.__in__) && has(self.argv))",message="a command criterion names where it runs (in) and its argv"
// +kubebuilder:validation:XValidation:rule="self.kind != 'manual' || (!has(self.steps) && !has(self.argv))",message="a manual criterion is checked by a human: no steps and no argv"
type AcceptanceCriterion struct {
	// ID names the criterion, unique within its plan.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	ID string `json:"id"`
	// Kind is how the criterion is checked.
	Kind AcceptanceKind `json:"kind"`
	// Statement is the approved one-line statement of what must hold.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	Statement string `json:"statement"`
	// Reason says why a manual criterion needs a human.
	// +optional
	// +kubebuilder:validation:MaxLength=120
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	Reason string `json:"reason,omitempty"`
	// Target is the previewed repository key an http criterion is sent to.
	// +optional
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Target string `json:"target,omitempty"`
	// Steps are an http criterion's requests, in order (the shorthand of a
	// bare request and expect is normalised into one step).
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Steps []AcceptanceStep `json:"steps,omitempty"`
	// In is where a command criterion runs.
	// +optional
	In *AcceptanceWhere `json:"in,omitempty"`
	// Argv is a command criterion's argument vector, run without a shell.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +kubebuilder:validation:items:MaxLength=200
	Argv []string `json:"argv,omitempty"`
	// Expect is what a command criterion's run must show.
	// +optional
	Expect *AcceptanceExpect `json:"expect,omitempty"`
	// Timeout bounds a command criterion's run (a Go duration, at most 10m).
	// +optional
	// +kubebuilder:validation:MaxLength=16
	Timeout string `json:"timeout,omitempty"`
}

// AcceptanceWhere is where a command criterion runs: exactly one of a
// previewed component's own image or the repository's agent image.
// +kubebuilder:validation:XValidation:rule="has(self.component) != has(self.repository)",message="in names exactly one of component and repository"
type AcceptanceWhere struct {
	// Component is a previewed repository key: the command runs in that
	// component's runtime image inside the slot.
	// +optional
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Component string `json:"component,omitempty"`
	// Repository is a Project repository key: the command runs in that
	// repository's accepted agent image, without a model.
	// +optional
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Repository string `json:"repository,omitempty"`
}

// AcceptanceStep is one request of an http criterion and what its response
// must show.
type AcceptanceStep struct {
	// Name labels the step for display.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Name string `json:"name,omitempty"`
	// Request is what the tester sends.
	Request AcceptanceRequest `json:"request"`
	// Expect is what the response must show.
	// +optional
	Expect AcceptanceExpect `json:"expect,omitempty"`
	// Capture names values of the response (RFC 6901 pointers into its JSON
	// body) that later steps reference by name (the capture placeholder).
	// +optional
	// +kubebuilder:validation:MaxProperties=4
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z][a-z0-9]{0,15}$') && size(self[k]) <= 256)",message="capture names are ^[a-z][a-z0-9]{0,15}$ and their pointers at most 256 characters"
	Capture map[string]string `json:"capture,omitempty"`
}

// AcceptanceRequest is one HTTP request, sent to the target component's
// Service with its path unchanged. It carries at most one of JSON and Body
// (acceptance.Validate; a raw JSON value is out of CEL's reach).
type AcceptanceRequest struct {
	// Method of the request.
	// +kubebuilder:validation:Enum=GET;HEAD;POST;PUT;PATCH;DELETE
	Method string `json:"method"`
	// Path, with its query, starting with exactly one slash.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^/([^/].*)?$`
	Path string `json:"path"`
	// Headers sent with the request.
	// +optional
	// +kubebuilder:validation:MaxProperties=8
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Za-z0-9-]{1,64}$') && size(self[k]) <= 256)",message="header names are ^[A-Za-z0-9-]{1,64}$ and their values at most 256 characters"
	Headers map[string]string `json:"headers,omitempty"`
	// JSON is a JSON request body.
	// +optional
	JSON *apiextensionsv1.JSON `json:"json,omitempty"`
	// Body is a literal request body.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Body string `json:"body,omitempty"`
}

// AcceptanceExpect is what a response (http) or a run (command) must show.
// Each JSON value is a literal compared exactly or one matcher object
// ($present, $absent, $contains, $approx with $tolerance, $order with $by).
type AcceptanceExpect struct {
	// Status is the expected HTTP status: one code or a list of codes.
	// +optional
	Status *apiextensionsv1.JSON `json:"status,omitempty"`
	// JSON maps RFC 6901 pointers into the response body to what each must
	// hold.
	// +optional
	// +kubebuilder:validation:MaxProperties=8
	JSON map[string]apiextensionsv1.JSON `json:"json,omitempty"`
	// Headers maps response header names to what each must hold.
	// +optional
	// +kubebuilder:validation:MaxProperties=4
	Headers map[string]apiextensionsv1.JSON `json:"headers,omitempty"`
	// BodyContains is a string the response body must contain.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	BodyContains string `json:"bodyContains,omitempty"`
	// Exit is a command's expected exit code.
	// +optional
	Exit *int32 `json:"exit,omitempty"`
	// StdoutContains is a string a command's stdout must contain.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	StdoutContains string `json:"stdoutContains,omitempty"`
	// StdoutJSON maps RFC 6901 pointers into a command's JSON stdout to what
	// each must hold.
	// +optional
	// +kubebuilder:validation:MaxProperties=8
	StdoutJSON map[string]apiextensionsv1.JSON `json:"stdoutJSON,omitempty"`
}

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
// +kubebuilder:validation:Enum=http;browser;exec
type CheckForm string

// Check forms.
const (
	CheckFormHTTP    CheckForm = "http"
	CheckFormBrowser CheckForm = "browser"
	CheckFormExec    CheckForm = "exec"
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
	// Run is the checkreview IntentRun that answered.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Run string `json:"run"`
}
