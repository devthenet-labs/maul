// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IntentName returns the name of the Intent for issue `issue` of the Project
// named `project`: <project>-<issue>. The schema holds every Intent to it, and
// inside the name budget it is at most 33 characters.
func IntentName(project string, issue int64) string {
	return project + "-" + strconv.FormatInt(issue, 10)
}

// IntentBranchPrefix prefixes every branch intent-controller creates in an
// app repository (maul-intent/<intent>). It deliberately never matches the
// Finding flow's "maul/" branch check, so the Finding pull-request handler
// can never act on an intent pull request.
const IntentBranchPrefix = "maul-intent/"

// MaxIntentPhaseTimes bounds status.phaseTimes. Replans and review rounds
// are human-driven and so unbounded in number; SetIntentPhase keeps the most
// recent entries so the log can never outgrow the schema's MaxItems.
const MaxIntentPhaseTimes = 64

// MaxIntentRefusedActors bounds status.commands.refusedActors, which keeps
// the latest accounts sent a refusal on the intent issue. The schema marker
// repeats it as a literal; keep the two in lockstep.
const MaxIntentRefusedActors = 32

// MaxIntentAppliedCommands bounds status.commands.applied, which keeps the
// latest acceptance commands the Intent applied. The schema marker repeats
// it as a literal; keep the two in lockstep.
const MaxIntentAppliedCommands = 64

// IntentPhase is the lifecycle of one intent. It is a local enum, not part of
// the Finding phase taxonomy in transitions.go, and its edge table
// (intentTransitions below) is separate from Finding's: intent-controller is
// the single writer of every edge.
// +kubebuilder:validation:Enum=Pending;Planning;AwaitingApproval;Building;InReview;Revising;Blocked;Merged;Closed;Failed
type IntentPhase string

// Intent phases.
const (
	// IntentPending: discovered, the trigger's authority not yet decided.
	IntentPending IntentPhase = "Pending"
	// IntentPlanning: a plan run is pending, running, or being written back.
	IntentPlanning IntentPhase = "Planning"
	// IntentAwaitingApproval: the plan is posted on the issue; waiting for an
	// approver's approval (or a replan request).
	IntentAwaitingApproval IntentPhase = "AwaitingApproval"
	// IntentBuilding: the approved plan is being built into a changeset,
	// pushed, and opened as pull requests.
	IntentBuilding IntentPhase = "Building"
	// IntentInReview: every pull request is open; waiting on reviews, checks
	// and merge.
	IntentInReview IntentPhase = "InReview"
	// IntentRevising: a revise or check-fix round is running against the
	// pull request head.
	IntentRevising IntentPhase = "Revising"
	// IntentBlocked: a limit or a precondition stops progress (revision
	// limit, cost ceiling, missing or rejected repository image, tripped
	// sandbox breaker, an intent branch or pull request that is not maul's,
	// repeated check failure); the conditions say which.
	// Re-evaluated when the Project changes, so raising a limit resumes the
	// intent in the phase it was blocked from (IntentBlockedFrom).
	IntentBlocked IntentPhase = "Blocked"
	// IntentMerged: every pull request merged — from InReview, or by a
	// human mid-round or while Blocked; the issue is closed as completed.
	// Terminal.
	IntentMerged IntentPhase = "Merged"
	// IntentClosed: a human closed the issue or ran /maul cancel, every
	// pull request settled with at least one closed unmerged (a
	// multi-repository intent's merged ones stay merged, and a notice says
	// which), or the trigger was not an approver's. Terminal. The issue is
	// left without its trigger: closed (by the human, or by intent-controller
	// for cancel and closed pull requests), or, after a refused trigger, with
	// the label removed.
	IntentClosed IntentPhase = "Closed"
	// IntentFailed: plan or build attempts exhausted, or the plan was
	// invalid twice. Stamps completedAt, and the trigger label is removed
	// from the issue on entry; an approver applying it again revives the
	// intent to Planning.
	IntentFailed IntentPhase = "Failed"
)

// intentTransitions is the Intent's legal edge table. Every edge has one
// writer component, intent-controller; the reconciler and the reason are
// noted per edge:
//
//   - ""→Pending: the intent reconciler's first status write on an Intent the
//     project reconciler created (the create itself is spec-only).
//   - Pending→Planning: the trigger label was applied by an approver.
//   - Pending→Closed: the trigger was not an approver's (or was a Bot's); one
//     notice is posted.
//   - Planning→AwaitingApproval: a valid plan was posted on the issue.
//   - Planning→Failed: plan attempts exhausted, or the plan invalid twice.
//   - AwaitingApproval→Building: an approval was accepted (approve label or
//     /maul approve, bound to the plan and input digests).
//   - AwaitingApproval→Planning: an approver asked for a replan (re-applied
//     the trigger label or ran /maul replan).
//   - Building→InReview: every pull request is open.
//   - Building→Failed: build attempts exhausted.
//   - InReview→Revising: a review round, /maul revise, or a check-fix round
//     started (slice 1b).
//   - Revising→InReview: the round pushed, or failed (a condition is set and
//     a notice posted; a failed round never fails the intent).
//   - InReview→Merged: every pull request merged.
//   - Revising→Merged, Blocked→Merged: every pull request merged by a human
//     during a round or while the intent was blocked (pull request state is
//     polled in both). A merge is the human's final word, so the intent
//     completes directly: the running round's Job is deleted through the
//     finalizer, and no false resume through InReview is recorded while a
//     block still holds. These mirror the edges to Closed.
//   - every non-terminal phase→Blocked: a limit or precondition stopped it.
//   - Blocked→the phase it was blocked from: the Project changed and the
//     block no longer holds (IntentBlockedFrom names the target).
//   - every non-terminal phase→Closed: a human closed the issue or ran
//     /maul cancel, or every pull request settled with at least one
//     closed unmerged.
//   - Failed→Planning: revival — an approver applied the trigger label again
//     (it was removed on entry to Failed).
var intentTransitions = map[IntentPhase][]IntentPhase{
	"":                     {IntentPending},
	IntentPending:          {IntentPlanning, IntentBlocked, IntentClosed},
	IntentPlanning:         {IntentAwaitingApproval, IntentBlocked, IntentClosed, IntentFailed},
	IntentAwaitingApproval: {IntentBuilding, IntentPlanning, IntentBlocked, IntentClosed},
	IntentBuilding:         {IntentInReview, IntentBlocked, IntentClosed, IntentFailed},
	IntentInReview:         {IntentRevising, IntentMerged, IntentBlocked, IntentClosed},
	IntentRevising:         {IntentInReview, IntentMerged, IntentBlocked, IntentClosed},
	IntentBlocked: {
		IntentPending, IntentPlanning, IntentAwaitingApproval,
		IntentBuilding, IntentInReview, IntentRevising, IntentMerged, IntentClosed,
	},
	IntentMerged: nil,
	IntentClosed: nil,
	IntentFailed: {IntentPlanning}, // revival by an approver's trigger label
}

// intentTerminal is the set of phases that complete an Intent for TTL
// purposes (status.completedAt is set on entry). Merged and Closed have no
// outgoing edges; Failed is revivable, and revival clears completedAt.
// Blocked is NOT terminal: a blocked intent waits for a human and must not
// expire while it does.
var intentTerminal = map[IntentPhase]bool{
	IntentMerged: true,
	IntentClosed: true,
	IntentFailed: true,
}

// CanTransitionIntent reports whether moving an Intent from phase `from` to
// `to` is legal. The empty phase means a new Intent. Self-transitions are
// always legal no-ops.
func CanTransitionIntent(from, to IntentPhase) bool {
	if from == to {
		return true
	}
	return slices.Contains(intentTransitions[from], to)
}

// IntentTerminal reports whether the phase completes an Intent (starts its
// TTL). Failed is terminal but revivable.
func IntentTerminal(p IntentPhase) bool {
	return intentTerminal[p]
}

// IntentBlockedFrom returns the phase a Blocked intent resumes to once its
// block no longer holds — the phase it was blocked from — or "" when the
// Intent is not Blocked or its history holds no earlier phase. Blocked has
// no self-entries in phaseTimes (self-transitions do not append), so the
// latest non-Blocked entry is always the one immediately before it.
func IntentBlockedFrom(i *Intent) IntentPhase {
	if i.Status.Phase != IntentBlocked {
		return ""
	}
	prior := IntentPhase("")
	for _, pt := range i.Status.PhaseTimes {
		if pt.Phase != IntentBlocked {
			prior = pt.Phase
		}
	}
	return prior
}

// SetIntentPhase moves the Intent to phase `to` at time `now`: it validates
// the transition, appends to status.phaseTimes (keeping the newest
// MaxIntentPhaseTimes entries), and maintains status.completedAt (set on
// terminal entry, cleared on revival — the TTL contract is completedAt +
// TTL). Callers running under conflict retry must call SetIntentPhase again
// after every re-Get so the transition is re-validated against fresh state;
// an illegal transition returns an error and mutates nothing.
func SetIntentPhase(i *Intent, to IntentPhase, now time.Time) error {
	from := i.Status.Phase
	if !CanTransitionIntent(from, to) {
		return fmt.Errorf("illegal intent transition %q -> %q", from, to)
	}
	if from == to {
		return nil
	}
	t := metav1.NewTime(now)
	i.Status.Phase = to
	times := append(i.Status.PhaseTimes, IntentPhaseTime{Phase: to, At: t})
	if over := len(times) - MaxIntentPhaseTimes; over > 0 {
		times = slices.Clone(times[over:])
	}
	i.Status.PhaseTimes = times
	if IntentTerminal(to) {
		i.Status.CompletedAt = &t
	} else {
		i.Status.CompletedAt = nil
	}
	return nil
}

// IntentIssue locates the intent issue: the human-written task.
type IntentIssue struct {
	// Repository is the https URL of the intent repository, copied from the
	// Project's spec.intentRepository at creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	Repository string `json:"repository"`
	// Number of the issue, at most seven digits (MaxIntentIssueNumber): it
	// is part of every Intent and IntentRun name (see the name budget).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9999999
	Number int64 `json:"number"`
	// URL is the issue's html URL, for display.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url,omitempty"`
}

// IntentRequest records who triggered the intent, as GitHub's issue events
// API reports it — never a handler-time stamp, which is not evidence of
// ordering under webhook redelivery and replay.
type IntentRequest struct {
	// Login of the actor that applied the trigger label. It may be anyone,
	// including a Bot ("<name>[bot]"): the intent reconciler decides
	// authority from it, and a non-approver's trigger closes the Intent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*(\[bot\])?$`
	Login string `json:"login"`
	// At is the labeled event's created_at.
	At metav1.Time `json:"at"`
	// EventID is the GitHub id of the labeled issue event.
	// +kubebuilder:validation:Minimum=1
	EventID int64 `json:"eventID"`
	// UserID is GitHub's numeric id of the actor: their identity, which the
	// login is not (design L1). Zero on an Intent created before it was
	// recorded.
	// +optional
	// +kubebuilder:validation:Minimum=1
	UserID int64 `json:"userID,omitempty"`
}

// IntentActionSource names where a human action on the intent issue came
// from, and so which GitHub id space its event id is in: a labeled issue
// event's id and an issue comment's id are separate id spaces, so an id alone
// cannot say which kind of action it points at.
// +kubebuilder:validation:Enum=label;command
type IntentActionSource string

// Human action sources.
const (
	// IntentActionLabel: a label applied to the intent issue — the approve
	// label, or the trigger label re-applied. The id is the labeled issue
	// event's, from the issue events API.
	IntentActionLabel IntentActionSource = "label"
	// IntentActionCommand: an issue comment whose first line is a /maul
	// command (approve, replan). The id is the comment's.
	IntentActionCommand IntentActionSource = "command"
)

// IntentAction identifies one human action on the intent issue as GitHub's
// API reports it — never a handler-time stamp.
type IntentAction struct {
	// Source of the action, and so the id space of EventID.
	Source IntentActionSource `json:"source"`
	// EventID is GitHub's id of the action, in Source's id space.
	// +kubebuilder:validation:Minimum=1
	EventID int64 `json:"eventID"`
	// Login of the actor. It may be anyone, a Bot ("<name>[bot]")
	// included: an action refused for its actor is consumed too.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*(\[bot\])?$`
	Login string `json:"login"`
	// UserID is GitHub's numeric id of the actor (design L1). Zero on an
	// action recorded before it was.
	// +optional
	// +kubebuilder:validation:Minimum=1
	UserID int64 `json:"userID,omitempty"`
	// At is the action's created_at, as GitHub reports it.
	At metav1.Time `json:"at"`
}

// IntentSpec identifies one intent issue. intent-controller writes it once,
// at creation; it is immutable except spec.suspend, which humans may patch
// with the native verb (CEL-enforced per field, so a mutation of any other
// field is refused at admission).
type IntentSpec struct {
	// Project names the Project this intent belongs to (the Project whose
	// trigger label the issue carries); at most MaxProjectNameLength, like
	// the Project's own name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=25
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.project is immutable"
	Project string `json:"project"`
	// Issue is the intent issue.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.issue is immutable"
	Issue IntentIssue `json:"issue"`
	// RequestedBy is who applied the trigger label, from the issue events.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.requestedBy is immutable"
	RequestedBy IntentRequest `json:"requestedBy"`
	// Suspend pauses the intent (human-written): no run is launched and
	// nothing is written to GitHub for it until cleared. Running Jobs
	// finish; a build that finishes meanwhile holds its push (commit and
	// branch) until then, without its slot of the run pool, and loses it if
	// the suspension outlasts the finished Job's TTL (the run ends
	// hold_expired, which does not count as an attempt).
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// IntentPhaseTime records when the intent entered a phase.
type IntentPhaseTime struct {
	// Phase entered.
	Phase IntentPhase `json:"phase"`
	// At is the entry time.
	At metav1.Time `json:"at"`
}

// IntentInput is the immutable snapshot of the issue a plan was made from.
type IntentInput struct {
	// Revision of the snapshot, 1-based. Every entry to Planning except a
	// resume from Blocked — the first, a replan, a revival from Failed —
	// takes a new snapshot at the next revision, so a revision is never
	// reused; it is the round of that Planning's plan runs. At most
	// MaxIntentRound.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Revision int32 `json:"revision"`
	// Digest is the sha256 of the snapshot bytes; an approval is bound to
	// it, and a changed issue body refuses the approval.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// ConfigMap names the immutable ConfigMap holding the snapshot
	// (<intent>-input-r<revision>): the issue title and body, plus, on a
	// replan, the approvers' comments since the previous plan. On a replan
	// or revival it also holds the planner's context file (context.md):
	// the previous plan, the last build's failure and the earlier approver
	// comments, which are not part of the request or its digest.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// ContextDigest is the sha256 of the snapshot's context file, which
	// this revision's plan runs are handed beside the request; empty when
	// there is none (always on the first plan). It never binds an
	// approval: the approval is bound to Digest, the request's.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContextDigest string `json:"contextDigest,omitempty"`
	// Trigger is the trigger action that opened this revision: the one
	// that created the Intent, or the replan or revival that moved it to
	// Planning. Every plan run of the revision is attributed from it (design
	// L1), never from status.lastTrigger, which a later refused or
	// unavailable action also moves. Absent on a revision opened before it
	// was recorded.
	// +optional
	Trigger *IntentAction `json:"trigger,omitempty"`
}

// IntentPlan is the plan posted for approval.
type IntentPlan struct {
	// Revision of the plan: the input revision it was planned from, so it
	// is never reused. Plan revisions can skip a number, when that
	// Planning's plan runs failed before any plan was posted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	Revision int32 `json:"revision"`
	// Digest is the sha256 of the raw plan report bytes stored in
	// ConfigMap; the build run re-hashes them at launch and refuses a
	// mismatch.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// ConfigMap names the immutable ConfigMap holding the raw plan report
	// (<intent>-plan-r<revision>).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigMap string `json:"configMap"`
	// CommentID is GitHub's id of the posted plan comment; zero until
	// posted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CommentID int64 `json:"commentID,omitempty"`
	// CommentDigest is the sha256 of the comment body as GitHub returned it
	// when posted. The approval re-fetches the comment and refuses if it no
	// longer hashes to this, so the builder builds what the approver saw.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CommentDigest string `json:"commentDigest,omitempty"`
	// PostedAt is the comment's created_at as GitHub returned it; only an
	// approval event strictly after it counts.
	// +optional
	PostedAt *metav1.Time `json:"postedAt,omitempty"`
	// Summary is the plan's one-line summary from its frontmatter.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Summary string `json:"summary,omitempty"`
	// Repositories are the URLs of the Project repositories the plan
	// touches (a subset of the Project's; a plan naming any other is
	// rejected).
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=256
	Repositories []string `json:"repositories,omitempty"`
}

// IntentApproval records the accepted approval and exactly what it approved.
type IntentApproval struct {
	// By is the approver's login. Only an approver's approval is ever
	// accepted, and a Bot never is, so the pattern admits no "[bot]".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by"`
	// ByID is GitHub's numeric id of the approver: their identity, which the
	// login is not (design L1). Zero on an approval recorded before it was.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ByID int64 `json:"byID,omitempty"`
	// Source of the approving action — the approve label, or a /maul
	// approve comment — and so which id space EventID is in.
	Source IntentActionSource `json:"source"`
	// EventID is GitHub's id of the approving action, in Source's id space:
	// the labeled issue event for the approve label, or the comment
	// carrying /maul approve.
	// +kubebuilder:validation:Minimum=1
	EventID int64 `json:"eventID"`
	// At is the approving action's created_at, as GitHub reports it.
	At metav1.Time `json:"at"`
	// PlanRevision is the plan revision approved; the build runs it starts
	// take it as their round.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	PlanRevision int32 `json:"planRevision"`
	// PlanDigest is the digest of the plan approved.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest"`
	// InputDigest is the digest of the issue snapshot the plan was made
	// from.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	InputDigest string `json:"inputDigest"`
}

// IntentPullRequest is one pull request maul opened for the intent. Pull
// requests are correlated only by the (repository, number, nodeID) recorded
// here when maul opened them — never by head ref, and never a fork's.
type IntentPullRequest struct {
	// Repository is the https URL of the app repository the pull request
	// is in (one of the Project's repositories); the list key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Repository string `json:"repository"`
	// Number of the pull request.
	// +kubebuilder:validation:Minimum=1
	Number int64 `json:"number"`
	// URL is the pull request's html URL.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	URL string `json:"url,omitempty"`
	// NodeID is GitHub's global node id of the pull request.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	NodeID string `json:"nodeID,omitempty"`
	// HeadSHA is the head commit last observed (maul's push, or a human's
	// since).
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	HeadSHA string `json:"headSHA,omitempty"`
	// State of the pull request.
	// +optional
	// +kubebuilder:validation:Enum=open;merged;closed
	State string `json:"state,omitempty"`
	// MergedAt is when the pull request merged.
	// +optional
	MergedAt *metav1.Time `json:"mergedAt,omitempty"`
	// MergeCommitSHA is the commit the merge put on the base branch.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	MergeCommitSHA string `json:"mergeCommitSHA,omitempty"`
	// ChecksObservedHeadSHA is a pushed head of this pull request whose
	// named checks settled, or whose check-poll timeout elapsed without an
	// observed failure. Once recorded, this head is not polled again; a new
	// pushed head resets it. Recorded per pull request, so a multi-repository
	// intent watches each repository's checks on their own.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	ChecksObservedHeadSHA string `json:"checksObservedHeadSHA,omitempty"`
	// ChecksObservedProjectGeneration is the Project generation whose check
	// names were observed at ChecksObservedHeadSHA. Changing checks.fix
	// resumes polling.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ChecksObservedProjectGeneration int64 `json:"checksObservedProjectGeneration,omitempty"`
	// ChecksRerun is the one re-run of failed Actions jobs maul asked for
	// on this pull request's head (the Project's checks.rerunFailed). It
	// stands only while its head is the pull request's: a failure at that
	// head after it starts the fix round, and a new pushed head may be
	// re-run once again. Written by the intent reconciler only.
	// +optional
	ChecksRerun *IntentChecksRerun `json:"checksRerun,omitempty"`
	// PreviewCommentID is GitHub's id of the sticky preview comment on this
	// pull request (marker <!-- maul:notice <namespace>/<intent> preview
	// -->): posted once the intent's preview is first live at the pull
	// request's head, and edited in place thereafter. Written by the intent
	// reconciler only; 0 while there is none.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PreviewCommentID int64 `json:"previewCommentID,omitempty"`
	// PreviewDigest is the sha256 of the body last written to the preview
	// comment, so an unchanged preview is never written again. Written by
	// the intent reconciler only.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PreviewDigest string `json:"previewDigest,omitempty"`
	// AcceptanceStatusID is GitHub's id of maul's latest maul/acceptance
	// commit status on AcceptanceHead: a later maul/acceptance with another
	// id is someone else's, and maul re-asserts its own. Written by the
	// intent reconciler only; 0 while none was posted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	AcceptanceStatusID int64 `json:"acceptanceStatusID,omitempty"`
	// AcceptanceHead is the head commit maul's latest maul/acceptance was
	// posted on.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	AcceptanceHead string `json:"acceptanceHead,omitempty"`
	// AcceptanceDigest is the sha256 over the head, the head-set digest, the
	// state and the description last posted as maul/acceptance, so an
	// unchanged status is never posted again and a moved sibling re-posts it.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	AcceptanceDigest string `json:"acceptanceDigest,omitempty"`
	// VerifyDigest is the sha256 of the verification section last written
	// into the sticky preview comment.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	VerifyDigest string `json:"verifyDigest,omitempty"`
}

// IntentChecksRerun records the re-run of failed GitHub Actions jobs maul
// requested on one pull request head before a check-fix round.
type IntentChecksRerun struct {
	// HeadSHA is the pull request head whose failed checks were re-run.
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	HeadSHA string `json:"headSHA"`
	// CheckRunIDs are the failed check runs whose Actions runs were re-run,
	// sorted. While one of them is still the latest run of its check, the
	// re-run has not reported yet.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	CheckRunIDs []int64 `json:"checkRunIDs"`
	// Checks are those check runs' names, sorted.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=128
	Checks []string `json:"checks,omitempty"`
	// WorkflowRunIDs are the Actions runs whose failed jobs were re-run,
	// sorted.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	WorkflowRunIDs []int64 `json:"workflowRunIDs"`
	// RequestedAt is when the re-run was requested. The checks timeout
	// counts from it again.
	RequestedAt metav1.Time `json:"requestedAt"`
}

// IntentPreviewBase records the default-branch head an intent previews for a
// previewed Project repository it has no pull request in, so its preview
// shows the change beside what is already on main.
type IntentPreviewBase struct {
	// Repository is the https URL of the Project repository; the list key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`
	Repository string `json:"repository"`
	// SHA is the default-branch head read once, when the intent's review
	// began. The preview runs that commit's immutable sha-<SHA> image.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	SHA string `json:"sha"`
}

// IntentUsage totals the agent spend of every run of the intent. Money is
// int64 micro-USD (structural schemas forbid floats, and a sum of decimal
// strings would round); zeroes mean nothing was reported.
type IntentUsage struct {
	// CostMicroUSD is the summed harness-reported cost in micro-USD,
	// checked against the Project's maxCostMicroUSD before every launch.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
	// InputTokens consumed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	InputTokens int64 `json:"inputTokens,omitempty"`
	// OutputTokens produced.
	// +optional
	// +kubebuilder:validation:Minimum=0
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CacheReadTokens read from prompt cache.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// CacheCreationTokens written to prompt cache.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CacheCreationTokens int64 `json:"cacheCreationTokens,omitempty"`
}

// IntentTracking locates the one status comment kept on the intent issue.
type IntentTracking struct {
	// StatusCommentID is GitHub's id of the status comment, posted exactly
	// once (marker <!-- maul:intent <project>/<issue> -->) and edited
	// thereafter.
	// +optional
	// +kubebuilder:validation:Minimum=0
	StatusCommentID int64 `json:"statusCommentID,omitempty"`
	// StatusDigest is the sha256 of the body last written to it, so an
	// unchanged status is never re-written.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	StatusDigest string `json:"statusDigest,omitempty"`
}

// IntentCommands is how far the poll has read the comments on the intent
// issue, and whom it has already refused there.
type IntentCommands struct {
	// Seen is the newest comment on the intent issue the poll has settled:
	// every comment up to it was answered, if it carried a command, or
	// needed nothing. Later polls list only the comments since it and read
	// only newer ones as commands. So an answered command is never answered
	// again, even after maul's reply to it is deleted, and a comment
	// edited after it was settled is never read as a command.
	// +optional
	Seen *IntentCommentRef `json:"seen,omitempty"`
	// RefusedActors are the GitHub ids of the accounts already sent a reply
	// refusing a command on this intent because they are not approvers (or
	// are bots), the latest MaxIntentRefusedActors of them. A later command
	// from one of them gets no reaction and no reply, so an account
	// commenting repeatedly cannot make maul write to GitHub once per
	// comment. An approver's command is always answered.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	RefusedActors []int64 `json:"refusedActors,omitempty"`
	// Applied are the GitHub ids of the comments carrying the acceptance
	// commands (/maul verify, /maul accept) whose effect the Intent
	// recorded, on the intent issue or any of its pull requests, the latest
	// MaxIntentAppliedCommands of them, each written with its effect. A
	// comment among them is never applied again, even after maul's reply to
	// it is deleted or the effect it recorded was consumed or superseded:
	// only its reply may still be owed.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	Applied []int64 `json:"applied,omitempty"`
}

// IntentCommentRef names one comment on the intent issue.
type IntentCommentRef struct {
	// ID is GitHub's id of the comment. GitHub's comment ids grow with
	// creation.
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id"`
	// At is the comment's created_at, as GitHub reports it.
	At metav1.Time `json:"at"`
}

// IntentAcceptance is the approved plan's acceptance criteria and the
// approvers' acceptances of the manual ones. intent-controller writes the
// criteria once per approved plan revision, from the plan bytes it re-reads
// under the approved digest; no run writes Intent status, so nothing a fix
// round does can change what was approved.
//
// Under the evaluator the approved plan declares outcomes instead of
// criteria (never both): Outcomes with their OutcomesDigest, the check store
// the evaluator's confirmed and reviewed checks are saved in, and the check
// reviewer's cached answers. Digest is then the digest of the plan's empty
// criteria list, as for a plan that declared none.
// +kubebuilder:validation:XValidation:rule="!has(self.criteria) || !has(self.outcomes)",message="an approved plan declares criteria or outcomes, never both"
// +kubebuilder:validation:XValidation:rule="has(self.outcomes) == has(self.outcomesDigest)",message="outcomes and outcomesDigest are set together"
type IntentAcceptance struct {
	// PlanRevision is the approved plan revision the criteria are from.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	PlanRevision int32 `json:"planRevision"`
	// Digest is the digest of the full criteria list, manual criteria
	// included (acceptance.Digest): what every PreviewVerification's
	// criteriaDigest must equal.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Criteria are the approved criteria in plan order; empty when the plan
	// declared none.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Criteria []AcceptanceCriterion `json:"criteria,omitempty"`
	// Outcomes are the approved outcomes in plan order (the evaluator).
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=id
	Outcomes []AcceptanceOutcome `json:"outcomes,omitempty"`
	// OutcomesDigest is the digest of the full outcome list, manual
	// outcomes included: what every outcome PreviewVerification's
	// outcomesDigest must equal.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	OutcomesDigest string `json:"outcomesDigest,omitempty"`
	// Checks names the check store of the approved plan revision; unset
	// until a first check is saved.
	// +optional
	Checks *CheckStoreRef `json:"checks,omitempty"`
	// Reviews cache the check reviewer's answers, so a check is reviewed
	// once per outcome statement.
	// +optional
	// +kubebuilder:validation:MaxItems=80
	// +listType=atomic
	Reviews []CheckReviewRecord `json:"reviews,omitempty"`
	// Accepted are the approvers' /maul accept records, oldest first.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Accepted []AcceptanceRecord `json:"accepted,omitempty"`
}

// AcceptanceRecord is one approver's /maul accept of a manual, command or
// quarantined criterion. It holds only at the combined change it was given
// at: once the heads move it is shown as not re-checked.
type AcceptanceRecord struct {
	// ID of the accepted criterion.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	ID string `json:"id"`
	// By is the approver's login, for display.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by"`
	// ByID is GitHub's numeric id of the approver: the identity.
	// +kubebuilder:validation:Minimum=1
	ByID int64 `json:"byID"`
	// At is the command comment's created_at.
	At metav1.Time `json:"at"`
	// CommentID is GitHub's id of the comment carrying the command.
	// +kubebuilder:validation:Minimum=1
	CommentID int64 `json:"commentID"`
	// HeadSetDigest is the combined change the acceptance was given at.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	HeadSetDigest string `json:"headSetDigest"`
	// Heads are that combined change's short commit SHAs, for display.
	// +optional
	// +kubebuilder:validation:MaxItems=4
	// +listType=atomic
	// +kubebuilder:validation:items:Pattern=`^[0-9a-f]{7,40}$`
	Heads []string `json:"heads,omitempty"`
	// Note is the text the approver wrote after the command, if any. It
	// is untrusted and shown only through untrusted.Visible.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Note string `json:"note,omitempty"`
}

// MaxAcceptanceNoteBytes bounds AcceptanceRecord.Note. The schema marker
// repeats it as a literal; keep them in lockstep.
const MaxAcceptanceNoteBytes = 1024

// IntentVerificationState is where an intent's acceptance verification
// stands. It is a sub-state of review: no Intent phase depends on it.
// +kubebuilder:validation:Enum=NotDeclared;Off;WaitingForCI;WaitingForPreview;Verifying;Passed;Failing;Fixing;Exhausted;NeedsReplan;Unavailable;Evaluating;Unconfirmed
type IntentVerificationState string

// Verification states.
const (
	// VerificationNotDeclared: the approved plan declared no criteria.
	VerificationNotDeclared IntentVerificationState = "NotDeclared"
	// VerificationOff: the Project or the controller does not verify.
	VerificationOff IntentVerificationState = "Off"
	// VerificationWaitingForCI: a pull request's named checks are not green
	// at its head yet.
	VerificationWaitingForCI IntentVerificationState = "WaitingForCI"
	// VerificationWaitingForPreview: the Preview is not live at exactly the
	// heads yet.
	VerificationWaitingForPreview IntentVerificationState = "WaitingForPreview"
	// VerificationVerifying: a PreviewVerification is in flight.
	VerificationVerifying IntentVerificationState = "Verifying"
	// VerificationPassed: every automatable criterion passed at the current
	// heads (some perhaps only on retry: quarantined).
	VerificationPassed IntentVerificationState = "Passed"
	// VerificationFailing: a criterion failed and no fix round runs.
	VerificationFailing IntentVerificationState = "Failing"
	// VerificationFixing: a verify-fix round runs.
	VerificationFixing IntentVerificationState = "Fixing"
	// VerificationExhausted: criteria still fail and maxVerifyFixes is
	// spent.
	VerificationExhausted IntentVerificationState = "Exhausted"
	// VerificationNeedsReplan: a criterion can no longer be run as approved
	// (its target is no longer previewed); only a replan helps.
	VerificationNeedsReplan IntentVerificationState = "NeedsReplan"
	// VerificationUnavailable: maul's own machinery failed every attempt,
	// or the Preview was not live within the bound (UnavailableReason).
	VerificationUnavailable IntentVerificationState = "Unavailable"
	// VerificationEvaluating: the evaluator judges the outcomes (its tool
	// pod or its evaluate run, a confirmation or a check review is in
	// flight).
	VerificationEvaluating IntentVerificationState = "Evaluating"
	// VerificationUnconfirmed: the evaluation settled without confirming
	// every outcome (unsure, disputed, an anomaly, or out of rounds, budget
	// or time): the hand-off is released as "maul couldn't confirm this",
	// never a block.
	VerificationUnconfirmed IntentVerificationState = "Unconfirmed"
)

// OutcomeResultValue is the decided result of one approved outcome
// (acceptance.Decide): passed only on a confirmed, reviewed, deterministic
// check the evaluator agrees with; failed only on a failing confirmed check
// the evaluator also fails; accepted by an approver's /maul accept.
// +kubebuilder:validation:Enum=passed;failed;unsure;quarantined;accepted
type OutcomeResultValue string

// Outcome results.
const (
	OutcomePassed      OutcomeResultValue = "passed"
	OutcomeFailed      OutcomeResultValue = "failed"
	OutcomeUnsure      OutcomeResultValue = "unsure"
	OutcomeQuarantined OutcomeResultValue = "quarantined"
	OutcomeAccepted    OutcomeResultValue = "accepted"
)

// OutcomeResult is the decided result of one approved outcome.
type OutcomeResult struct {
	// ID of the outcome.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	ID string `json:"id"`
	// Result decided.
	Result OutcomeResultValue `json:"result"`
	// Basis is the digest of the saved check the result rests on; unset
	// when none does.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Basis string `json:"basis,omitempty"`
	// Reason says why the result is not passed. It may quote the
	// evaluator, so it is untrusted and shown only through the templates'
	// sanitising.
	// +optional
	// +kubebuilder:validation:MaxLength=600
	Reason string `json:"reason,omitempty"`
	// Line is the maul-rendered one-line description of the basis check
	// (its steps and assertions in maul's words).
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	Line string `json:"line,omitempty"`
}

// AnomalyRecord is one look-around anomaly the evaluator reported, with the
// ledger entries that show it: it downgrades the hand-off, never starts a
// fix. Summary and route are the evaluator's, untrusted.
type AnomalyRecord struct {
	// Summary of the anomaly.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=600
	Summary string `json:"summary"`
	// Route it was seen on.
	// +optional
	// +kubebuilder:validation:MaxLength=300
	Route string `json:"route,omitempty"`
	// Ledger are the entries cited, by seq.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=512
	Ledger []int32 `json:"ledger,omitempty"`
}

// HandoffMode is when the preview link is handed to humans: gated (held
// until the evaluation passes or gives up, bounded by handoffMax) or early
// (at once, labelled unverified: slice 1's behaviour).
// +kubebuilder:validation:Enum=gated;early
type HandoffMode string

// Hand-off modes.
const (
	HandoffGated HandoffMode = "gated"
	HandoffEarly HandoffMode = "early"
)

// HandoffReason says why the hand-off was released.
// +kubebuilder:validation:Enum=ready;unconfirmed;out-of-time;human;command;early;evaluator-unverified
type HandoffReason string

// Hand-off reasons.
const (
	// HandoffReasonReady: every outcome passed with no anomaly.
	HandoffReasonReady HandoffReason = "ready"
	// HandoffReasonUnconfirmed: the evaluation settled without confirming.
	HandoffReasonUnconfirmed HandoffReason = "unconfirmed"
	// HandoffReasonOutOfTime: handoffMax ran out.
	HandoffReasonOutOfTime HandoffReason = "out-of-time"
	// HandoffReasonHuman: a human comment or push released it at once.
	HandoffReasonHuman HandoffReason = "human"
	// HandoffReasonCommand: an approver's /maul preview.
	HandoffReasonCommand HandoffReason = "command"
	// HandoffReasonEarly: the mode is early.
	HandoffReasonEarly HandoffReason = "early"
	// HandoffReasonEvaluatorUnverified: the Project's harness has no
	// verified evaluator (EvaluatorUnverified), so nothing is held.
	HandoffReasonEvaluatorUnverified HandoffReason = "evaluator-unverified"
)

// HandoffState is the hand-off of an intent's preview to humans.
// +kubebuilder:validation:XValidation:rule="!has(self.released) || !self.released || (has(self.releasedAt) && has(self.reason))",message="a released hand-off records when and why"
type HandoffState struct {
	// Mode in force for the intent.
	Mode HandoffMode `json:"mode"`
	// Released reports that the preview link has been handed over.
	// +optional
	Released bool `json:"released,omitempty"`
	// ReleasedAt is when.
	// +optional
	ReleasedAt *metav1.Time `json:"releasedAt,omitempty"`
	// Reason is why.
	// +optional
	Reason HandoffReason `json:"reason,omitempty"`
}

// VerificationUnavailableReason says why a verification is Unavailable.
// +kubebuilder:validation:Enum=PreviewNotLive
type VerificationUnavailableReason string

// Unavailable reasons. None (empty) is maul's machinery failing every
// attempt.
const (
	// VerificationUnavailablePreviewNotLive: the Preview was not live at
	// exactly the heads within the wait bound.
	VerificationUnavailablePreviewNotLive VerificationUnavailableReason = "PreviewNotLive"
)

// VerdictOutcome is acceptance.Judge's verdict on one criterion: pass,
// quarantined (failed, then passed on its retry: never counted as passed),
// or fail. Nothing the application does yields any other outcome.
// +kubebuilder:validation:Enum=pass;quarantined;fail
type VerdictOutcome string

// Verdict outcomes.
const (
	VerdictPass        VerdictOutcome = "pass"
	VerdictQuarantined VerdictOutcome = "quarantined"
	VerdictFail        VerdictOutcome = "fail"
)

// CriterionState is the judged state of one criterion that did not pass.
type CriterionState struct {
	// ID of the criterion.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	ID string `json:"id"`
	// Outcome judged.
	Outcome VerdictOutcome `json:"outcome"`
	// Fingerprint is the failure's signature over status codes, transport
	// error classes and matched-expectation bits only, never a value.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^maul-verify-fingerprint/[0-9]+:[0-9a-f]{64}$`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// AutoFixState says whether a failed verification may start a verify-fix
// round on its own.
// +kubebuilder:validation:Enum=Armed;Halted
type AutoFixState string

// Auto-fix states.
const (
	AutoFixArmed  AutoFixState = "Armed"
	AutoFixHalted AutoFixState = "Halted"
)

// AutoFixReason says why automatic verify-fix rounds halted.
// +kubebuilder:validation:Enum=HumanReview;HumanCommand;HumanPush;MaxVerifyFixes;RepeatedFailure;CostCeiling;CriterionError;NoOpenPR;FixFoundNoDefect;HumanStop
type AutoFixReason string

// Auto-fix halt reasons.
const (
	// AutoFixHumanReview: an approver's review started a round; the human
	// wins.
	AutoFixHumanReview AutoFixReason = "HumanReview"
	// AutoFixHumanCommand: an approver's /maul revise or retry started a
	// round.
	AutoFixHumanCommand AutoFixReason = "HumanCommand"
	// AutoFixHumanPush: someone other than maul pushed to a pull request.
	AutoFixHumanPush AutoFixReason = "HumanPush"
	// AutoFixMaxVerifyFixes: the Project's maxVerifyFixes is spent.
	AutoFixMaxVerifyFixes AutoFixReason = "MaxVerifyFixes"
	// AutoFixRepeatedFailure: a fix round ended with the same failure
	// fingerprint it was started for.
	AutoFixRepeatedFailure AutoFixReason = "RepeatedFailure"
	// AutoFixCostCeiling: the intent's spend reached its ceiling.
	AutoFixCostCeiling AutoFixReason = "CostCeiling"
	// AutoFixCriterionError: a criterion can no longer be run as approved.
	AutoFixCriterionError AutoFixReason = "CriterionError"
	// AutoFixNoOpenPR: no failing criterion's repository has an open pull
	// request to fix.
	AutoFixNoOpenPR AutoFixReason = "NoOpenPR"
	// AutoFixFixFoundNoDefect: an acceptance-fix round's agent concluded,
	// explicitly, that the code has no defect to fix, so it changed
	// nothing; a human judges whether the verification was wrong.
	AutoFixFixFoundNoDefect AutoFixReason = "FixFoundNoDefect"
	// AutoFixHumanStop: an approver's /maul verify stop halted automatic
	// fixes, starting nothing.
	AutoFixHumanStop AutoFixReason = "HumanStop"
)

// AutoFix is the automatic verify-fix latch: armed by each approved plan
// revision, halted by any human round or push or by a bound, and re-armed
// only by an approver's /maul verify fix.
type AutoFix struct {
	// State of the latch; unset is Armed.
	// +optional
	State AutoFixState `json:"state,omitempty"`
	// Reason the latch halted.
	// +optional
	Reason AutoFixReason `json:"reason,omitempty"`
	// Since is when the latch took its state.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// Rearm is the approver's /maul verify fix that last re-armed it.
	// +optional
	Rearm *Rearm `json:"rearm,omitempty"`
	// Stop is the approver's /maul verify stop that halted it (reason
	// HumanStop); a re-arm clears it.
	// +optional
	Stop *AutoFixStop `json:"stop,omitempty"`
}

// AutoFixStop records an approver's /maul verify stop.
type AutoFixStop struct {
	// By is the approver's login, for display.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by"`
	// ByID is GitHub's numeric id of the approver.
	// +kubebuilder:validation:Minimum=1
	ByID int64 `json:"byID"`
	// At is the command comment's created_at.
	At metav1.Time `json:"at"`
	// CommentID is GitHub's id of the comment carrying the command.
	// +kubebuilder:validation:Minimum=1
	CommentID int64 `json:"commentID"`
}

// Rearm records an approver's /maul verify fix.
type Rearm struct {
	// By is the approver's login, for display.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by"`
	// ByID is GitHub's numeric id of the approver.
	// +kubebuilder:validation:Minimum=1
	ByID int64 `json:"byID"`
	// At is the command comment's created_at.
	At metav1.Time `json:"at"`
	// Round is status.rounds when re-armed: only a human round after it
	// halts the latch again.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Round int32 `json:"round"`
}

// NoDefectRecord is an acceptance-fix round's conclusion that the code has
// no defect to fix: the run, and the agent's one-line reason, bounded,
// shown only through the templates' sanitising.
type NoDefectRecord struct {
	// Run is the IntentRun that concluded it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Run string `json:"run"`
	// Reason is the agent's account of why the verification failed.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// At is when the round completed.
	At metav1.Time `json:"at"`
	// HeadSetDigest is the combined change it was concluded about: it is
	// shown only while the verification is about the same one.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	HeadSetDigest string `json:"headSetDigest,omitempty"`
}

// VerifyRequest is an approver's pending /maul verify, run at the next
// opportunity.
type VerifyRequest struct {
	// By is the approver's login, for display.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by"`
	// ByID is GitHub's numeric id of the approver.
	// +kubebuilder:validation:Minimum=1
	ByID int64 `json:"byID"`
	// At is the command comment's created_at.
	At metav1.Time `json:"at"`
	// CommentID is GitHub's id of the comment carrying the command.
	// +kubebuilder:validation:Minimum=1
	CommentID int64 `json:"commentID"`
	// Fix also re-arms automatic fixes (/maul verify fix).
	// +optional
	Fix bool `json:"fix,omitempty"`
	// Reset also restarts the Preview's parts first (/maul verify --reset).
	// +optional
	Reset bool `json:"reset,omitempty"`
}

// ResetState is the last /maul verify --reset: its generation is copied
// into the Preview's spec.resetGeneration.
type ResetState struct {
	// Generation counts resets; it never decreases.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Generation int64 `json:"generation,omitempty"`
	// By is the approver's login, for display.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	By string `json:"by,omitempty"`
	// ByID is GitHub's numeric id of the approver.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ByID int64 `json:"byID,omitempty"`
	// At is the command comment's created_at.
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// IntentVerification is the state of the acceptance verification of an
// intent's combined change: the judged result of its latest
// PreviewVerification, the auto-fix latch, and pending human requests.
// Written by the intent reconciler only.
type IntentVerification struct {
	// State of the verification.
	// +optional
	State IntentVerificationState `json:"state,omitempty"`
	// Round is the latest verification round.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Round int32 `json:"round,omitempty"`
	// Attempt is the latest attempt within it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	Attempt int32 `json:"attempt,omitempty"`
	// Ref is the latest PreviewVerification, pinned by UID.
	// +optional
	Ref *ObjectReference `json:"ref,omitempty"`
	// HeadSetDigest is the combined change the state is about.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	HeadSetDigest string `json:"headSetDigest,omitempty"`
	// Heads are that combined change's components and revisions.
	// +optional
	// +kubebuilder:validation:MaxItems=4
	// +listType=map
	// +listMapKey=component
	Heads []VerificationHead `json:"heads,omitempty"`
	// The counters and lists below count criteria, or outcomes under the
	// evaluator: their bounds are the larger list's (20 outcomes).
	//
	// Passed counts the automatable criteria that passed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	Passed int32 `json:"passed,omitempty"`
	// Failed counts the automatable criteria that failed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	Failed int32 `json:"failed,omitempty"`
	// Quarantined counts the criteria that passed only on retry.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	Quarantined int32 `json:"quarantined,omitempty"`
	// ManualOpen counts the manual criteria not accepted at the current
	// heads.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	ManualOpen int32 `json:"manualOpen,omitempty"`
	// ManualAccepted counts the manual criteria accepted at the current
	// heads.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	ManualAccepted int32 `json:"manualAccepted,omitempty"`
	// NotRun counts the criteria this slice cannot run yet.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	NotRun int32 `json:"notRun,omitempty"`
	// Failing are the criteria that did not pass, in plan order.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	Failing []CriterionState `json:"failing,omitempty"`
	// QuarantinedIDs are the criteria that passed only on retry.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9-]{0,31}$`
	QuarantinedIDs []string `json:"quarantinedIDs,omitempty"`
	// AutoFix is the automatic verify-fix latch.
	// +optional
	AutoFix AutoFix `json:"autoFix,omitempty"`
	// NoDefect is the latest acceptance-fix round's conclusion that the code
	// has no defect, kept apart from the auto-fix latch: it stands whatever
	// halted the latch, until the next fix round, the heads it judged move,
	// or a new plan revision.
	// +optional
	NoDefect *NoDefectRecord `json:"noDefect,omitempty"`
	// CancelRun names a Pending verify-fix IntentRun a human round
	// overtook: the run scheduler settles it unlaunched instead of creating
	// its Job.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	CancelRun string `json:"cancelRun,omitempty"`
	// Requested is an approver's pending /maul verify.
	// +optional
	Requested *VerifyRequest `json:"requested,omitempty"`
	// Reset is the last /maul verify --reset.
	// +optional
	Reset ResetState `json:"reset,omitempty"`
	// Automatic counts the verifications started without a request in the
	// current plan revision, against the Project's maxVerifications.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Automatic int32 `json:"automatic,omitempty"`
	// LastAutomaticAt is when the latest of them started.
	// +optional
	LastAutomaticAt *metav1.Time `json:"lastAutomaticAt,omitempty"`
	// InfraRetries counts the attempts maul's own machinery failed in the
	// current round.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	InfraRetries int32 `json:"infraRetries,omitempty"`
	// PreviewWaitSince is when the verification last began waiting for a
	// live Preview at its heads (WaitingForPreview). A wait is bounded:
	// past it the verification is Unavailable (PreviewNotLive) until the
	// Preview is live.
	// +optional
	PreviewWaitSince *metav1.Time `json:"previewWaitSince,omitempty"`
	// UnavailableReason says why the state is Unavailable: PreviewNotLive
	// when the Preview was not live at the heads within the bound (Failed,
	// expired, or never served); empty when maul's own machinery failed
	// every attempt. Read only with state Unavailable.
	// +optional
	UnavailableReason VerificationUnavailableReason `json:"unavailableReason,omitempty"`
	// Outcomes are the decided results of the approved outcomes, in plan
	// order (the evaluator).
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	Outcomes []OutcomeResult `json:"outcomes,omitempty"`
	// Anomalies are the latest round's look-around anomalies.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +listType=atomic
	Anomalies []AnomalyRecord `json:"anomalies,omitempty"`
	// Handoff is the hand-off of the preview link to humans.
	// +optional
	Handoff *HandoffState `json:"handoff,omitempty"`
	// FirstGreenAt is when CI first went green at the first PR heads: the
	// start of the handoffMax clock.
	// +optional
	FirstGreenAt *metav1.Time `json:"firstGreenAt,omitempty"`
	// EvaluationCostMicroUSD is what the intent's evaluate and checkreview
	// runs have spent, against the Project's maxEvaluateCostMicroUSD.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000000000
	EvaluationCostMicroUSD int64 `json:"evaluationCostMicroUSD,omitempty"`
	// Disputed are the digests of the saved checks the evaluator disputed,
	// awaiting a replay round once their replacements are confirmed and
	// reviewed.
	// +optional
	// +kubebuilder:validation:MaxItems=40
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^sha256:[0-9a-f]{64}$`
	Disputed []string `json:"disputed,omitempty"`
}

// IntentStatus is the intent's observed state. Written only by
// intent-controller's intent reconciler.
type IntentStatus struct {
	// Phase of the lifecycle (see intentTransitions for edges and writers).
	// +optional
	Phase IntentPhase `json:"phase,omitempty"`
	// PhaseTimes is the phase entry log, newest last, bounded to the most
	// recent MaxIntentPhaseTimes entries.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	PhaseTimes []IntentPhaseTime `json:"phaseTimes,omitempty"`
	// Conditions of the intent: BudgetExhausted, RevisionLimitReached,
	// ImageRequired, BranchConflict, ApprovalRejected and (slice 1b)
	// ChecksFailing.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the last spec generation acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ChecksObservedHeadSHA is a pushed PR head whose named checks settled,
	// or whose check-poll timeout elapsed without an observed failure. Once
	// recorded, this head is not polled again; a new pushed head resets it.
	// This Intent-level pair is deprecated in favour of the per-PR pair
	// (pullRequests[].checksObservedHeadSHA and
	// checksObservedProjectGeneration), which a multi-repository intent
	// needs; it is kept so a one-PR intent recorded before the move keeps
	// its observation, and is then read only as that fallback.
	// +optional
	ChecksObservedHeadSHA string `json:"checksObservedHeadSHA,omitempty"`
	// ChecksObservedProjectGeneration is the Project generation whose check
	// names were observed at that head. Changing checks.fix resumes polling.
	// Deprecated with ChecksObservedHeadSHA.
	// +optional
	ChecksObservedProjectGeneration int64 `json:"checksObservedProjectGeneration,omitempty"`
	// Input is the current issue snapshot.
	// +optional
	Input *IntentInput `json:"input,omitempty"`
	// Plan is the current plan.
	// +optional
	Plan *IntentPlan `json:"plan,omitempty"`
	// Approval is the accepted approval of the current plan; nil until one
	// is accepted, and cleared by a replan.
	// +optional
	Approval *IntentApproval `json:"approval,omitempty"`
	// CredentialHold is the person whose missing or unusable model
	// credential holds the Intent Blocked (ConditionCredentialRequired,
	// egressBroker.userCredentials.mode enforce only): the one the run it
	// would have launched is attributed to. The block lifts once that
	// person's ModelCredential is Valid; a block from Building is read from
	// the current approval instead, so a later approval by an approver who
	// holds a Valid credential lifts it. Cleared when the block lifts.
	// +optional
	CredentialHold *UserRef `json:"credentialHold,omitempty"`
	// LastTrigger is the newest trigger action consumed after the one that
	// created the intent (spec.requestedBy, which is immutable): the
	// trigger label re-applied, or /maul replan. Every trigger action the
	// controller answers is consumed and recorded here, whatever the
	// answer: acted on (a replan from AwaitingApproval, a revival from
	// Failed), refused as not an approver's, or answered "not available in
	// this phase" (a replan asked for while Planning or Building, say). A
	// later poll considers only trigger actions newer than it (newer than
	// spec.requestedBy while it is nil), comparing GitHub's own created_at
	// and never the controller's phaseTimes or completedAt. So an action
	// that was answered is never acted on later: a replan refused while
	// Building does not revive the intent when the build then fails, a
	// replan refused while Planning does not replay once the plan is
	// posted, and a revival whose plan fails again, posting no plan to
	// anchor on, cannot re-consume the action that revived it. A /maul
	// replan comment from someone who is not an approver is refused before
	// its verb is read, and is consumed by commands.seen instead.
	// +optional
	LastTrigger *IntentAction `json:"lastTrigger,omitempty"`
	// Commands records how far the poll has answered the commands on the
	// intent issue, so none is answered twice or taken from a comment read
	// before.
	// +optional
	Commands *IntentCommands `json:"commands,omitempty"`
	// Branch is the branch every pull request of the intent is opened from:
	// maul-intent/<intent>, created once and then only fast-forwarded.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^maul-intent/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Branch string `json:"branch,omitempty"`
	// PullRequests are the pull requests maul opened, one per app
	// repository.
	// +optional
	// +listType=map
	// +listMapKey=repository
	// +kubebuilder:validation:MaxItems=8
	PullRequests []IntentPullRequest `json:"pullRequests,omitempty"`
	// PreviewBases are the default-branch heads the intent's preview runs
	// for previewed repositories it has no pull request in, one per
	// repository, each recorded once and never rewritten, so the preview
	// does not move when main does. Written by the intent reconciler.
	// +optional
	// +listType=map
	// +listMapKey=repository
	// +kubebuilder:validation:MaxItems=8
	PreviewBases []IntentPreviewBase `json:"previewBases,omitempty"`
	// Rounds is the revise-round ordinal: how many revise-stage rounds have
	// started, whatever their trigger (review, command or checks) and
	// whatever their outcome — a failed round counts. A new round's runs
	// take spec.round = Rounds+1, and the status write that records the
	// round's first run as ActiveRun advances Rounds to it, so a restart
	// between that create and that write recomputes the same name and
	// adopts the run rather than starting the round twice. Retries and the
	// head_moved re-run are further attempts of the same round. Revisions
	// and CheckFixes count subsets of these rounds (slice 1b).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Rounds int32 `json:"rounds,omitempty"`
	// RoundNoticesThrough is the last contiguous revise round whose PR
	// notice was posted or adopted by marker. Zero also recovers notices
	// lost by older controllers. Only intent-controller writes this cursor;
	// pending deliveries survive phase changes and prevent TTL expiry.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	RoundNoticesThrough int32 `json:"roundNoticesThrough,omitempty"`
	// Revisions counts completed review-driven revise rounds, against the
	// Project's maxRevisions.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Revisions int32 `json:"revisions,omitempty"`
	// CheckFixes counts check-fix rounds, against the Project's
	// maxCheckFixes (slice 1b).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	CheckFixes int32 `json:"checkFixes,omitempty"`
	// VerifyFixes counts completed verify-fix rounds, for display. The
	// count held against the Project's maxVerifyFixes is derived from the
	// Intent's runs (the current plan revision's verify rounds), never read
	// from here.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	VerifyFixes int32 `json:"verifyFixes,omitempty"`
	// Acceptance is the approved plan's acceptance criteria, re-parsed from
	// the digest-checked plan bytes once review begins, and the manual
	// criteria approvers accepted since.
	// +optional
	Acceptance *IntentAcceptance `json:"acceptance,omitempty"`
	// Verification is the state of the acceptance verification of the
	// intent's combined change.
	// +optional
	Verification *IntentVerification `json:"verification,omitempty"`
	// Dependencies is the intent's wait on the issues GitHub records as
	// blocking its issue (intent_dependencies_types.go); nil until a start
	// was decided with dependencies honoured.
	// +optional
	Dependencies *IntentDependencies `json:"dependencies,omitempty"`
	// Usage totals the spend of every run.
	// +optional
	Usage IntentUsage `json:"usage,omitempty"`
	// Tracking locates the status comment on the intent issue.
	// +optional
	Tracking *IntentTracking `json:"tracking,omitempty"`
	// ActiveRun points at the IntentRun currently holding the run lease
	// (the lease itself is the deterministic IntentRun create).
	// +optional
	ActiveRun *ObjectReference `json:"activeRun,omitempty"`
	// CompletedAt is set on terminal-phase entry and cleared on revival;
	// the TTL contract is completedAt + TTL.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=maul
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Issue",type=string,JSONPath=`.spec.issue.url`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PRs",type=string,JSONPath=`.status.pullRequests[*].url`,description="The intent's pull requests, one per repository it changed"
// +kubebuilder:printcolumn:name="Revisions",type=integer,JSONPath=`.status.revisions`
// +kubebuilder:printcolumn:name="Cost",type=integer,JSONPath=`.status.usage.costMicroUSD`,description="Total spend in micro-USD"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Intent is one unit of human-requested development work: an issue in a
// Project's intent repository carried from plan, through a human approval,
// to built pull requests and their merge. It is the state machine for that
// work (a local phase enum, single writer intent-controller), owns one
// immutable IntentRun per agent attempt, and expires on a TTL after
// completion. The issue is the human-facing projection. Its name is always
// IntentName(spec.project, spec.issue.number) — <project>-<issue>, at most 33
// characters — so the project reconciler's create is idempotent per issue and
// every name derived from it fits the name budget. (The rule compares the
// issue number as an integer: concatenating string(int), whose size CEL
// cannot bound, would exceed the rule cost budget.)
// +kubebuilder:validation:XValidation:rule="self.metadata.name.startsWith(self.spec.project + '-') && !self.metadata.name.substring(size(self.spec.project) + 1).startsWith('0') && int(self.metadata.name.substring(size(self.spec.project) + 1)) == self.spec.issue.number",message="an Intent is named <spec.project>-<spec.issue.number>"
type Intent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IntentSpec `json:"spec"`
	// +optional
	Status IntentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IntentList contains a list of Intent.
type IntentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Intent `json:"items"`
}
