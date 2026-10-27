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
