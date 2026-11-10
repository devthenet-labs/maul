// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolledger

import (
	"reflect"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ApplyStatus writes the ledger's runs and confirmed checks onto status and
// returns its summary, exactly as the readers it gathers show them.
func TestApplyStatus(t *testing.T) {
	log, msg := toolLedger(t, testPNG(t, 8, 8))
	out := verifiedLedger(t, log, msg)
	spec := &v1alpha1.PreviewVerificationSpec{Outcomes: testOutcomes()}
	var st v1alpha1.PreviewVerificationStatus
	sum := ApplyStatus(&st, spec, out)
	entries := out.Ledger.Entries
	want := &v1alpha1.LedgerSummary{Count: 7, SHA256: out.Ledger.Summary.SHA256, Entries: Index(entries)}
	if !reflect.DeepEqual(sum, want) {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
	if st.Ledger != nil {
		t.Error("ApplyStatus set st.Ledger; the host names its ConfigMaps first")
	}
	if !reflect.DeepEqual(st.Replay, ReplayRuns(entries)) || len(st.Replay) != 1 ||
		!reflect.DeepEqual(st.Confirmations, ConfirmationRuns(entries)) || len(st.Confirmations) != 1 ||
		!reflect.DeepEqual(st.Confirmed, ConfirmedChecks(entries, spec.Outcomes)) || len(st.Confirmed) != 1 {
		t.Errorf("status = replay %+v, confirmations %+v, confirmed %+v", st.Replay, st.Confirmations, st.Confirmed)
	}
}

func TestRoundReason(t *testing.T) {
	agent := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay}, {Kind: toolwire.KindAgent}}
	runner := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay}, {Kind: toolwire.KindRunner}}
	relay := []toolwire.LedgerEntry{{Kind: toolwire.KindRelay}, {Kind: toolwire.KindRelay}}
	for _, tc := range []struct {
		name                      string
		rung                      v1alpha1.VerificationRung
		entries                   []toolwire.LedgerEntry
		haveTools, replayFinished bool
		want                      v1alpha1.PreviewVerificationReason
	}{
		{"full, the evaluator called", v1alpha1.RungFull, agent, true, false, ""},
		{"full, the runner after it", v1alpha1.RungFull, runner, true, false, ""},
		{"full, the relay alone", v1alpha1.RungFull, relay, true, false,
			v1alpha1.VerificationReasonEvaluatorNotRun},
		{"full, never told where the tools were", v1alpha1.RungFull, relay, false, false,
			v1alpha1.VerificationReasonToolsNotReady},
		{"full, no ledger", v1alpha1.RungFull, nil, true, true, v1alpha1.VerificationReasonEvaluatorNotRun},
		{"replay finished", v1alpha1.RungReplay, relay, false, true, ""},
		{"replay cut short", v1alpha1.RungReplay, relay, true, false, v1alpha1.VerificationReasonReplayIncomplete},
		{"replay cut short, an agent's call aside", v1alpha1.RungReplay, agent, true, false,
			v1alpha1.VerificationReasonReplayIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoundReason(tc.rung, tc.entries, tc.haveTools, tc.replayFinished); got != tc.want {
				t.Errorf("RoundReason = %q, want %q", got, tc.want)
			}
		})
	}
}
