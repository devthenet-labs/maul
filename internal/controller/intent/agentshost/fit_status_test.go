// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// overCap is an agents verification settled in phase for reason whose
// status is over maxObject even once its log tails go: a full ledger
// index passes it, padded by an annotation that fits alone.
func overCap(phase v1alpha1.PreviewVerificationPhase,
	reason v1alpha1.PreviewVerificationReason) *v1alpha1.PreviewVerification {
	pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: "cli-1-v1-a1",
		Annotations: map[string]string{"pad": strings.Repeat("p", maxObject-(200<<10))}},
		Spec: v1alpha1.PreviewVerificationSpec{Placement: v1alpha1.PlacementAgents}}
	pv.Status = v1alpha1.PreviewVerificationStatus{Phase: phase, Reason: reason,
		Ledger: &v1alpha1.LedgerSummary{Count: 512, SHA256: strings.Repeat("a", 64), Entries: fullIndex()},
		Environment: []v1alpha1.PartFact{{Name: "build", Role: v1alpha1.PreviewRoleBuild,
			LogTail: strings.Repeat("x", 400<<10)}}}
	return pv
}

// fullIndex is a ledger index at its bounds: 512 entries of 8 artifacts.
func fullIndex() []v1alpha1.LedgerIndexEntry {
	shas := make([]string, 8)
	for i := range shas {
		shas[i] = fmt.Sprintf("%064x", i)
	}
	out := make([]v1alpha1.LedgerIndexEntry, 512)
	for i := range out {
		out[i] = v1alpha1.LedgerIndexEntry{Seq: int32(i + 1), Kind: v1alpha1.LedgerKindAgent,
			Verb: v1alpha1.LedgerVerbExec, ResultSHA256: strings.Repeat("b", 64), Artifacts: shas}
	}
	return out
}

// TestFitStatusKeepsASettledPhase: an agents verification over its cap
// once the log tails go loses its ledger fields, and only an Observed one,
// whose verdict rests on them, becomes maul's Infra ledger-mismatch; any
// other phase keeps its own reason, so a head that did not build stays
// tools-build-failed (Unconfirmed for the build, never Unavailable) and a
// Stale round stays stale.
func TestFitStatusKeepsASettledPhase(t *testing.T) {
	for _, tc := range []struct {
		phase      v1alpha1.PreviewVerificationPhase
		reason     v1alpha1.PreviewVerificationReason
		wantPhase  v1alpha1.PreviewVerificationPhase
		wantReason v1alpha1.PreviewVerificationReason
	}{
		{v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK,
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonLedgerMismatch},
		{v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsBuildFailed,
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsBuildFailed},
		{v1alpha1.VerificationInfra, v1alpha1.VerificationReasonSandboxUnenforced,
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonSandboxUnenforced},
		{v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsDeadline,
			v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsDeadline},
		{v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleApproval,
			v1alpha1.VerificationStale, v1alpha1.VerificationReasonStaleApproval},
	} {
		t.Run(string(tc.phase)+"/"+string(tc.reason), func(t *testing.T) {
			pv := overCap(tc.phase, tc.reason)
			fitStatus(pv)
			if pv.Status.Phase != tc.wantPhase || pv.Status.Reason != tc.wantReason {
				t.Fatalf("phase %s %s, want %s %s", pv.Status.Phase, pv.Status.Reason, tc.wantPhase, tc.wantReason)
			}
			if pv.Status.Ledger != nil {
				t.Error("the ledger fields were kept over the cap")
			}
			if n := jsonSize(pv); n > maxObject {
				t.Errorf("status is %d bytes, over %d", n, maxObject)
			}
		})
	}
	// Under the cap once tails go, from the last part back, nothing else
	// is dropped.
	pv := overCap(v1alpha1.VerificationObserved, v1alpha1.VerificationReasonOK)
	pv.Annotations = nil
	tail := strings.Repeat("x", 400<<10)
	pv.Status.Environment = []v1alpha1.PartFact{{Name: "build", LogTail: tail}, {Name: "tools", LogTail: tail},
		{Name: "sandbox-head", LogTail: tail}, {Name: "sandbox-base", LogTail: tail}}
	fitStatus(pv)
	if pv.Status.Phase != v1alpha1.VerificationObserved || pv.Status.Ledger == nil ||
		pv.Status.Environment[0].LogTail == "" || pv.Status.Environment[3].LogTail != "" {
		t.Errorf("phase %s, ledger %v: want the last tails dropped alone", pv.Status.Phase, pv.Status.Ledger)
	}
}
