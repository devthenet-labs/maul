// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"
	"time"

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
// It is an observation, never a verdict: intent-controller decides what the
// tool pod's ledger shows (acceptance.Decide).
// +kubebuilder:validation:Enum=Pending;Running;Observed;EnvFailed;Infra;Stale
type PreviewVerificationPhase string

// PreviewVerification phases.
const (
	// VerificationPending: created, nothing rendered yet.
	VerificationPending PreviewVerificationPhase = "Pending"
	// VerificationRunning: the tool pod runs, in the Preview's slot or, for
	// an agents verification, in the agents namespace.
	VerificationRunning PreviewVerificationPhase = "Running"
	// VerificationObserved: the tool pod's ledger was read and verified;
	// intent-controller decides what it shows.
	VerificationObserved PreviewVerificationPhase = "Observed"
	// VerificationEnvFailed: the application's environment failed (a
	// component crashed, never became Ready, was evicted or its image could
	// not be pulled); the evidence is in status.environment and the failure
	// is charged to the intent.
	VerificationEnvFailed PreviewVerificationPhase = "EnvFailed"
	// VerificationInfra: maul's own machinery failed (the tool pod was not
	// scheduled, an image of it not pulled, its ledger not held); retried as
	// a new attempt, never charged to the intent.
	VerificationInfra PreviewVerificationPhase = "Infra"
	// VerificationStale: the request no longer matches its Preview or its
	// Intent (moved heads, another generation, another approved plan);
	// dropped.
	VerificationStale PreviewVerificationPhase = "Stale"
)

// PreviewVerificationReason is the fixed vocabulary of a
// PreviewVerification's status.reason: never free text, so nothing the slot
// produced reaches a reader through it.
//
// Two values are retired spellings that nothing writes any more, kept in
// the schema only because settled PreviewVerifications may still hold them
// and a status write must not fail on a value it leaves unchanged:
// stale-criteria (now stale-approval) and tester-output-invalid (now
// tools-output-invalid).
// +kubebuilder:validation:Enum=ok;stale-preview;stale-approval;stale-criteria;rollout-unsettled;quota-refused;deadline-no-summary;tools-output-invalid;tester-output-invalid;component-crashloop;component-not-ready;component-evicted;component-image-pull;component-oom;preview-failed;service-restarted;component-unreachable;component-crashed;component-disrupted;service-no-cluster-ip;tools-unscheduled;tools-image-pull;tools-crashloop;tools-not-ready;evaluator-not-run;ledger-mismatch;evidence-mismatch;tools-build-failed;sandbox-unenforced;sandbox-image-unavailable;tree-unavailable;tool-pods-saturated;tools-evicted;tools-deadline;tools-refused;replay-incomplete;kept-copy-failed
type PreviewVerificationReason string

// PreviewVerification reasons.
const (
	VerificationReasonOK           PreviewVerificationReason = "ok"
	VerificationReasonStalePreview PreviewVerificationReason = "stale-preview"
	// VerificationReasonStaleApproval: the request no longer matches its
	// Intent's approved plan (another plan revision or outcome list, its
	// Intent or Project no longer verified), or it evaluates no outcomes.
	VerificationReasonStaleApproval    PreviewVerificationReason = "stale-approval"
	VerificationReasonRolloutUnsettled PreviewVerificationReason = "rollout-unsettled"
	VerificationReasonQuotaRefused     PreviewVerificationReason = "quota-refused"
	// VerificationReasonDeadlineNoSummary: the tool pod ended without the
	// summary its ledger is held to. Infra.
	VerificationReasonDeadlineNoSummary PreviewVerificationReason = "deadline-no-summary"
	// VerificationReasonToolsOutputInvalid: what the tool pod left could
	// not be recorded as it is (a status the object cannot hold), and no
	// other cause is named. Infra.
	VerificationReasonToolsOutputInvalid PreviewVerificationReason = "tools-output-invalid"
	VerificationReasonComponentCrashLoop PreviewVerificationReason = "component-crashloop"
	VerificationReasonComponentNotReady  PreviewVerificationReason = "component-not-ready"
	VerificationReasonComponentEvicted   PreviewVerificationReason = "component-evicted"
	VerificationReasonComponentImagePull PreviewVerificationReason = "component-image-pull"
	VerificationReasonComponentOOM       PreviewVerificationReason = "component-oom"
	VerificationReasonPreviewFailed      PreviewVerificationReason = "preview-failed"
	VerificationReasonServiceRestarted   PreviewVerificationReason = "service-restarted"
	// VerificationReasonComponentUnreachable: a component's Service
	// accepted no connection from the tool pod for its whole readiness window
	// while the component was Ready (its environment fact's
	// unreachableForMs, status.readiness). EnvFailed, the application's: the
	// window absorbs the slot policy being programmed.
	VerificationReasonComponentUnreachable PreviewVerificationReason = "component-unreachable"
	// VerificationReasonComponentCrashed: a component's process exited (OOM
	// included) during the tool pod's run, by the kubelet's timestamps (its
	// fact's crashedDuring), and came back. EnvFailed, the application's.
	VerificationReasonComponentCrashed PreviewVerificationReason = "component-crashed"
	// VerificationReasonComponentDisrupted: during the tool pod's run a Pod of
	// a component's current revision was taken away by the cluster (the
	// Eviction API, a drain, preemption, the taint manager, Pod GC, a
	// deletion, node loss, a graceful node shutdown or kubelet preemption,
	// or a replacement with no evidence), exited cleanly, or went not-Ready
	// with no evidence of the application's (its fact's disrupted). Infra,
	// retried and bounded: the application holds no API credential, and
	// the most it can do without crashing is make itself not-Ready.
	VerificationReasonComponentDisrupted PreviewVerificationReason = "component-disrupted"
	// VerificationReasonServiceNoClusterIP: a component's Service had no
	// ClusterIP to render the tool pod's target from (it never resolves a
	// name). Infra, settled before the tool pod starts.
	VerificationReasonServiceNoClusterIP PreviewVerificationReason = "service-no-cluster-ip"
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
	// VerificationReasonEvaluatorNotRun: a full round's tool pod was
	// published Ready but finished (at its wall clock, as a rule) with no
	// call from the evaluator in its ledger, neither an agent entry nor
	// agent-runner's own: no evaluator ran against it. Infra.
	VerificationReasonEvaluatorNotRun PreviewVerificationReason = "evaluator-not-run"
	// VerificationReasonLedgerMismatch: the tools container's ledger lines
	// did not match its summary (count and sha256) or its termination
	// message. Infra, never a verdict.
	VerificationReasonLedgerMismatch PreviewVerificationReason = "ledger-mismatch"
	// VerificationReasonEvidenceMismatch: an evidence image did not match
	// the sha256 its ledger entry recorded, or was not a PNG. Infra.
	VerificationReasonEvidenceMismatch PreviewVerificationReason = "evidence-mismatch"
	// VerificationReasonToolsBuildFailed: the agents tool pod could not
	// build the head tree (its build part's fact holds the exit). Infra
	// ("couldn't confirm"), never a fail: CI built the same head green.
	VerificationReasonToolsBuildFailed PreviewVerificationReason = "tools-build-failed"
	// VerificationReasonSandboxUnenforced: the agents tool pod's prepare
	// found the sandbox's network open (the strict probe's exit 78). Infra;
	// it trips the sandbox breaker.
	VerificationReasonSandboxUnenforced PreviewVerificationReason = "sandbox-unenforced"
	// VerificationReasonSandboxImageUnavailable: the workspace's repository
	// has no accepted runner image to run the sandboxes in (O8). Infra,
	// settled before any pod is created.
	VerificationReasonSandboxImageUnavailable PreviewVerificationReason = "sandbox-image-unavailable"
	// VerificationReasonTreeUnavailable: a commit-pinned Repository of the
	// head or base tree did not become Ready. Infra.
	VerificationReasonTreeUnavailable PreviewVerificationReason = "tree-unavailable"
	// VerificationReasonToolPodsSaturated: the agents tool pod bound
	// (--intent-max-tool-pods) was reached; the round waits. Never a
	// verdict.
	VerificationReasonToolPodsSaturated PreviewVerificationReason = "tool-pods-saturated"
	// VerificationReasonToolsEvicted: the cluster took the agents tool pod
	// away (an eviction, preemption or node loss). Infra.
	VerificationReasonToolsEvicted PreviewVerificationReason = "tools-evicted"
	// VerificationReasonToolsDeadline: the agents tool pod reached its Job
	// deadline with no verified summary. Infra.
	VerificationReasonToolsDeadline PreviewVerificationReason = "tools-deadline"
	// VerificationReasonToolsRefused: the API server refused to create the
	// agents tool pod's Job (an admission policy or webhook, RBAC, or the
	// object invalid; a quota's refusal is quota-refused): no pod ever
	// existed. Its message is in intent-controller's
	// log. Infra, never a verdict.
	VerificationReasonToolsRefused PreviewVerificationReason = "tools-refused"
	// VerificationReasonReplayIncomplete: a replay round's tool pod finished
	// before the end of its replay (its wall clock, or its container told to
	// stop), so a saved check may never have run at these heads (review
	// VC-3). Infra, never a verdict.
	VerificationReasonReplayIncomplete PreviewVerificationReason = "replay-incomplete"
	// VerificationReasonKeptCopyFailed: an agents tool pod's tree built,
	// but its sandbox could not keep the tree, build output and HOME every
	// check run starts from (together past the copy's bound, or a full work
	// volume), so no check could run there. Infra, never a verdict.
	VerificationReasonKeptCopyFailed PreviewVerificationReason = "kept-copy-failed"
)

// VerificationPlacement is where an outcome verification's tool pod runs:
// slot (preview-controller, in the Preview's slot; S1) or agents
// (intent-controller's agents host, in the agents namespace, against the
// head and base trees of one repository; S2). Unset is slot, so a slot
// verification stores exactly what it stored before placements existed.
// The single writer of status follows it: preview-controller for slot,
// intent-controller's agents host for agents.
// +kubebuilder:validation:Enum=slot;agents
type VerificationPlacement string

// Placements.
const (
	PlacementSlot   VerificationPlacement = "slot"
	PlacementAgents VerificationPlacement = "agents"
)

// TreeSide names one of the two trees an agents verification builds and
// runs: the PR head or its merge base.
// +kubebuilder:validation:Enum=head;base
type TreeSide string

// Tree sides.
const (
	TreeHead TreeSide = "head"
	TreeBase TreeSide = "base"
)

// The agents verification's bounds. The schema markers repeat them as
// literals; keep them in lockstep.
const (
	// MaxSlotVerificationTimeout bounds a slot verification's timeout.
	MaxSlotVerificationTimeout = time.Hour
	// MaxAgentsVerificationTimeout bounds an agents verification's
	// timeout: the evaluate timeout, the build timeout and a margin (O9).
	MaxAgentsVerificationTimeout = 75 * time.Minute
	// MaxBaseRuns bounds status.base.runs: a base run of every replayed and
	// every confirmed check.
	MaxBaseRuns = 80
	// MaxLedgerBuilds bounds ledger.builds: one build of each tree, and one
	// retry of each.
	MaxLedgerBuilds = 4
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

// PreviewVerificationSpec is one request to evaluate an Intent's approved
// outcomes (the agent evaluator) against its Preview, or in the agents
// namespace's workspace, at exactly the recorded heads: the outcomes with
// their digest, the rung and the tool pod's tools. Slice 1's criteria are
// removed: a request without outcomes is refused (a stored one, pruned of
// its criteria, is answered Stale stale-approval and never run).
// intent-controller writes it once, at creation, and it is immutable: a new
// attempt or round is a new PreviewVerification. preview-controller re-checks
// every field against the Intent and the Preview before it renders anything.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new PreviewVerification for a new attempt"
// +kubebuilder:validation:XValidation:rule="has(self.outcomes)",message="a verification evaluates outcomes: slice 1's criteria are removed"
// +kubebuilder:validation:XValidation:rule="has(self.outcomes) == has(self.outcomesDigest)",message="outcomes and outcomesDigest are set together"
// +kubebuilder:validation:XValidation:rule="has(self.outcomes) == has(self.rung) && has(self.outcomes) == has(self.tools)",message="an outcome verification, and only one, names its rung and its tools"
// +kubebuilder:validation:XValidation:rule="!has(self.checks) || has(self.outcomes)",message="saved checks belong to an outcome verification"
// +kubebuilder:validation:XValidation:rule="has(self.checks) == has(self.checksDigest)",message="checks and checksDigest are set together"
// +kubebuilder:validation:XValidation:rule="!has(self.rung) || self.rung != 'replay' || has(self.checks)",message="a replay round replays saved checks: it needs at least one"
// +kubebuilder:validation:XValidation:rule="has(self.intentRef.uid) && size(self.intentRef.uid) > 0",message="spec.intentRef.uid is required"
// +kubebuilder:validation:XValidation:rule="(has(self.placement) && self.placement == 'agents') || (has(self.previewRef) && has(self.previewRef.uid) && size(self.previewRef.uid) > 0)",message="spec.previewRef.uid is required"
// +kubebuilder:validation:XValidation:rule="(has(self.placement) && self.placement == 'agents') || (has(self.previewGeneration) && self.previewGeneration >= 1)",message="spec.previewGeneration must be at least 1 on a slot verification"
// +kubebuilder:validation:XValidation:rule="duration(self.timeout) >= duration('1m') && duration(self.timeout) <= ((has(self.placement) && self.placement == 'agents') ? duration('75m') : duration('1h'))",message="spec.timeout must be between 1m and 1h (75m on an agents verification)"
// +kubebuilder:validation:XValidation:rule="has(self.workspace) == (has(self.placement) && self.placement == 'agents')",message="an agents verification, and only one, names its workspace"
// +kubebuilder:validation:XValidation:rule="!has(self.placement) || self.placement != 'agents' || (!has(self.previewRef) && !has(self.previewGeneration))",message="an agents verification has no Preview: previewRef and previewGeneration are unset"
// +kubebuilder:validation:XValidation:rule="!has(self.workspace) || self.heads.exists(h, h.component == self.workspace.repository && h.revision == self.workspace.headSHA)",message="an agents verification's workspace builds one of its heads: spec.heads names spec.workspace.repository at spec.workspace.headSHA"
// +kubebuilder:validation:XValidation:rule="!has(self.placement) || self.placement != 'agents' || (has(self.outcomes) && has(self.tools) && !(has(self.tools.browser) && self.tools.browser) && (!has(self.tools.lookAroundRoutes) || self.tools.lookAroundRoutes == 0))",message="an agents verification evaluates outcomes with no browser and no look-around"
type PreviewVerificationSpec struct {
	// Placement is where the tool pod runs (VerificationPlacement); unset
	// is slot. Immutable with the spec.
	// +optional
	Placement VerificationPlacement `json:"placement,omitempty"`
	// IntentRef is the Intent that owns the verification, pinned by UID.
	IntentRef ObjectReference `json:"intentRef"`
	// PreviewRef is the Intent's Preview, pinned by UID. Required on a slot
	// verification, unset on an agents one.
	// +optional
	PreviewRef ObjectReference `json:"previewRef,omitzero"`
	// PreviewGeneration is the Preview generation the heads were read at:
	// at least 1 on a slot verification, unset on an agents one.
	// +optional
	PreviewGeneration int64 `json:"previewGeneration,omitempty"`
	// Workspace is what an agents verification builds and runs: one
	// repository's head and base trees in its accepted runner image. Set
	// on an agents verification, and only on one.
	// +optional
	Workspace *VerificationWorkspace `json:"workspace,omitempty"`
	// HeadSetDigest is the sha256 over the sorted (repository, revision)
	// pairs of every Preview component: the combined change verified.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	HeadSetDigest string `json:"headSetDigest"`
	// Heads are the Preview spec's components and their revisions; on an
	// agents verification, the PR heads, the workspace's repository among
	// them at its headSHA.
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
	// Timeout is the tool pod's wall clock.
	Timeout metav1.Duration `json:"timeout"`
}

// IsAgents reports whether the verification is placed in the agents
// namespace; unset placement is slot.
func (s *PreviewVerificationSpec) IsAgents() bool { return s.Placement == PlacementAgents }

// VerificationWorkspace is an agents verification's workspace: the head and
// base of one Project repository, each a commit-pinned Repository the tool
// pod fetches, built and run in the repository's accepted runner image under
// the build's network profile. intent-controller writes it once, with the
// spec.
type VerificationWorkspace struct {
	// Repository is the Project repository's key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Repository string `json:"repository"`
	// HeadSHA is the PR head commit.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	HeadSHA string `json:"headSHA"`
	// BaseSHA is the merge base of the head and the PR's base branch.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	BaseSHA string `json:"baseSHA"`
	// HeadRepositoryRef is the Repository pinned to HeadSHA, by UID.
	// +kubebuilder:validation:XValidation:rule="has(self.uid) && size(self.uid) > 0",message="the head repository's uid is required"
	HeadRepositoryRef ObjectReference `json:"headRepositoryRef"`
	// BaseRepositoryRef is the Repository pinned to BaseSHA, by UID.
	// +kubebuilder:validation:XValidation:rule="has(self.uid) && size(self.uid) > 0",message="the base repository's uid is required"
	BaseRepositoryRef ObjectReference `json:"baseRepositoryRef"`
	// SandboxImage is the repository's accepted runner image, by digest:
	// both sandboxes run it.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[a-z0-9][^@\s]*@sha256:[0-9a-f]{64}$`
	SandboxImage string `json:"sandboxImage"`
	// SandboxSearchPath is that image's sanitized PATH (its RunnerImage's
	// searchPath): absolute entries only.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:Pattern=`^/[^:\x00-\x1f]*(:/[^:\x00-\x1f]*)*$`
	SandboxSearchPath string `json:"sandboxSearchPath"`
	// NetworkProfile is the operator network profile the repository's
	// build gets; unset means none. The tool pod's network is never wider.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`
	NetworkProfile string `json:"networkProfile,omitempty"`
	// Surface is what the tool pod builds: a cli or a library.
	Surface WorkspaceSurface `json:"surface"`
}

// WorkspaceSurface is exactly one of cli and library.
// +kubebuilder:validation:XValidation:rule="has(self.cli) != has(self.library)",message="a workspace surface is exactly one of cli and library"
type WorkspaceSurface struct {
	// CLI builds a binary from the tree.
	// +optional
	CLI *WorkspaceCLI `json:"cli,omitempty"`
	// Library runs consumer programs against the tree.
	// +optional
	Library *WorkspaceLibrary `json:"library,omitempty"`
}

// WorkspaceCLI is a cli surface as resolved for one verification: the
// Project's build and binary, and its build timeout with the default
// applied.
type WorkspaceCLI struct {
	// Build is the build argv (CLISurface.Build).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1f\x7f]+$`
	// +kubebuilder:validation:XValidation:rule="self.exists(a, a.contains('/work/bin/') || a.endsWith('/work/bin'))",message="the build must write under /work/bin"
	Build []string `json:"build"`
	// Binary is the built binary (CLISurface.Binary).
	// +kubebuilder:validation:MaxLength=139
	// +kubebuilder:validation:Pattern=`^/work/bin(/[A-Za-z0-9_][A-Za-z0-9._-]{0,63}){1,2}$`
	Binary string `json:"binary"`
	// BuildTimeout bounds each tree's build, between 1m and 20m.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('20m')",message="buildTimeout must be between 1m and 20m"
	BuildTimeout metav1.Duration `json:"buildTimeout"`
}

// WorkspaceLibrary is a library surface as resolved for one verification.
type WorkspaceLibrary struct {
	// Language of the library.
	Language LibraryLanguage `json:"language"`
}

// PreviewPartRole is the role of one part of a Preview's slot. tester is
// a retired spelling (slice 1's tester, which nothing renders any more),
// kept in the schema only because a settled PreviewVerification may still
// hold it.
// +kubebuilder:validation:Enum=component;service;tester;command;seed;tools;build
type PreviewPartRole string

// Preview part roles.
const (
	PreviewRoleComponent PreviewPartRole = "component"
	PreviewRoleService   PreviewPartRole = "service"
	PreviewRoleCommand   PreviewPartRole = "command"
	PreviewRoleSeed      PreviewPartRole = "seed"
	// PreviewRoleToolPod is the slot tool pod's part role: the same value
	// as its maul.run/preview-role label (PreviewRoleTools).
	PreviewRoleToolPod PreviewPartRole = PreviewRoleTools
	// PreviewRoleBuild is a build of one tree in an agents tool pod
	// (build-head, build-base).
	PreviewRoleBuild PreviewPartRole = "build"
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
	// Name of the part (a component's key, a service's name, tools, ...).
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
	// included) during the tool pod's run: the application's failure.
	// +optional
	CrashedDuring bool `json:"crashedDuring,omitempty"`
	// Disrupted reports that the cluster took the component's Pod away
	// during the tool pod's run: maul's, never the application's.
	// +optional
	Disrupted bool `json:"disrupted,omitempty"`
	// UnreachableForMs is how long the tool pod tried, and failed, to
	// connect to the component's Service before its first tool call (its
	// whole readiness window); zero when reached.
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

// TargetWait is how the tool pod waited, before its first tool call, for one
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

// PreviewVerificationStatus is written only by preview-controller (and, for
// an agents verification, by intent-controller's agents host): what the
// tool pod and the kubelet observed, never a verdict.
type PreviewVerificationStatus struct {
	// Phase of the attempt.
	// +optional
	Phase PreviewVerificationPhase `json:"phase,omitempty"`
	// Reason is the phase's reason, from a fixed vocabulary.
	// +optional
	Reason PreviewVerificationReason `json:"reason,omitempty"`
	// StartedAt is when the tool pod's Job was created: the window environment
	// evidence is read from.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the attempt settled.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// ToolsImageID is the digest-bearing image ID the tool pod's tools
	// container ran.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	ToolsImageID string `json:"toolsImageID,omitempty"`
	// Environment is the evidence about each part of the slot.
	// +optional
	// +kubebuilder:validation:MaxItems=12
	// +listType=atomic
	Environment []PartFact `json:"environment,omitempty"`
	// Readiness is the tool pod's wait for each component's Service before
	// its first tool call, in component-name order, as its verified ledger
	// records it. A component never reached ends the attempt:
	// EnvFailed component-unreachable (its fact's unreachableForMs), the
	// application's. The tool pod dials each Service's ClusterIP and
	// resolves no name.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Readiness []TargetWait `json:"readiness,omitempty"`
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
	// Base is an agents verification's base side: whether its outcomes need
	// a base comparison, whether the base tree built, and each check's run
	// at base. Unset on a slot verification.
	// +optional
	Base *BaseStatus `json:"base,omitempty"`
}

// BaseStatus is an agents verification's base side (DESIGN 2.5, 7.10).
type BaseStatus struct {
	// Required reports that a change outcome's check must fail by assertion
	// at base.
	// +optional
	Required bool `json:"required,omitempty"`
	// Unbuildable reports that the base tree did not build: every base run
	// is n/a and a pass rests on the other rules, labelled "no base
	// comparison".
	// +optional
	Unbuildable bool `json:"unbuildable,omitempty"`
	// Runs are the checks' runs at base, each with a fresh run token
	// (CheckRun.tree base). Each names the base tree explicitly: an unset
	// tree reads as head (CheckRun.Side), so a base run without it would
	// stand for a head run.
	// +optional
	// +kubebuilder:validation:MaxItems=80
	// +listType=atomic
	// +kubebuilder:validation:XValidation:rule="self.all(r, has(r.tree) && r.tree == 'base')",message="every base run names the base tree"
	Runs []CheckRun `json:"runs,omitempty"`
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
// +kubebuilder:validation:Enum=http;browser;exec;logs;events;check-run;check-results;confirm;finish;session;build;files;run
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
	// LedgerVerbBuild records an agents tool pod's build of one tree.
	LedgerVerbBuild LedgerVerb = "build"
	// LedgerVerbFiles records a read of a pristine tree's files.
	LedgerVerbFiles LedgerVerb = "files"
	// LedgerVerbRun records an agent-written program compiled and run
	// against one tree.
	LedgerVerbRun LedgerVerb = "run"
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
	// Builds are an agents tool pod's tree builds, in ledger order.
	// +optional
	// +kubebuilder:validation:MaxItems=4
	// +listType=atomic
	Builds []LedgerBuild `json:"builds,omitempty"`
}

// BuildResult is how one tree's build ended.
// +kubebuilder:validation:Enum=built;failed;timeout;error
type BuildResult string

// Build results.
const (
	BuildBuilt   BuildResult = "built"
	BuildFailed  BuildResult = "failed"
	BuildTimeout BuildResult = "timeout"
	// BuildError is maul's own failure to run the build (the exec shim,
	// the tree), never the build's.
	BuildError BuildResult = "error"
)

// LedgerBuild is one tree's build in an agents tool pod, as its ledger
// entry recorded it.
type LedgerBuild struct {
	// Side is the tree built.
	Side TreeSide `json:"side"`
	// Result of the build.
	Result BuildResult `json:"result"`
	// Seq is the build's ledger entry.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	Seq int32 `json:"seq"`
	// ExitCode of the build's process; 0 when it built or never ran.
	// +optional
	ExitCode int32 `json:"exitCode,omitempty"`
	// DurationMs is how long the build ran, in milliseconds.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600000
	DurationMs int32 `json:"durationMs,omitempty"`
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

// CheckRunCause is why an error run errored, recorded only when the error
// is a property of the tree it ran against rather than a run cut short or
// disturbed (SPEC-S2 S2-20): only these base errors leave a check with no
// base comparison. A timeout, processes left behind, output cut, a bind or
// connection error or a sandbox that refused or did not answer carries
// none (toolwire.StepCause, httpstep.RunCause).
// +kubebuilder:validation:Enum=compile;vendored-tree;replace-outside-tree;no-module;base-unbuildable
type CheckRunCause string

// Check run causes.
const (
	// CheckRunCauseCompile: the run program did not compile against the
	// tree (the compiler ran to its end and refused it).
	CheckRunCauseCompile CheckRunCause = "compile"
	// CheckRunCauseVendoredTree, CheckRunCauseReplaceOutsideTree and
	// CheckRunCauseNoModule: a tree a run program cannot be built against.
	CheckRunCauseVendoredTree       CheckRunCause = "vendored-tree"
	CheckRunCauseReplaceOutsideTree CheckRunCause = "replace-outside-tree"
	CheckRunCauseNoModule           CheckRunCause = "no-module"
	// CheckRunCauseBaseUnbuildable: the base did not build.
	CheckRunCauseBaseUnbuildable CheckRunCause = "base-unbuildable"
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
	// DryRunSeq is the ledger entry of the dry run a confirmation required:
	// green, or, for a check expected to fail (Expect), red by assertion;
	// unset on a replay.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	DryRunSeq int32 `json:"dryRunSeq,omitempty"`
	// Expect is fail for a confirmation of a check the evaluator proposed
	// to show a defect.
	// +optional
	Expect CheckExpectation `json:"expect,omitempty"`
	// AssertionStep is, for a failed run red by assertion, the step (1-based)
	// at which both attempts failed, an assertion of a check that otherwise
	// ran (toolwire.CheckRunRecord.AssertionStep): never a check error, a
	// timeout or a locator that resolved to nothing. Unset otherwise.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	AssertionStep int32 `json:"assertionStep,omitempty"`
	// AssertionID names which assertion of that step failed
	// (toolwire.StepRecord.AssertionID): "json <pointer>", "header <name>",
	// "bodyContains", "marker <id>" or "expect". Set with AssertionStep.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	AssertionID string `json:"assertionID,omitempty"`
	// Tree is the tree the run ran against in an agents verification;
	// unset is head (Side), and a slot verification never sets it.
	// +optional
	Tree TreeSide `json:"tree,omitempty"`
	// Cause is, for an error run at the base of an agents verification,
	// why it errored, when the error is the tree's (CheckRunCause); unset
	// for every other run, and on every slot run.
	// +optional
	Cause CheckRunCause `json:"cause,omitempty"`
}

// RedByAssertion reports a run red by assertion (AssertionStep).
func (r CheckRun) RedByAssertion() bool { return r.Result == CheckRunFail && r.AssertionStep > 0 }

// Side is the tree the run ran against: head when unset.
func (r CheckRun) Side() TreeSide {
	if r.Tree == "" {
		return TreeHead
	}
	return r.Tree
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

// PreviewVerification is one attempt to evaluate an Intent's approved
// outcomes, against its Preview inside the Preview's slot or, for a Project
// with no preview, in the agents namespace. It lives in the release
// namespace, owned by its Intent (controller reference), named
// <intent>-v<round>-a<attempt>. intent-controller creates and deletes it
// and decides what its ledger shows; preview-controller (or, for an agents
// verification, intent-controller's agents host) runs the tool pod and
// writes its status. Its name is written into label values, so
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
