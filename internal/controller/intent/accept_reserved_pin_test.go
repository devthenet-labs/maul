// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/command"
)

// TestReservedOutcomeIDsAreTheAcceptWords: the outcome ids the plan grammar
// refuses are exactly the /maul accept arguments that name something other
// than one outcome, so a plan can declare no outcome an approver could not
// accept by its own name, and refuses no other id.
func TestReservedOutcomeIDsAreTheAcceptWords(t *testing.T) {
	want := []string{command.AcceptAll}
	got := slices.Sorted(slices.Values(acceptance.ReservedOutcomeIDs))
	if !slices.Equal(got, want) {
		t.Fatalf("acceptance.ReservedOutcomeIDs = %v, want %v", got, want)
	}
	for _, id := range want {
		if err := acceptance.ValidateOutcomes([]acceptance.PlanOutcome{{ID: id, Outcome: "x"}}); err == nil {
			t.Errorf("an outcome of id %q is admitted", id)
		}
	}
}
