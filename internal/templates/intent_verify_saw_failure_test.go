// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestOutcomeDetailSawFailure (S1 demo bug 13): an outcome whose pass maul
// withheld because the evaluator saw it fail says so on the issue, rather
// than the bare "couldn't confirm".
func TestOutcomeDetailSawFailure(t *testing.T) {
	o := VerifyOutcomeResult{ID: "create-round-trip", Result: OutcomeResultUnsure,
		Reason: acceptance.ReasonEvaluatorSawFailure}
	if got := outcomeVerdict(o, "unsure"); !strings.Contains(got, "the evaluator saw the outcome as worded fail") {
		t.Errorf("verdict = %q, want the evaluator's observed failure named", got)
	}
}
