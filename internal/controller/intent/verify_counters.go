// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/runnerguard"
)

// OutcomeSupersededByHuman is the outcome of an acceptance-fix run still
// Pending when an approver's review or command arrived: it is cancelled
// before its Job is created, so the human's round runs instead, and it
// spends no verify-fix round.
const OutcomeSupersededByHuman = "superseded_by_human"

// OutcomeStoppedByHuman is the outcome of an acceptance-fix run still
// Pending when an approver's /maul verify stop arrived: it is cancelled
// before its Job is created, nothing runs in its place, and it spends no
// verify-fix round.
const OutcomeStoppedByHuman = "stopped_by_human"

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

// agentRan reports whether run's agent executed: the one rule for "an
// attempt where an agent tried a fix", which verifyFixRounds and
// priorVerifyFailure both read through fixTried. Every outcome a run
// records is listed (agent_ran_test.go holds the list to every outcome
// constant). It fails safe for maxVerifyFixes: an outcome recorded both
// after an agent ran and before is counted as ran, so a round whose agent
// keeps crashing is bounded. runtime_error is one (a CLI that exited
// non-zero after a full session), and so is aborted (an OOM kill, the
// deadline, a success without a changeset), as uncounted counts both as
// attempts. Only the outcomes that are never an agent's are not: an image
// that cannot run the agent, a refused launch or input, no node to run on,
// a human cancelling it before its Job, and a run the sandbox probe
// refused. A head move is decided by whether the Job was created (at push,
// the agent ran).
func agentRan(run *v1alpha1.IntentRun) bool {
	if runnerguard.Refused(run.Status.Conditions) {
		return false
	}
	switch run.Status.Outcome {
	case string(envelope.OutcomeOK), OutcomeNoDefect, string(envelope.OutcomeCommitFailed), OutcomeNotBuilt,
		string(envelope.OutcomeReportInvalid), string(envelope.OutcomeReportMissing),
		string(envelope.OutcomeTimeout), string(envelope.OutcomeBudgetExceeded),
		string(envelope.OutcomeChangesetTooLarge), string(envelope.OutcomeChangesetRejected),
		OutcomeDependencyRefused, OutcomeBranchExists, OutcomeHoldExpired, OutcomePushRefused, OutcomeEvicted,
		string(envelope.OutcomeRuntimeError), OutcomeAborted:
		return true
	case OutcomeHeadMoved:
		return run.Status.JobRef != nil
	case "", string(envelope.OutcomeImageIncompatible), OutcomeImageRequired, OutcomeLaunchRefused,
		OutcomeInputUnavailable, OutcomeNoUsableFeedback,
		OutcomeUnschedulable, OutcomeSupersededByHuman, OutcomeStoppedByHuman:
		return false
	}
	return false
}

// fixTried reports a round in which an agent tried a fix: some attempt's
// agent ran (agentRan), whatever became of its later attempts.
func (rs roundRuns) fixTried() bool {
	return slices.ContainsFunc(rs, agentRan)
}

// finished reports a round whose latest attempt ended.
func (rs roundRuns) finished() bool {
	latest := rs.latest()
	return latest != nil && (latest.Status.Phase == v1alpha1.RunComplete || latest.Status.Phase == v1alpha1.RunFailed)
}

// verifyFixRounds counts the acceptance-fix rounds held against the
// Project's maxVerifyFixes: the distinct verify rounds of the approved plan
// revision in which an agent tried a fix (fixTried), and the one in
// flight, so a new approved plan starts the count again. A round no agent
// ran in, every attempt cancelled, refused or failed before it started, is
// not counted; one whose first attempt ran is, however its retry ended,
// cancelled by a human included. status.verifyFixes, which counts
// completed rounds only, is a display mirror and never this count.
func (p *pass) verifyFixRounds() int32 {
	ap := p.in.Status.Approval
	if ap == nil {
		return 0
	}
	seen := map[int32]bool{}
	for _, run := range p.runs {
		if run.Spec.Stage != v1alpha1.IntentStageRevise || roundKindOf(run.Spec.Trigger) != roundVerify ||
			run.Spec.Inputs.PlanRevision != ap.PlanRevision || seen[run.Spec.Round] {
			continue
		}
		rs := p.round(v1alpha1.IntentStageRevise, run.Spec.Round, anyRepository)
		if !rs.finished() || rs.fixTried() {
			seen[run.Spec.Round] = true
		}
	}
	return int32(len(seen))
}
