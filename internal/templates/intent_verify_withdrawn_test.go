// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// TestVerifyLineStatesTheWithdrawal: an intent whose earlier result the
// upgrade withdrew (VerifyProgress.Withdrawn) says so on every progress line
// that carries a verification's state, whatever the state, and says nothing
// of it otherwise. The sentence never says maul checked or confirmed
// anything.
func TestVerifyLineStatesTheWithdrawal(t *testing.T) {
	if bad := overstates(WithdrawnSentence); len(bad) > 0 {
		t.Fatalf("WithdrawnSentence says %q: %q", bad, WithdrawnSentence)
	}
	for _, state := range []string{VerifyWaiting, VerifyWaitingForCI, VerifyEvaluating, VerifyUnconfirmed,
		VerifyPassed, VerifyFailing, VerifyExhausted, VerifyUnavailable, VerifyPreviewNotLive, VerifyHeadsMoved} {
		p := VerifyProgress{State: state, Verification: 3, FixesUsed: 0, MaxFixes: 2, Passing: 1, Total: 3,
			Outcomes: true}
		if got := verifyLine(p); strings.Contains(got, WithdrawnSentence) {
			t.Errorf("%s: the withdrawal stated without it: %q", state, got)
		}
		p.Withdrawn = true
		got := verifyLine(p)
		if !strings.Contains(got, WithdrawnSentence) {
			t.Errorf("%s: the withdrawal is not stated: %q", state, got)
		}
		if !strings.HasSuffix(got, WithdrawnSentence) {
			t.Errorf("%s: the withdrawal is not the line's last sentence: %q", state, got)
		}
	}
}
