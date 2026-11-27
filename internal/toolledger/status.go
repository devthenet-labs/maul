// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"slices"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ApplyStatus returns a verified ledger's summary (count, sha256 and the
// index of every entry), the same way for every host; the host names the
// ConfigMaps it wrote (and the builds, for a workspace) on it and sets it as
// the verification's status.ledger.
func ApplyStatus(out Output) *v1alpha1.LedgerSummary {
	return &v1alpha1.LedgerSummary{Count: ClampInt32(out.Ledger.Summary.Count), SHA256: out.Ledger.Summary.SHA256,
		Entries: Index(out.Ledger.Entries)}
}

// EvaluatorCalled reports an evaluator's call in the ledger: one of its own
// tool calls, or agent-runner's after it exited (its finish). The relay's
// entries alone (readiness, builds, its own finish) are a round no evaluator
// ran in.
func EvaluatorCalled(entries []toolwire.LedgerEntry) bool {
	return slices.ContainsFunc(entries, func(e toolwire.LedgerEntry) bool {
		return e.Kind == toolwire.KindAgent || e.Kind == toolwire.KindRunner
	})
}

// RoundReason is why a verified ledger, otherwise observed, does not show
// its round: a round no evaluator called is Infra evaluator-not-run, or
// tools-not-ready when the evaluator was never told where the tools were
// (haveTools false). Every round is guarded alike, whatever rung its
// verification was written with: no round is judged on the relay's entries
// alone. "" when the ledger shows the round.
func RoundReason(entries []toolwire.LedgerEntry, haveTools bool) v1alpha1.PreviewVerificationReason {
	switch {
	case EvaluatorCalled(entries):
		return ""
	case !haveTools:
		return v1alpha1.VerificationReasonToolsNotReady
	}
	return v1alpha1.VerificationReasonEvaluatorNotRun
}
