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
	d, ok := outcomeDetailOf(o, "create-round-trip", "unsure")
	if !ok || !strings.Contains(d.Sentence, "the evaluator saw the outcome as worded fail") {
		t.Errorf("detail = %+v, %v, want the evaluator's observed failure named", d, ok)
	}
}
