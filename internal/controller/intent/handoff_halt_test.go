// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestGivingUpReleasesTheLinkInTheHaltsWrite: a halt by which maul gives up
// on a failing judgement (no fix left, the same failure again, the cost
// ceiling) releases the held preview link in its own write, as a judgement
// does (releaseIfDue), never a pass later. TestOutcomeVerifyFixNoDefect
// and TestCostCeilingEndsVerifyRetry hold the halts written as a round
// settles.
func TestGivingUpReleasesTheLinkInTheHaltsWrite(t *testing.T) {
	for _, tc := range []struct {
		reason v1alpha1.AutoFixReason
		state  v1alpha1.IntentVerificationState
	}{
		{v1alpha1.AutoFixMaxVerifyFixes, v1alpha1.VerificationExhausted},
		{v1alpha1.AutoFixRepeatedFailure, ""},
		{v1alpha1.AutoFixCostCeiling, ""},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			e, name, _ := failingEvaluation(t)
			if h := e.verification(name).Handoff; h == nil || h.Released {
				t.Fatalf("hand-off = %+v, want the link held while Failing", h)
			}
			if err := e.prVerifyPass(name).haltFix(context.Background(), tc.reason, tc.state, "halted"); err != nil {
				t.Fatal(err)
			}
			v := e.verification(name)
			if v.AutoFix.Reason != tc.reason {
				t.Fatalf("auto-fix = %+v, want halted %s", v.AutoFix, tc.reason)
			}
			checkReleasedUnconfirmed(t, v)
		})
	}
}

// checkReleasedUnconfirmed fails unless v's held link was released because
// maul gave up (unconfirmed).
func checkReleasedUnconfirmed(t *testing.T, v *v1alpha1.IntentVerification) {
	t.Helper()
	if h := v.Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonUnconfirmed {
		t.Errorf("hand-off = %+v, want released unconfirmed in the write that gave up", h)
	}
}
