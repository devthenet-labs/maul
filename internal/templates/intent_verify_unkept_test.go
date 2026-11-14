// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestOutcomeVerdictUnkept (G3-B-CLI-1): a red check maul re-ran of an
// outcome it then did not keep is named on the outcome's row with maul's
// reason, so the issue never says only that no check showed it; an unknown
// reason, or a confirmed or accepted outcome, adds nothing.
func TestOutcomeVerdictUnkept(t *testing.T) {
	for _, tt := range []struct {
		name   string
		o      VerifyOutcomeResult
		want   string
		absent bool
	}{
		{"the slot's run-data rule", VerifyOutcomeResult{Result: OutcomeResultUnsure,
			Reason: acceptance.ReasonModelOnlyFail, Unkept: UnkeptRunData},
			"maul re-ran a check of it red but did not keep it: in a preview, a check that shows a defect must " +
				"write data of its own run and expect only success statuses", false},
		{"a confirmation not red by its assertion", VerifyOutcomeResult{Result: OutcomeResultUnsure,
			Reason: acceptance.ReasonModelOnlyFail, Unkept: UnkeptConfirmation},
			"did not keep it: not every run of it was red by the assertion it was proposed failing", false},
		{"the saved-check rules", VerifyOutcomeResult{Result: OutcomeResultUnsure, Unkept: UnkeptRules},
			"did not keep it: it breaks the saved-check rules", false},
		{"an unknown reason", VerifyOutcomeResult{Result: OutcomeResultUnsure, Unkept: "<b>x</b>"}, "keep", true},
		{"a confirmed outcome", VerifyOutcomeResult{Result: OutcomeResultPassed, Unkept: UnkeptRunData}, "keep",
			true},
		{"an accepted outcome", VerifyOutcomeResult{Result: OutcomeResultAccepted, Unkept: UnkeptRunData}, "keep",
			true},
	} {
		got := outcomeVerdict(tt.o, "unsure")
		if strings.Contains(got, tt.want) == tt.absent {
			t.Errorf("%s: verdict = %q, want %q present %v", tt.name, got, tt.want, !tt.absent)
		}
	}
}
