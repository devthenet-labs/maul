// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestOutcomeVerdictUnkept (G3-B-CLI-1): a check proposed to show an
// outcome failing that maul did not keep is named on the outcome's row with
// maul's reason, so the issue never says only that no check showed it, and
// never that the check ran red, which a refused one may not have; an
// unknown reason, or a confirmed or accepted outcome, adds nothing.
func TestOutcomeVerdictUnkept(t *testing.T) {
	for _, tt := range []struct {
		name   string
		o      VerifyOutcomeResult
		want   string
		absent bool
	}{
		{"the slot's run-data rule", VerifyOutcomeResult{Result: OutcomeResultUnsure,
			Reason: acceptance.ReasonModelOnlyFail, Unkept: UnkeptRunData},
			"a check proposed to show it failing was not kept: in a preview, a check that shows a defect must " +
				"write data of its own run and expect only success statuses", false},
		{"a confirmation not red by its assertion", VerifyOutcomeResult{Result: OutcomeResultUnsure,
			Reason: acceptance.ReasonModelOnlyFail, Unkept: UnkeptConfirmation},
			"was not kept: maul's own runs of it did not show it failing by the assertion it was proposed " +
				"failing", false},
		{"the saved-check rules", VerifyOutcomeResult{Result: OutcomeResultUnsure, Unkept: UnkeptRules},
			"was not kept: it breaks the saved-check rules", false},
		{"an unknown reason", VerifyOutcomeResult{Result: OutcomeResultUnsure, Unkept: "<b>x</b>"}, "not kept", true},
		{"a confirmed outcome", VerifyOutcomeResult{Result: OutcomeResultPassed, Unkept: UnkeptRunData}, "not kept",
			true},
		{"an accepted outcome", VerifyOutcomeResult{Result: OutcomeResultAccepted, Unkept: UnkeptRunData}, "not kept",
			true},
	} {
		got := outcomeVerdict(tt.o, "unsure")
		if strings.Contains(got, tt.want) == tt.absent {
			t.Errorf("%s: verdict = %q, want %q present %v", tt.name, got, tt.want, !tt.absent)
		}
		if strings.Contains(got, " red") {
			t.Errorf("%s: verdict = %q claims a red run", tt.name, got)
		}
	}
}
