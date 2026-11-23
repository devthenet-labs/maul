// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
)

// TestConfirmedOutcomeShowsNoClaim: an outcome maul itself judged (here
// failed on a replayed check) shows maul's own evidence, never the
// evaluator's note.
func TestConfirmedOutcomeShowsNoClaim(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 3)
	for _, c := range e.verifyComments(name, 1) {
		if strings.Contains(c.Body, "The evaluator's claim") || strings.Contains(c.Body, "The evaluator judged") {
			t.Errorf("a failed outcome shows the evaluator's claim:\n%s", c.Body)
		}
	}
}
