// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package crdconv

import (
	"reflect"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TestOutcomesToCRD: every field of a plan outcome lands on the CRD mirror
// as written, an empty list is nil, and a normalised list keeps its kinds.
func TestOutcomesToCRD(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []acceptance.PlanOutcome
		want []v1alpha1.AcceptanceOutcome
	}{
		{name: "nil"},
		{name: "empty", in: []acceptance.PlanOutcome{}},
		{
			name: "every field",
			in: acceptance.NormalizeOutcomes([]acceptance.PlanOutcome{
				{ID: "rows-scroll", Outcome: "the two rows scroll together", From: "- rows scroll together"},
				{ID: "still-loads", Outcome: "the leaderboard still loads", Kind: acceptance.OutcomeKindKeep},
				{ID: "sounds-good", Outcome: "it sounds good", Manual: true, Reason: "only a human hears it"},
			}),
			want: []v1alpha1.AcceptanceOutcome{
				{ID: "rows-scroll", Outcome: "the two rows scroll together", From: "- rows scroll together",
					Kind: v1alpha1.OutcomeKindChange},
				{ID: "still-loads", Outcome: "the leaderboard still loads", Kind: v1alpha1.OutcomeKindKeep},
				{ID: "sounds-good", Outcome: "it sounds good", Kind: v1alpha1.OutcomeKindChange, Manual: true,
					Reason: "only a human hears it"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := OutcomesToCRD(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("OutcomesToCRD = %+v, want %+v", got, tt.want)
			}
		})
	}
}
