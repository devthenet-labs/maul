// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ApplyStatus returns the ledger's summary and index and leaves none of the
// retired check runs (replay, confirmations, confirmed checks) on status,
// whatever an earlier write left there.
func TestApplyStatus(t *testing.T) {
	log, msg := toolLedger(t, testPNG(t, 8, 8))
	out := verifiedLedger(t, log, msg)
	run := []v1alpha1.CheckRun{{CheckDigest: "sha256:" + sha256Hex(nil), Result: v1alpha1.CheckRunPass}}
	st := v1alpha1.PreviewVerificationStatus{Replay: run, Confirmations: run,
		Confirmed: []v1alpha1.SavedCheck{{ID: "items.body"}}}
	sum := ApplyStatus(&st, out)
	entries := out.Ledger.Entries
	want := &v1alpha1.LedgerSummary{Count: 7, SHA256: out.Ledger.Summary.SHA256, Entries: Index(entries)}
	if !reflect.DeepEqual(sum, want) {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
	if st.Ledger != nil {
		t.Error("ApplyStatus set st.Ledger; the host names its ConfigMaps first")
	}
	if st.Replay != nil || st.Confirmations != nil || st.Confirmed != nil || st.Base != nil {
		t.Errorf("status = replay %+v, confirmations %+v, confirmed %+v, base %+v; want none: only the index "+
			"is written", st.Replay, st.Confirmations, st.Confirmed, st.Base)
	}
}

// TestRoundReason: a round with no agent or runner entry is evaluator-not-run,
// or tools-not-ready when the evaluator was never told where the tools
// were, whatever else the relay recorded: the ledger of a round an earlier
// release ran as a replay (the saved checks' runs and the relay's own
// "replay-only" finish) included.
func TestRoundReason(t *testing.T) {
	agent := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay}, {Kind: toolwire.KindAgent}}
	runner := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay}, {Kind: toolwire.KindRunner}}
	relay := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay}, {Kind: toolwire.KindRelay}}
	replay := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay, Verb: toolwire.VerbHTTP},
		{Kind: toolwire.KindRelay, Verb: toolwire.VerbCheckRun},
		{Kind: toolwire.KindRelay, Verb: toolwire.VerbFinish, Args: json.RawMessage(`{"reason":"replay-only"}`)}}
	for _, tc := range []struct {
		name      string
		entries   []toolwire.LedgerEntry
		haveTools bool
		want      v1alpha1.PreviewVerificationReason
	}{
		{"the evaluator called", agent, true, ""},
		{"the runner after it", runner, true, ""},
		{"the relay alone", relay, true, v1alpha1.VerificationReasonEvaluatorNotRun},
		{"never told where the tools were", relay, false, v1alpha1.VerificationReasonToolsNotReady},
		{"the evaluator called though tools were never written", agent, false, ""},
		{"no ledger", nil, true, v1alpha1.VerificationReasonEvaluatorNotRun},
		{"no ledger, never told where the tools were", nil, false, v1alpha1.VerificationReasonToolsNotReady},
		{"a replay the relay finished", replay, true, v1alpha1.VerificationReasonEvaluatorNotRun},
		{"a replay, never told where the tools were", replay, false, v1alpha1.VerificationReasonToolsNotReady},
		{"a replay and the evaluator's call", append(slices.Clone(replay), toolwire.LedgerEntry{
			Kind: toolwire.KindAgent}), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoundReason(tc.entries, tc.haveTools); got != tc.want {
				t.Errorf("RoundReason = %q, want %q", got, tc.want)
			}
		})
	}
}
