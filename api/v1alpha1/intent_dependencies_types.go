// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MaxIntentBlockers bounds the blockers an intent records: past it, More
// counts the rest.
const MaxIntentBlockers = 16

// Intent dependencies: an intent whose issue GitHub records as blocked by
// other issues waits in Pending, without a phase of its own, until every
// blocker is closed as completed (intent-controller's dependencies.go).
const (
	// ConditionWaitingOnDependencies marks an Intent whose start waits on
	// the issues its issue is blocked by: True while it waits (a reason
	// below), False once the start was decided (a release reason below).
	// Never written while intent-controller runs with
	// --intent-dependencies=false.
	ConditionWaitingOnDependencies = "WaitingOnDependencies"

	// ConditionDependenciesUnavailable marks a Project whose intents start
	// without waiting for their blockers: True with DependenciesOff, or the
	// degraded reason last read for one of its intents; False
	// (DependenciesRead) while dependencies are read and honoured. Absent
	// until any intent of the Project has decided a start.
	ConditionDependenciesUnavailable = "DependenciesUnavailable"
)

// WaitingOnDependencies reasons, True: the intent waits.
const (
	// ReasonOpenBlockers: at least one blocker is open.
	ReasonOpenBlockers = "OpenBlockers"
	// ReasonBlockerNotCompleted: none is open, but one was closed as not
	// completed (not planned, a duplicate): what it was for never landed.
	ReasonBlockerNotCompleted = "BlockerNotCompleted"
	// ReasonBlockerUnreadable: GitHub counts blockers it would not show.
	ReasonBlockerUnreadable = "BlockerUnreadable"
	// ReasonDependencyCycle: the open blockers lead back to this intent
	// through intents of the same Project.
	ReasonDependencyCycle = "DependencyCycle"
	// ReasonCheckingDependencies: GitHub did not answer the read; the
	// intent waits until DependencyGiveUp after the first failure.
	ReasonCheckingDependencies = "CheckingDependencies"
	// ReasonNoActiveSlot: every blocker is closed (or an approver said
	// start), and the Project's active intents are at its limit.
	ReasonNoActiveSlot = "NoActiveSlot"
)

// WaitingOnDependencies reasons, False: the start was decided.
const (
	// ReasonNoBlockers: the issue was blocked by nothing.
	ReasonNoBlockers = "NoBlockers"
	// ReasonBlockersClosed: every blocker was closed as completed.
	ReasonBlockersClosed = "BlockersClosed"
	// ReasonStartedByApprover: an approver's /maul start ended the wait.
	ReasonStartedByApprover = "StartedByApprover"
	// ReasonDependenciesUnsupported: this GitHub offers no issue
	// dependencies (404/410); the intent started without them.
	ReasonDependenciesUnsupported = "DependenciesUnsupported"
	// ReasonDependenciesForbidden: the App may not read them (403).
	ReasonDependenciesForbidden = "DependenciesForbidden"
	// ReasonDependenciesUnanswered: GitHub did not answer for the whole
	// give-up bound.
	ReasonDependenciesUnanswered = "DependenciesUnanswered"
)

// DependenciesUnavailable reasons of a Project.
const (
	// ReasonDependenciesOff: intent-controller runs with
	// --intent-dependencies=false.
	ReasonDependenciesOff = "DependenciesOff"
	// ReasonDependenciesRead: dependencies are read and honoured.
	ReasonDependenciesRead = "DependenciesRead"
)

// IntentDependencies is an intent's wait on the issues GitHub records as
// blocking its issue. Written only by intent-controller's intent reconciler.
type IntentDependencies struct {
	// Since is when the wait began: the trigger accepted with dependencies
	// honoured. Nil while only an approver's start is recorded and the
	// trigger is still to be decided.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// Blockers are the blockers not yet closed as completed, as last read
	// while the intent waited, open first.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	Blockers []IntentBlocker `json:"blockers,omitempty"`
	// More counts such blockers past the ones recorded.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	More int32 `json:"more,omitempty"`
	// Unreadable counts blockers GitHub counts but would not show.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=999
	Unreadable int32 `json:"unreadable,omitempty"`
	// UnavailableSince is the first failed read of the current failing
	// streak; cleared by a read that succeeds.
	// +optional
	UnavailableSince *metav1.Time `json:"unavailableSince,omitempty"`
	// StartedBy is the approver's /maul start that ended the wait.
	// +optional
	StartedBy *IntentAction `json:"startedBy,omitempty"`
	// ReleasedAt is when the wait ended, for any reason.
	// +optional
	ReleasedAt *metav1.Time `json:"releasedAt,omitempty"`
	// LateNotice is when maul posted the one notice that the issue became
	// blocked after its work started.
	// +optional
	LateNotice *metav1.Time `json:"lateNotice,omitempty"`
	// Late are the open blockers that notice named.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	Late []IntentBlocker `json:"late,omitempty"`
}

// IntentBlocker is one issue blocking an intent's issue, as GitHub listed
// it. No title is kept: the comment links each blocker.
type IntentBlocker struct {
	// Repository is the blocker's repository as an https URL.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^https://[^\s/]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repository string `json:"repository"`
	// Number is the blocker's issue number.
	// +kubebuilder:validation:Minimum=1
	Number int64 `json:"number"`
	// State is the blocker's state.
	// +kubebuilder:validation:Enum=open;closed
	State string `json:"state"`
	// StateReason is GitHub's state_reason, any future value tolerated.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z_]*$`
	StateReason string `json:"stateReason,omitempty"`
}

// Waiting reports an intent whose start waits on its dependencies now: in
// Pending, with WaitingOnDependencies True.
func (in *Intent) Waiting() bool {
	if in.Status.Phase != IntentPending {
		return false
	}
	for _, c := range in.Status.Conditions {
		if c.Type == ConditionWaitingOnDependencies {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
