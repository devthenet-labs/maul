// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ApplyStatus writes what a verified ledger shows onto st, the same way for
// every host: the head's replay and confirmation runs and the checks it
// confirmed for spec's outcomes. It returns the ledger's summary (count,
// sha256 and the index of every entry); the host names the ConfigMaps it
// wrote (and the builds, for a workspace) on it and sets st.Ledger.
func ApplyStatus(st *v1alpha1.PreviewVerificationStatus, spec *v1alpha1.PreviewVerificationSpec,
	out Output) *v1alpha1.LedgerSummary {
	entries := out.Ledger.Entries
	st.Replay = ReplayRuns(entries)
	st.Confirmations = ConfirmationRuns(entries)
	st.Confirmed = ConfirmedChecks(entries, spec.Outcomes)
	return &v1alpha1.LedgerSummary{Count: ClampInt32(out.Ledger.Summary.Count), SHA256: out.Ledger.Summary.SHA256,
		Entries: Index(entries)}
}

// EvaluatorCalled reports an evaluator's call in the ledger: one of its own
// tool calls, or agent-runner's after it exited (confirm, finish). The
// relay's entries alone (readiness, builds, replay, a deadline finish) are a
// round no evaluator ran in.
func EvaluatorCalled(entries []toolwire.LedgerEntry) bool {
	return slices.ContainsFunc(entries, func(e toolwire.LedgerEntry) bool {
		return e.Kind == toolwire.KindAgent || e.Kind == toolwire.KindRunner
	})
}

// RoundReason is why a verified ledger, otherwise observed, does not show
// its round: a full round no evaluator called is Infra evaluator-not-run,
// or tools-not-ready when the evaluator was never told where the tools were
// (haveTools false); a replay round its relay finished before its replay's
// end (replayFinished false: previewtools.ReplayFinished, which this
// package cannot import) may have left a saved check unrun at these heads
// (review VC-3), so it is replay-incomplete, never judged. "" when the
// ledger shows the round.
func RoundReason(rung v1alpha1.VerificationRung, entries []toolwire.LedgerEntry, haveTools,
	replayFinished bool) v1alpha1.PreviewVerificationReason {
	switch {
	case rung == v1alpha1.RungFull && !EvaluatorCalled(entries) && !haveTools:
		return v1alpha1.VerificationReasonToolsNotReady
	case rung == v1alpha1.RungFull && !EvaluatorCalled(entries):
		return v1alpha1.VerificationReasonEvaluatorNotRun
	case rung == v1alpha1.RungReplay && !replayFinished:
		return v1alpha1.VerificationReasonReplayIncomplete
	}
	return ""
}
