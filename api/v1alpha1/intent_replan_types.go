// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import "strconv"

// IntentBranch is the branch the pull requests of an Intent's plan revision
// planRevision are opened from. Pushes are create-only and never forced, so
// a plan made by a replan in review cannot reuse the branch of the plan it
// replaced: from the first such plan on (branchesFrom, status.planBranchesFrom,
// zero while the intent was never replanned in review) each plan revision
// gets its own, maul-intent/<intent>-p<revision>. Every other plan keeps
// maul-intent/<intent>, byte for byte, so nothing an intent stored before
// replans in review existed names another branch. No branch of one intent is
// another's: an Intent's name ends in "-<issue number>", and a suffixed
// branch's last segment starts with "p".
func IntentBranch(intent string, planRevision, branchesFrom int32) string {
	if branchesFrom > 0 && planRevision >= branchesFrom {
		return IntentBranchPrefix + intent + "-p" + strconv.FormatInt(int64(planRevision), 10)
	}
	return IntentBranchPrefix + intent
}

// IntentReplan is a replan accepted while the intent was in review (InReview
// or Revising, or Blocked from either), with --intent-replan-in-review on.
// It is recorded before anything of it is done, and every step it takes
// (stopping the round, withdrawing maul/acceptance, one comment on each open
// pull request and its close, then Planning) is idempotent, so a pass that
// finds it set resumes it, whatever step a restart interrupted. Nothing of
// review runs while it is set: maul's own closes are never read as a human's.
type IntentReplan struct {
	// Trigger is the replan: the trigger label re-applied, or /maul replan,
	// by an approver. The new plan's runs are attributed to it.
	Trigger IntentAction `json:"trigger"`
	// PlanRevision is the approved plan revision the replan supersedes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=999
	PlanRevision int32 `json:"planRevision"`
}
