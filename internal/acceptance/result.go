// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

// The types Judge decides in: an approved outcome as it sees it, the
// evaluator's claims, an outcome's result and the reasons Judge shares with
// the controller, the fix request and the hand-off.

// OutcomeRef is one approved outcome as Judge sees it, in plan order.
type OutcomeRef struct {
	ID string
	// Digest is the digest of the outcome's text, which a failure is
	// fingerprinted on (OutcomeFingerprint).
	Digest string
	// Keep marks a `keep` outcome; otherwise it is a `change` outcome.
	Keep bool
	// Manual marks an outcome closed only by a human (`/maul accept`).
	Manual bool
}

// Claim is what the evaluator said about an outcome.
type Claim string

// Claims.
const (
	ClaimPass   Claim = "pass"
	ClaimFail   Claim = "fail"
	ClaimUnsure Claim = "unsure"
)

// Result is an outcome's result this round. A human's acceptance is the
// controller's, layered on top.
type Result string

// Results.
const (
	ResultPassed Result = "passed"
	ResultFailed Result = "failed"
	ResultUnsure Result = "unsure"
)

// Reasons Judge shares with the controller, beside its own (judge.go). An
// earlier release's records may carry other reasons, which are free
// strings: the comments word the ones they know and add nothing for any
// other.
const (
	// ReasonManual: a human closes this outcome.
	ReasonManual = "manual"
	// ReasonEnvFailed, ReasonInfra: the environment was failing, the
	// change's (EnvAppFailure) or maul's own (EnvInfra).
	ReasonEnvFailed = "env-failed"
	ReasonInfra     = "infra"
	// ReasonHeldFinding: an earlier round at the same heads saw the outcome
	// not hold (G-B-LIB-1): a finding is a fact about the heads, so a later
	// round there leaves the outcome unsure, never passed, until the heads
	// move, an approver accepts it, or a fix round handed the failure
	// concludes no defect. Judge's input (Held); the intent controller,
	// which keeps the rounds' records, decides what is held.
	ReasonHeldFinding = "held-finding"
	// ReasonBuildFailed: the tool pod could not build the head (slice S2):
	// maul's Infra, so the round has no fail and no pass whatever its calls
	// show (the PreviewVerification reason of the same name).
	ReasonBuildFailed = "tools-build-failed"
)

// FixFailure is one failing outcome a verify-fix round is handed.
type FixFailure struct {
	Outcome string
	// Seq is the outcome's lowest valid citation.
	Seq int32
}

// FixRequest asks for a verify-fix round.
type FixRequest struct {
	Failures []FixFailure
	// Fingerprint is OutcomeFingerprint of the failing outcomes.
	Fingerprint string
}

// Handoff is the hand-off this round earns.
type Handoff string

// Hand-offs.
const (
	// HandoffReady: every outcome passed in a clean environment. A
	// look-around anomaly is a note for the reviewer, never Judge's input.
	HandoffReady Handoff = "ready"
	// HandoffUnconfirmed: anything else.
	HandoffUnconfirmed Handoff = "unconfirmed"
)
