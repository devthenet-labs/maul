// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"

// OutcomeSupersededByHuman is the outcome of an acceptance-fix run still
// Pending when an approver's review or command arrived: it is cancelled
// before its Job is created, so the human's round runs instead, and it
// spends no verify-fix round.
const OutcomeSupersededByHuman = "superseded_by_human"

// roundKind is what a revise round is for, by its trigger: each counts
// against its own limit, and each is told and reported differently.
type roundKind int

const (
	// roundHuman: an approver asked (a review, or /maul revise or retry).
	roundHuman roundKind = iota
	// roundChecks: a named check failed at maul's own head.
	roundChecks
	// roundVerify: the acceptance verification failed at maul's own heads.
	roundVerify
)

// roundKindOf classifies a revise round's trigger. Every trigger the
// schema admits has its own case (trigger_exhaustive_test.go). Only a
// revise run carries a trigger (CEL), so the default is a run with none,
// which only a test fixture or a build or plan run is; it reads as an
// approver's round, as every round did before triggers were recorded.
func roundKindOf(t v1alpha1.IntentRunTrigger) roundKind {
	switch t {
	case v1alpha1.IntentRunTriggerReview, v1alpha1.IntentRunTriggerCommand:
		return roundHuman
	case v1alpha1.IntentRunTriggerChecks:
		return roundChecks
	case v1alpha1.IntentRunTriggerVerify:
		return roundVerify
	default:
		return roundHuman
	}
}

// verifyFixRounds counts the acceptance-fix rounds held against the
// Project's maxVerifyFixes: the distinct verify rounds of the approved plan
// revision, a failed one included, so a new approved plan starts the count
// again. A round whose latest attempt a human's review superseded before it
// launched (OutcomeSupersededByHuman) is not counted: it never ran.
// status.verifyFixes, which counts completed rounds only, is a display
// mirror and never this count.
func (p *pass) verifyFixRounds() int32 {
	ap := p.in.Status.Approval
	if ap == nil {
		return 0
	}
	seen := map[int32]bool{}
	for _, run := range p.runs {
		if run.Spec.Stage != v1alpha1.IntentStageRevise || roundKindOf(run.Spec.Trigger) != roundVerify ||
			run.Spec.Inputs.PlanRevision != ap.PlanRevision {
			continue
		}
		if latest := p.round(v1alpha1.IntentStageRevise, run.Spec.Round, anyRepository).latest(); latest != nil &&
			latest.Status.Phase == v1alpha1.RunFailed && latest.Status.Outcome == OutcomeSupersededByHuman {
			continue
		}
		seen[run.Spec.Round] = true
	}
	return int32(len(seen))
}
