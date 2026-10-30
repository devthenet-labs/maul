// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PreviewVerificationName returns the deterministic name of one verification
// attempt of the Intent named `intent`: <intent>-v<round>-a<attempt>. Inside
// the name budget (an Intent name is at most 33 characters, a round at most
// three digits) it is at most 41 characters, a valid label value.
func PreviewVerificationName(intent string, round, attempt int32) string {
	return fmt.Sprintf("%s-v%d-a%d", intent, round, attempt)
}

// MaxVerificationAttempts bounds a PreviewVerification's attempt: an
// infrastructure failure (never the application's) is retried at most this
// many times in one round before the verification is reported unavailable.
const MaxVerificationAttempts = 3

// PreviewVerificationPhase is the local state of one verification attempt.
// It is an observation, never a verdict: intent-controller judges what the
// observations show (acceptance.Judge).
// +kubebuilder:validation:Enum=Pending;Running;Observed;EnvFailed;Infra;Stale
type PreviewVerificationPhase string

// PreviewVerification phases.
const (
	// VerificationPending: created, nothing rendered yet.
	VerificationPending PreviewVerificationPhase = "Pending"
	// VerificationRunning: the tester Job runs in the Preview's slot.
	VerificationRunning PreviewVerificationPhase = "Running"
	// VerificationObserved: the tester's observations were read and
	// verified; intent-controller judges them.
	VerificationObserved PreviewVerificationPhase = "Observed"
	// VerificationEnvFailed: the application's environment failed (a
	// component crashed, never became Ready, was evicted or its image could
	// not be pulled); the evidence is in status.environment and the failure
	// is charged to the intent.
	VerificationEnvFailed PreviewVerificationPhase = "EnvFailed"
	// VerificationInfra: maul's own machinery failed (the tester was not
	// scheduled, its image not pulled, its output invalid); retried as a
	// new attempt, never charged to the intent.
	VerificationInfra PreviewVerificationPhase = "Infra"
	// VerificationStale: the request no longer matches its Preview (moved
	// heads, another generation, other criteria); dropped.
	VerificationStale PreviewVerificationPhase = "Stale"
)

// PreviewVerificationReason is the fixed vocabulary of a
// PreviewVerification's status.reason: never free text, so nothing the slot
// produced reaches a reader through it.
// +kubebuilder:validation:Enum=ok;stale-preview;stale-criteria;rollout-unsettled;tester-unscheduled;tester-image-pull;quota-refused;deadline-no-summary;summary-mismatch;tester-output-invalid;component-crashloop;component-not-ready;component-evicted;component-image-pull;component-oom;preview-failed;service-restarted;component-unreachable;component-crashed;component-disrupted;service-no-cluster-ip;tester-version-mismatch;tools-unscheduled;tools-image-pull;tools-crashloop;tools-not-ready;ledger-mismatch;evidence-mismatch
type PreviewVerificationReason string

// PreviewVerification reasons.
const (
	VerificationReasonOK                  PreviewVerificationReason = "ok"
	VerificationReasonStalePreview        PreviewVerificationReason = "stale-preview"
	VerificationReasonStaleCriteria       PreviewVerificationReason = "stale-criteria"
	VerificationReasonRolloutUnsettled    PreviewVerificationReason = "rollout-unsettled"
	VerificationReasonTesterUnscheduled   PreviewVerificationReason = "tester-unscheduled"
	VerificationReasonTesterImagePull     PreviewVerificationReason = "tester-image-pull"
	VerificationReasonQuotaRefused        PreviewVerificationReason = "quota-refused"
	VerificationReasonDeadlineNoSummary   PreviewVerificationReason = "deadline-no-summary"
	VerificationReasonSummaryMismatch     PreviewVerificationReason = "summary-mismatch"
	VerificationReasonTesterOutputInvalid PreviewVerificationReason = "tester-output-invalid"
	VerificationReasonComponentCrashLoop  PreviewVerificationReason = "component-crashloop"
	VerificationReasonComponentNotReady   PreviewVerificationReason = "component-not-ready"
	VerificationReasonComponentEvicted    PreviewVerificationReason = "component-evicted"
	VerificationReasonComponentImagePull  PreviewVerificationReason = "component-image-pull"
	VerificationReasonComponentOOM        PreviewVerificationReason = "component-oom"
	VerificationReasonPreviewFailed       PreviewVerificationReason = "preview-failed"
	VerificationReasonServiceRestarted    PreviewVerificationReason = "service-restarted"
	// VerificationReasonComponentUnreachable: a component's Service
	// accepted no connection from the tester for its whole readiness window
	// while the component was Ready (its environment fact's
	// unreachableForMs, status.readiness). EnvFailed, the application's: the
	// window absorbs the slot policy being programmed.
	VerificationReasonComponentUnreachable PreviewVerificationReason = "component-unreachable"
	// VerificationReasonComponentCrashed: a component's process exited (OOM
	// included) during the tester's run, by the kubelet's timestamps (its
	// fact's crashedDuring), and came back. EnvFailed, the application's.
	VerificationReasonComponentCrashed PreviewVerificationReason = "component-crashed"
	// VerificationReasonComponentDisrupted: during the tester's run a Pod of
	// a component's current revision was taken away by the cluster (the
	// Eviction API, a drain, preemption, the taint manager, Pod GC, a
	// deletion, node loss, a graceful node shutdown or kubelet preemption,
	// or a replacement with no evidence), exited cleanly, or went not-Ready
	// with no evidence of the application's (its fact's disrupted). Infra,
	// retried and bounded: the application holds no API credential, and
	// the most it can do without crashing is make itself not-Ready.
	VerificationReasonComponentDisrupted PreviewVerificationReason = "component-disrupted"
	// VerificationReasonServiceNoClusterIP: a component's Service had no
	// ClusterIP to render the tester's target from (the tester never
	// resolves a name). Infra, settled before any tester starts.
	VerificationReasonServiceNoClusterIP PreviewVerificationReason = "service-no-cluster-ip"
	// VerificationReasonTesterVersionMismatch: the tester wrote another
	// MAUL-VERIFY wire version than preview-controller reads
	// (status.testerWireVersion beside acceptance.WireVersion): the tester
	// image and the controller disagree. Infra.
	VerificationReasonTesterVersionMismatch PreviewVerificationReason = "tester-version-mismatch"
	// VerificationReasonToolsUnscheduled: no node took the slot tool pod.
	// Infra.
	VerificationReasonToolsUnscheduled PreviewVerificationReason = "tools-unscheduled"
	// VerificationReasonToolsImagePull: one of the tool pod's three images
	// could not be pulled. Infra.
	VerificationReasonToolsImagePull PreviewVerificationReason = "tools-image-pull"
	// VerificationReasonToolsCrashLoop: one of the tool pod's containers (a
	// native sidecar, browser or sandbox, above all) kept exiting in error:
	// the kubelet backing it off, or restarted past a small bound. Its
	// environment fact names the container, its last termination and, as
	// the fact's redacted logTail, its termination message. Infra.
	VerificationReasonToolsCrashLoop PreviewVerificationReason = "tools-crashloop"
	// VerificationReasonToolsNotReady: the tool pod did not become Ready
	// within its readiness bound (a quota or LimitRange refusal leaves the
	// Job Running with no pod, so this comes from the bound, never from Job
	// status). Infra.
	VerificationReasonToolsNotReady PreviewVerificationReason = "tools-not-ready"
	// VerificationReasonLedgerMismatch: the tools container's ledger lines
	// did not match its summary (count and sha256) or its termination
	// message. Infra, never a verdict.
	VerificationReasonLedgerMismatch PreviewVerificationReason = "ledger-mismatch"
	// VerificationReasonEvidenceMismatch: an evidence image did not match
	// the sha256 its ledger entry recorded, or was not a PNG. Infra.
	VerificationReasonEvidenceMismatch PreviewVerificationReason = "evidence-mismatch"
)

// VerificationRung is how much of an evaluation a round runs: full (the
// saved checks replayed, then the evaluator) or replay (the saved checks
// only, no model: the one round after a dispute, SPEC Q17).
// +kubebuilder:validation:Enum=full;replay
type VerificationRung string

// Rungs.
const (
	RungFull   VerificationRung = "full"
	RungReplay VerificationRung = "replay"
)

// The evaluator's tool bounds. The schema markers repeat them as literals;
// keep them in lockstep.
const (
	// MinToolCalls and MaxToolCallsLimit bound maxToolCalls, on a Project
	// and on a PreviewVerification. The upper bound is what the tools
	// ledger has room for beside its replay, confirmation and relay entries
	// (internal/toolwire.MaxEntries).
	MinToolCalls      = 50
	MaxToolCallsLimit = 300
	// MaxLookAroundRoutes bounds the routes the look-around visits.
	MaxLookAroundRoutes = 8
	// MaxLedgerIndexEntries bounds a ledger's index on status: every entry
	// a ledger can hold (internal/toolwire.MaxEntries), so the index is
	// never truncated.
	MaxLedgerIndexEntries = 512
)

// ToolsSpec is what an outcome verification's slot tool pod offers the
// evaluator, beside http, logs and events, which it always offers.
type ToolsSpec struct {
	// Browser runs the browser sidecar.
	// +optional
	Browser bool `json:"browser,omitempty"`
	// Sandbox runs the sandbox sidecar (exec checks and agent-written
	// clients).
	// +optional
	Sandbox bool `json:"sandbox,omitempty"`
	// MaxToolCalls bounds the evaluator's tool calls; a call past it is
	// refused, and the refusal is a ledger entry.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=300
	MaxToolCalls int32 `json:"maxToolCalls"`
	// LookAroundRoutes bounds the routes the look-around visits; zero
	// skips it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	LookAroundRoutes int32 `json:"lookAroundRoutes,omitempty"`
}

// PreviewVerificationSpec is one request to run an Intent's automatable
// acceptance criteria (slice 1's tester), or to evaluate its approved
// outcomes (the evaluator), against its Preview at exactly the recorded
// heads: exactly one of criteria and outcomes, each with its digest; an
// outcome verification also names its rung and its tool pod's tools.
// intent-controller writes it once, at creation, and it is immutable: a new
// attempt or round is a new PreviewVerification. preview-controller re-checks
// every field against the Intent and the Preview before it renders anything.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new PreviewVerification for a new attempt"
// +kubebuilder:validation:XValidation:rule="has(self.criteria) != has(self.outcomes)",message="a verification runs exactly one of criteria and outcomes"
// +kubebuilder:validation:XValidation:rule="has(self.criteria) == has(self.criteriaDigest)",message="criteria and criteriaDigest are set together"
// +kubebuilder:validation:XValidation:rule="has(self.outcomes) == has(self.outcomesDigest)",message="outcomes and outcomesDigest are set together"
// +kubebuilder:validation:XValidation:rule="has(self.outcomes) == has(self.rung) && has(self.outcomes) == has(self.tools)",message="an outcome verification, and only one, names its rung and its tools"
// +kubebuilder:validation:XValidation:rule="!has(self.checks) || has(self.outcomes)",message="saved checks belong to an outcome verification"
// +kubebuilder:validation:XValidation:rule="has(self.checks) == has(self.checksDigest)",message="checks and checksDigest are set together"
// +kubebuilder:validation:XValidation:rule="!has(self.rung) || self.rung != 'replay' || has(self.checks)",message="a replay round replays saved checks: it needs at least one"
// +kubebuilder:validation:XValidation:rule="has(self.intentRef.uid) && size(self.intentRef.uid) > 0",message="spec.intentRef.uid is required"
// +kubebuilder:validation:XValidation:rule="has(self.previewRef.uid) && size(self.previewRef.uid) > 0",message="spec.previewRef.uid is required"
// +kubebuilder:validation:XValidation:rule="duration(self.timeout) >= duration('1m') && duration(self.timeout) <= duration('1h')",message="spec.timeout must be between 1m and 1h"
type PreviewVerificationSpec struct {
	// IntentRef is the Intent that owns the verification, pinned by UID.
	IntentRef ObjectReference `json:"intentRef"`
	// PreviewRef is the Intent's Preview, pinned by UID.
	PreviewRef ObjectReference `json:"previewRef"`
	// PreviewGeneration is the Preview generation the heads were read at.
	// +kubebuilder:validation:Minimum=1
	PreviewGeneration int64 `json:"previewGeneration"`
	// HeadSetDigest is the sha256 over the sorted (repository, revision)
	// pairs of every Preview component: the combined change verified.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	HeadSetDigest string `json:"headSetDigest"`
	// Heads are the Preview spec's components and their revisions.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +listType=map
	// +listMapKey=component
	Heads []VerificationHead `json:"heads"`
	// Round is the verification round, 1-based.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Round int32 `json:"round"`
	// Attempt within the round, 1-based; only an infrastructure failure
	// starts another (MaxVerificationAttempts).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Attempt int32 `json:"attempt"`
	// Run is the random token every run placeholder of this attempt carries:
	// v<round>a<attempt>t<try>-<run>.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{6}$`
	Run string `json:"run"`
	// Criteria are the automatable criteria of the Intent's approved
	// acceptance list, in plan order (slice 1's tester).
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Criteria []AcceptanceCriterion `json:"criteria,omitempty"`
	// CriteriaDigest is the digest of the Intent's full approved list
	// (status.acceptance.digest), of which Criteria is the automatable
	// subset. Set with Criteria, and only with it.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CriteriaDigest string `json:"criteriaDigest,omitempty"`
	// Outcomes are the Intent's approved outcomes (status.acceptance.outcomes),
	// in plan order, manual ones included: the evaluator judges them.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=id
	Outcomes []AcceptanceOutcome `json:"outcomes,omitempty"`
	// OutcomesDigest is the Intent's status.acceptance.outcomesDigest. Set
	// with Outcomes, and only with it.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	OutcomesDigest string `json:"outcomesDigest,omitempty"`
	// Checks are the Intent's saved checks, copied inline from its check
	// store (status.acceptance.checks), which preview-controller copies
	// into the slot tool pod's ConfigMap: it never reads the store, so it
	// needs no read of the release namespace's ConfigMaps (SPEC Q1). Empty
	// on a first round, before any check is saved.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=40
	// +listType=atomic
	Checks []SavedCheck `json:"checks,omitempty"`
	// ChecksDigest is the check store's digest
	// (status.acceptance.checks.digest). Set with Checks, and only with it.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ChecksDigest string `json:"checksDigest,omitempty"`
	// Rung is how much of an evaluation the round runs. Set on an outcome
	// verification, and only on one.
	// +optional
	Rung VerificationRung `json:"rung,omitempty"`
	// Tools is what the slot tool pod offers. Set on an outcome
	// verification, and only on one.
	// +optional
	Tools *ToolsSpec `json:"tools,omitempty"`
	// Timeout is the tester Job's, or the tool pod's, wall clock.
	Timeout metav1.Duration `json:"timeout"`
}

// TransportError classifies a request that got no usable response; none
// when it got one.
// +kubebuilder:validation:Enum=none;refused;timeout;reset;dns;tls;too-large;capture-missing
type TransportError string

// Transport error classes.
const (
	TransportNone           TransportError = "none"
	TransportRefused        TransportError = "refused"
	TransportTimeout        TransportError = "timeout"
	TransportReset          TransportError = "reset"
	TransportDNS            TransportError = "dns"
	TransportTLS            TransportError = "tls"
	TransportTooLarge       TransportError = "too-large"
	TransportCaptureMissing TransportError = "capture-missing"
)

// StepObservation is what one request of one try saw: raw, bounded values,
// never a verdict. Every value came from the application under test and is
// untrusted.
type StepObservation struct {
	// Method as sent.
	// +kubebuilder:validation:Enum=GET;HEAD;POST;PUT;PATCH;DELETE
	Method string `json:"method"`
	// Path as sent, after substitution.
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`
	// Status is the response's HTTP status; zero when there was none.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Status int32 `json:"status,omitempty"`
	// TransportError classifies a request that got no usable response.
	// +optional
	TransportError TransportError `json:"transportError,omitempty"`
	// JSONValid reports whether the body parsed as JSON.
	// +optional
	JSONValid bool `json:"jsonValid,omitempty"`
	// BodyBytes is the body's length as read (capped by the tester).
	// +optional
	// +kubebuilder:validation:Minimum=0
	BodyBytes int32 `json:"bodyBytes,omitempty"`
	// LatencyMs is the request's duration in milliseconds.
	// +optional
	// +kubebuilder:validation:Minimum=0
	LatencyMs int32 `json:"latencyMs,omitempty"`
	// Pointers maps each expected, $by and capture pointer to the raw JSON
	// found there; a pointer absent from the body is absent from the map.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 512 && size(self[k]) <= 256)",message="pointers are at most 512 characters and their values at most 256"
	Pointers map[string]string `json:"pointers,omitempty"`
	// Captures are the values this step captured for later steps.
	// +optional
	// +kubebuilder:validation:MaxProperties=4
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z][a-z0-9]{0,15}$') && size(self[k]) <= 128)",message="capture names are ^[a-z][a-z0-9]{0,15}$ and their values at most 128 characters"
	Captures map[string]string `json:"captures,omitempty"`
	// Headers are the response headers the step's expectation names.
	// +optional
	// +kubebuilder:validation:MaxProperties=4
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 64 && size(self[k]) <= 256)",message="header names are at most 64 characters and their values at most 256"
	Headers map[string]string `json:"headers,omitempty"`
	// BodyPrefix is the start of the body, base64, for display only; the
	// first value dropped when the object is too large.
	// +optional
	// +kubebuilder:validation:MaxLength=684
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9+/]*={0,2}$`
	BodyPrefix string `json:"bodyPrefix,omitempty"`
}

// TryObservation is one try of one criterion: a retry runs every step again
// under a fresh run placeholder.
type TryObservation struct {
	// Run is the run placeholder token of the try.
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^v[0-9]{1,3}a[0-9]t[0-9]-[0-9a-f]{6}$`
	Run string `json:"run"`
	// Steps observed, in order; fewer than declared when a step's request
	// could not be built or sent.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Steps []StepObservation `json:"steps,omitempty"`
}

// CriterionObservation is what the tester saw for one criterion: its tries,
// or, when the status could not hold them, only the TooLarge marker.
// +kubebuilder:validation:XValidation:rule="(has(self.tooLarge) && self.tooLarge) != has(self.tries)",message="an observation holds its tries or is marked tooLarge, never both or neither"
type CriterionObservation struct {
	// ID of the criterion.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	ID string `json:"id"`
	// Tries made: a second only when the first failed.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=atomic
	Tries []TryObservation `json:"tries,omitempty"`
	// TooLarge marks an observation preview-controller replaced because the
	// object could not hold it however its values were reduced. It is
	// judged a failure (observation-too-large), never a pass.
	// +optional
	TooLarge bool `json:"tooLarge,omitempty"`
}

// PreviewPartRole is the role of one part of a Preview's slot.
// +kubebuilder:validation:Enum=component;service;tester;command;seed;tools
type PreviewPartRole string

// Preview part roles.
const (
	PreviewRoleComponent PreviewPartRole = "component"
	PreviewRoleService   PreviewPartRole = "service"
	PreviewRoleTester    PreviewPartRole = "tester"
	PreviewRoleCommand   PreviewPartRole = "command"
	PreviewRoleSeed      PreviewPartRole = "seed"
	// PreviewRoleToolPod is the slot tool pod's part role: the same value
	// as its maul.run/preview-role label (PreviewRoleTools).
	PreviewRoleToolPod PreviewPartRole = PreviewRoleTools
)

// Termination is a container's last termination.
type Termination struct {
	// Reason the kubelet gave (OOMKilled, Error, Completed, ...).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]*$`
	Reason string `json:"reason,omitempty"`
	// ExitCode of the container.
	ExitCode int32 `json:"exitCode"`
}

// PartFact is the environment evidence about one part of the slot while
// the verification ran, captured from the kubelet's container facts.
type PartFact struct {
	// Name of the part (a component's key, a service's name, tester, ...).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Role of the part.
	Role PreviewPartRole `json:"role"`
	// Ready reports whether the part's pod was Ready.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// Restarts counts the part's container restarts.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Restarts int32 `json:"restarts,omitempty"`
	// Waiting is the container's waiting reason (CrashLoopBackOff,
	// ImagePullBackOff, ErrImagePull, CreateContainerConfigError, ...).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]*$`
	Waiting string `json:"waiting,omitempty"`
	// LastTermination is the container's last termination.
	// +optional
	LastTermination *Termination `json:"lastTermination,omitempty"`
	// Evicted reports that the part's pod was evicted.
	// +optional
	Evicted bool `json:"evicted,omitempty"`
	// Unschedulable reports that no node could take the part's pod.
	// +optional
	Unschedulable bool `json:"unschedulable,omitempty"`
	// CrashedDuring reports that the component's process exited (OOM
	// included) during the tester's run: the application's failure.
	// +optional
	CrashedDuring bool `json:"crashedDuring,omitempty"`
	// Disrupted reports that the cluster took the component's Pod away
	// during the tester's run: maul's, never the application's.
	// +optional
	Disrupted bool `json:"disrupted,omitempty"`
	// UnreachableForMs is how long the tester tried, and failed, to connect
	// to the component's Service before its first criterion (its whole
	// readiness window); zero when reached.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=120000
	UnreachableForMs int32 `json:"unreachableForMs,omitempty"`
	// LogTail is the end of the part's log since the verification started,
	// redacted (slice 2). Untrusted and opaque.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	LogTail string `json:"logTail,omitempty"`
}

// TargetWait is how the tester waited, before its first criterion, for one
// component's Service to accept a TCP connection: how long, in how many
// attempts, and how the failed ones failed. Every attempt but a last
// successful one failed, so the Service was reached exactly when attempts
// is one more than the failures.
type TargetWait struct {
	// Target is the component (its Project repository's key).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=16
	Target string `json:"target"`
	// WaitMs is how long the wait took, in milliseconds.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=120000
	WaitMs int32 `json:"waitMs"`
	// Attempts is how many connections were attempted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	Attempts int32 `json:"attempts"`
	// Refused counts attempts refused.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	Refused int32 `json:"refused,omitempty"`
	// Timeout counts attempts that timed out.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	Timeout int32 `json:"timeout,omitempty"`
	// Reset counts attempts that failed otherwise.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	Reset int32 `json:"reset,omitempty"`
	// DNS counts attempts whose Service name did not resolve.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	DNS int32 `json:"dns,omitempty"`
}

// PreviewVerificationStatus is written only by preview-controller: what the
// tester and the kubelet observed, never a verdict.
type PreviewVerificationStatus struct {
	// Phase of the attempt.
	// +optional
	Phase PreviewVerificationPhase `json:"phase,omitempty"`
	// Reason is the phase's reason, from a fixed vocabulary.
	// +optional
	Reason PreviewVerificationReason `json:"reason,omitempty"`
	// StartedAt is when the tester Job was created: the window environment
	// evidence is read from.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the attempt settled.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// TesterImageID is the digest-bearing image ID the tester ran.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	TesterImageID string `json:"testerImageID,omitempty"`
	// Observations are the tester's, one per criterion, in spec order.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Observations []CriterionObservation `json:"observations,omitempty"`
	// Environment is the evidence about each part of the slot.
	// +optional
	// +kubebuilder:validation:MaxItems=12
	// +listType=atomic
	Environment []PartFact `json:"environment,omitempty"`
	// SummarySHA256 is the hex sha256 the tester's summary line declared
	// over its observation lines, which preview-controller verified.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	SummarySHA256 string `json:"summarySHA256,omitempty"`
	// Readiness is the tester's wait for each component's Service before
	// its first criterion, in component-name order, as its summary
	// declared it. A component never reached ends the attempt with no
	// observations: EnvFailed component-unreachable (its fact's
	// unreachableForMs), the application's. The tester dials each
	// Service's ClusterIP and resolves no name.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Readiness []TargetWait `json:"readiness,omitempty"`
	// TesterWireVersion is the MAUL-VERIFY version the tester wrote, set
	// when it was not the one preview-controller reads
	// (tester-version-mismatch).
	// +optional
	// +kubebuilder:validation:Minimum=0
	TesterWireVersion int32 `json:"testerWireVersion,omitempty"`
	// Tools is the slot tool pod, once scheduled: where the evaluator
	// reaches it (a hostAlias, since the evaluator has no DNS).
	// +optional
	Tools *ToolsStatus `json:"tools,omitempty"`
	// Ledger summarises the tools container's verified ledger.
	// +optional
	Ledger *LedgerSummary `json:"ledger,omitempty"`
	// Replay is each saved check's replay at the start of the round, in
	// spec order.
	// +optional
	// +kubebuilder:validation:MaxItems=40
	// +listType=atomic
	Replay []CheckRun `json:"replay,omitempty"`
	// Confirmations is each proposed check's confirmation run, alone with
	// a fresh run token, after the evaluator finished.
	// +optional
	// +kubebuilder:validation:MaxItems=40
	// +listType=atomic
	Confirmations []CheckRun `json:"confirmations,omitempty"`
	// Confirmed are the proposed checks that passed their confirmation,
	// their bodies read from the verified ledger (never from the
	// evaluator's report): what intent-controller may store once reviewed.
	// +optional
	// +kubebuilder:validation:MaxItems=40
	// +listType=atomic
	Confirmed []SavedCheck `json:"confirmed,omitempty"`
	// Capture summarises the component log tails and slot events captured
	// for the evaluator.
	// +optional
	Capture *CaptureSummary `json:"capture,omitempty"`
}

// ToolsStatus is the slot tool pod as preview-controller observed it.
type ToolsStatus struct {
	// PodIP is the tool pod's address.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F:.]+$`
	PodIP string `json:"podIP"`
	// Port is the tools container's listener.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Ready reports whether the tools container was Ready.
	// +optional
	Ready bool `json:"ready,omitempty"`
}

// LedgerKind is who caused a ledger entry (internal/toolwire.Kind).
// +kubebuilder:validation:Enum=agent;runner;relay
type LedgerKind string

// Ledger kinds.
const (
	LedgerKindAgent  LedgerKind = "agent"
	LedgerKindRunner LedgerKind = "runner"
	LedgerKindRelay  LedgerKind = "relay"
)

// LedgerVerb is the tool a ledger entry records (internal/toolwire.Verb).
// +kubebuilder:validation:Enum=http;browser;exec;logs;events;check-run;check-results;confirm;finish;session
type LedgerVerb string

// Ledger verbs.
const (
	LedgerVerbHTTP         LedgerVerb = "http"
	LedgerVerbBrowser      LedgerVerb = "browser"
	LedgerVerbExec         LedgerVerb = "exec"
	LedgerVerbLogs         LedgerVerb = "logs"
	LedgerVerbEvents       LedgerVerb = "events"
	LedgerVerbCheckRun     LedgerVerb = "check-run"
	LedgerVerbCheckResults LedgerVerb = "check-results"
	LedgerVerbConfirm      LedgerVerb = "confirm"
	LedgerVerbFinish       LedgerVerb = "finish"
	LedgerVerbSession      LedgerVerb = "session"
)

// LedgerSummary is the tools container's ledger as preview-controller
// verified it: the count and sha256 its summary declared, where the full
// ledger and the evidence images are kept, and an index of every entry.
type LedgerSummary struct {
	// Count is the number of entries.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=512
	Count int32 `json:"count"`
	// SHA256 is the hex sha256 over the entry lines in order.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	SHA256 string `json:"sha256"`
	// ConfigMap names the full ledger: <pv>-ledger, owned by the
	// PreviewVerification, oldest outputs truncated with a marker past its
	// size bound.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap,omitempty"`
	// Evidence names the cited screenshots, re-encoded: <pv>-evidence,
	// owned by the PreviewVerification.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Evidence string `json:"evidence,omitempty"`
	// Entries index every entry, in order; never truncated.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	// +listType=atomic
	Entries []LedgerIndexEntry `json:"entries,omitempty"`
}

// LedgerIndexEntry is one ledger entry's facts, without its output.
type LedgerIndexEntry struct {
	// Seq is the entry's sequence number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	Seq int32 `json:"seq"`
	// Kind is who caused it.
	Kind LedgerKind `json:"kind"`
	// Verb is the tool it records.
	Verb LedgerVerb `json:"verb"`
	// Exit is the exit code of the process it ran; 0 when it ran none.
	// +optional
	Exit int32 `json:"exit,omitempty"`
	// ResultSHA256 is the hex sha256 of its full output.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	ResultSHA256 string `json:"resultSHA256"`
	// Artifacts are the hex sha256s of its artifacts (screenshots), in
	// order.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	// +kubebuilder:validation:items:Pattern=`^[0-9a-f]{64}$`
	Artifacts []string `json:"artifacts,omitempty"`
	// Refused reports that nothing ran: the call was refused.
	// +optional
	Refused bool `json:"refused,omitempty"`
}

// CheckRunResult is what one deterministic run of a saved or proposed
// check showed. error is the check's own failure to run (an indeterminate
// assertion included), refused a check the grammar refused.
// +kubebuilder:validation:Enum=pass;fail;quarantined;error;refused
type CheckRunResult string

// Check run results.
const (
	CheckRunPass        CheckRunResult = "pass"
	CheckRunFail        CheckRunResult = "fail"
	CheckRunQuarantined CheckRunResult = "quarantined"
	CheckRunError       CheckRunResult = "error"
	CheckRunRefused     CheckRunResult = "refused"
)

// CheckRun is one run of one check, as the ledger recorded it.
type CheckRun struct {
	// CheckDigest is the check's digest.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CheckDigest string `json:"checkDigest"`
	// Result of the run.
	Result CheckRunResult `json:"result"`
	// Token is the fresh run token the run substituted for the check's
	// run placeholder.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+$`
	Token string `json:"token"`
	// Seq is the ledger entry of the run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	Seq int32 `json:"seq"`
	// DryRunSeq is the ledger entry of the green dry run a confirmation
	// required; unset on a replay.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	DryRunSeq int32 `json:"dryRunSeq,omitempty"`
}

// CaptureSummary is what was captured from the slot for the evaluator: per
// component, how many log bytes (a tail since the verification started,
// redacted) and whether older ones were dropped; and how many slot events.
type CaptureSummary struct {
	// Components are the per-component log tails.
	// +optional
	// +kubebuilder:validation:MaxItems=12
	// +listType=atomic
	Components []ComponentCapture `json:"components,omitempty"`
	// Events counts the slot events and container states captured.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000
	Events int32 `json:"events,omitempty"`
}

// ComponentCapture is one component's captured log tail.
type ComponentCapture struct {
	// Name of the part.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Bytes held, at most 64 KiB.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65536
	Bytes int32 `json:"bytes"`
	// Truncated reports that older output was dropped to keep the newest.
	// +optional
	Truncated bool `json:"truncated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pver,categories=maul
// +kubebuilder:printcolumn:name="Intent",type=string,JSONPath=`.spec.intentRef.name`
// +kubebuilder:printcolumn:name="Round",type=integer,JSONPath=`.spec.round`
// +kubebuilder:printcolumn:name="Attempt",type=integer,JSONPath=`.spec.attempt`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PreviewVerification is one attempt to run an Intent's automatable
// acceptance criteria against its Preview, inside the Preview's slot. It
// lives in the release namespace, owned by its Intent (controller
// reference), named <intent>-v<round>-a<attempt>. intent-controller creates
// and deletes it and judges its observations; preview-controller runs the
// tester and writes its status. Its name is written into label values, so
// the schema refuses one over 63 characters.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="PreviewVerification names are at most 63 characters: they are written into label values"
type PreviewVerification struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PreviewVerificationSpec `json:"spec"`
	// +optional
	Status PreviewVerificationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreviewVerificationList contains a list of PreviewVerification.
type PreviewVerificationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewVerification `json:"items"`
}
