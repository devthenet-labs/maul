// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance_test

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestEmptyListDigest pins v1alpha1.EmptyListDigest, which the Intent
// schema repeats as a literal and intent-controller keys criteria-era
// records on, to both of this package's digests of the empty list.
func TestEmptyListDigest(t *testing.T) {
	for name, got := range map[string]string{
		"Digest(nil)":                              acceptance.Digest(nil),
		"Digest([]Criterion{})":                    acceptance.Digest([]acceptance.Criterion{}),
		"OutcomesDigest(nil)":                      acceptance.OutcomesDigest(nil),
		"OutcomesDigest([]PlanOutcome{})":          acceptance.OutcomesDigest([]acceptance.PlanOutcome{}),
		"OutcomesDigest(NormalizeOutcomes(empty))": acceptance.OutcomesDigest(acceptance.NormalizeOutcomes(nil)),
	} {
		if got != v1alpha1.EmptyListDigest {
			t.Errorf("%s = %s, want v1alpha1.EmptyListDigest %s", name, got, v1alpha1.EmptyListDigest)
		}
	}
}
