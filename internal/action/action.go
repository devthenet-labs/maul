// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package action

import (
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// Verbs the human-action surface understands. They are custom RBAC verbs —
// the per-finding ones on findings.patchy.bitwisemedia.uk, the
// integration-scoped ones on integrations.patchy.bitwisemedia.uk: plain
// strings as far as the API server is concerned, so granting one confers
// exactly one action.
const (
	// VerbApprove releases the breaking-change hold and revives handed-off
	// findings.
	VerbApprove = "approve"
	// VerbRetry recovers a Failed finding to the state it failed from.
	VerbRetry = "retry"
	// VerbExpedite skips the waiting stages: the accumulation window, the
	// minimum age, and queue position.
	VerbExpedite = "expedite"
	// VerbSuspend pauses pipeline progress for a finding.
	VerbSuspend = "suspend"
	// VerbResume lifts a suspension.
	VerbResume = "resume"
	// VerbBackfill lists an integration's pre-existing open alerts into
	// findings — the ones that predate webhook coverage (integration-scoped).
	VerbBackfill = "backfill"
	// VerbReplay redelivers the webhook delivery log (integration-scoped
	// demo tooling).
	VerbReplay = "replay"
	// VerbReset deletes every pipeline resource in the namespace
	// (integration-scoped demo tooling).
	VerbReset = "reset"
)

// Intent verbs. They join the same vocabulary so that, when intents gain
// status-page or CLI actions, those use these names too. Until then they
// arrive only as GitHub commands (internal/command) and are not custom RBAC
// verbs: they are deliberately in none of the lists below, so Apply rejects
// them with ErrUnknownVerb, and neither the admission policy, the status
// server's access reviews nor the CLI enumerates them for Findings. An intent
// reuses VerbApprove (approve the posted plan) and VerbRetry (retry a failed
// round on its pull request) with their own meaning there.
const (
	// VerbReplan asks for a fresh plan of an intent awaiting approval.
	VerbReplan = "replan"
	// VerbCancel stops an intent and closes its issue.
	VerbCancel = "cancel"
	// VerbRevise starts a revision round on an intent's pull request.
	VerbRevise = "revise"
)

// ActionVerbs lists the per-finding verbs in the order clients present them.
var ActionVerbs = []string{VerbApprove, VerbRetry, VerbExpedite, VerbSuspend, VerbResume}

// AdminVerbs lists the demo-tooling verbs the status page's user menu
// surfaces, resolved on integrations.
var AdminVerbs = []string{VerbReplay, VerbReset}

// IntegrationVerbs lists every integration-scoped verb, in the order
// clients present them. All are checked on
// integrations.patchy.bitwisemedia.uk — the same resource the admission
// policy's authorizer names, which is what keeps the CLI/status-server
// SubjectAccessReviews and the server-side enforcement in agreement.
var IntegrationVerbs = []string{VerbBackfill, VerbReplay, VerbReset}

// Errors Apply returns. Callers map them onto their own vocabulary: the status
// server turns ErrUnavailable into a 403 and ErrUnknownVerb into a 404; the CLI
// turns them into exit codes.
var (
	// ErrUnavailable reports that the verb is real but the finding's current
	// state has no use for it.
	ErrUnavailable = errors.New("action is not available in this phase")
	// ErrUnknownVerb reports a verb outside ActionVerbs.
	ErrUnknownVerb = errors.New("unknown action verb")
)

// Apply records verb on f as user at time now, mutating f's spec only — a
// human action never writes status and never moves a phase itself. The
// phase-owning controller observes the spec change and performs the transition,
// which is what keeps every phase edge single-writer.
//
// changed reports whether f was actually modified. An action whose effect is
// already recorded returns (false, nil): callers skip the write rather than
// churning resourceVersion. note is recorded on approvals and ignored by every
// other verb.
//
// Callers running under conflict retry must call Apply again after every
// re-Get, so the gating is re-evaluated against fresh state.
func Apply(f *v1alpha1.Finding, verb, user, note string, now time.Time) (changed bool, err error) {
	return ApplyWithID(f, verb, user, 0, note, now)
}

// ApplyWithID is Apply for an action a GitHub account took (a /patchy
// command on the tracking issue): the approval, retry or expedite request
// also records the account's numeric id (byID), the identity run
// attribution keys on, since a login can be renamed and reused. id <= 0
// records none, as Apply does for the status page and the CLI, whose
// usernames are not GitHub accounts.
func ApplyWithID(f *v1alpha1.Finding, verb, user string, id int64, note string, now time.Time) (changed bool,
	err error) {
	id = max(id, 0)
	switch verb {
	case VerbSuspend:
		if f.Spec.Suspend {
			return false, nil
		}
		// A terminal finding has no progress left to pause.
		if v1alpha1.Terminal(f.Status.Phase) {
			return false, ErrUnavailable
		}
		f.Spec.Suspend = true

	case VerbResume:
		if !f.Spec.Suspend {
			return false, nil
		}
		f.Spec.Suspend = false

	case VerbApprove:
		return applyApprove(f, user, id, note, now)

	case VerbRetry:
		if v1alpha1.RetryTarget(f) == "" {
			return false, ErrUnavailable // not Failed, or no recoverable history
		}
		if v1alpha1.RetryRequested(f) {
			return false, nil // a fresh retry is already pending consumption
		}
		f.Spec.Retry = &v1alpha1.ActionRequest{By: user, ByID: id, At: metav1.NewTime(now)}

	case VerbExpedite:
		if f.Spec.Expedite != nil {
			return false, nil // expedite is standing; first request wins
		}
		if !expeditable(f.Status.Phase) {
			return false, ErrUnavailable
		}
		f.Spec.Expedite = &v1alpha1.ActionRequest{By: user, ByID: id, At: metav1.NewTime(now)}

	default:
		return false, ErrUnknownVerb
	}
	return true, nil
}

// Available reports the verbs that would actually do something to f at time
// now, in ActionVerbs order. It probes Apply against a copy rather than
// re-stating the gates, so the two can never disagree.
//
// It answers "is there anything to do here", not "may this user do it" — a
// caller still has to check its own grants.
func Available(f *v1alpha1.Finding, now time.Time) []string {
	var out []string
	for _, verb := range ActionVerbs {
		probe := f.DeepCopy()
		if changed, err := Apply(probe, verb, "", "", now); err == nil && changed {
			out = append(out, verb)
		}
	}
	return out
}

// applyApprove is ApplyWithID's approve: admitted while the finding awaits
// approval or is handed off, and while its remediation is held for its
// approver's model credential.
func applyApprove(f *v1alpha1.Finding, user string, id int64, note string, now time.Time) (bool, error) {
	phase := f.Status.Phase
	if credentialHeld(f) {
		// Held for the approver's model credential (per-user credentials,
		// enforce): a later approval replaces whom the next attempt is
		// for, so someone holding a valid credential can let it run. The
		// same approver again changes nothing.
		if ap := f.Spec.Approval; ap != nil && ap.By == user && ap.ByID == id {
			return false, nil
		}
		f.Spec.Approval = &v1alpha1.Approval{By: user, ByID: id, At: metav1.NewTime(now), Note: note}
		return true, nil
	}
	if phase != v1alpha1.PhaseAwaitingApproval && phase != v1alpha1.PhaseHandedOff {
		return false, ErrUnavailable
	}
	// First approval wins — except a HandedOff finding whose recorded
	// approval predates completion: that approval can never revive it
	// (the spawner requires approval newer than completedAt), so a fresh
	// one replaces it.
	if f.Spec.Approval != nil && !staleApproval(f) {
		return false, nil
	}
	f.Spec.Approval = &v1alpha1.Approval{By: user, ByID: id, At: metav1.NewTime(now), Note: note}
	return true, nil
}

// credentialHeld reports a Queued finding whose next remediation attempt is
// held for its approver's model credential (CredentialRequired True, set by
// remediation-controller's spawner only under per-user credentials enforce).
func credentialHeld(f *v1alpha1.Finding) bool {
	return f.Status.Phase == v1alpha1.PhaseQueued &&
		meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ConditionCredentialRequired)
}

// expeditable reports the phases where an expedite still has waiting stages
// ahead of it to skip: everything up to and including Queued. A run already in
// flight (Remediating) or a PR under review gains nothing from it.
func expeditable(p v1alpha1.Phase) bool {
	switch p {
	case v1alpha1.PhaseOpened, v1alpha1.PhaseEnhanced, v1alpha1.PhaseInvestigating,
		v1alpha1.PhaseAwaitingApproval, v1alpha1.PhaseQueued:
		return true
	}
	return false
}

// staleApproval reports a HandedOff finding whose approval is too old to revive
// it: remediation-controller's revival gate requires the approval to be newer
// than status.completedAt.
func staleApproval(f *v1alpha1.Finding) bool {
	if f.Status.Phase != v1alpha1.PhaseHandedOff || f.Spec.Approval == nil {
		return false
	}
	done := f.Status.CompletedAt
	return done != nil && !f.Spec.Approval.At.After(done.Time)
}
