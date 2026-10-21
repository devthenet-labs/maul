// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package priority

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

func TestScore(t *testing.T) {
	cases := []struct {
		name           string
		severity       v1alpha1.Level
		exploitability v1alpha1.Rating
		likelihood     v1alpha1.Rating
		impact         v1alpha1.Rating
		want           int32
	}{
		{"everything critical maxes out", v1alpha1.LevelCritical,
			v1alpha1.RatingCritical, v1alpha1.RatingCritical, v1alpha1.RatingCritical, 100},
		{"everything low bottoms out near zero", v1alpha1.LevelLow,
			v1alpha1.RatingNone, v1alpha1.RatingNone, v1alpha1.RatingNone, 0},
		{"unassessed dimensions score as none", v1alpha1.LevelHigh, "", "", "", 20},
		{"critical severity alone is bounded by its weight", v1alpha1.LevelCritical,
			v1alpha1.RatingNone, v1alpha1.RatingNone, v1alpha1.RatingNone, 30},
		{"exploitable critical outranks unexploitable critical", v1alpha1.LevelCritical,
			v1alpha1.RatingCritical, v1alpha1.RatingNone, v1alpha1.RatingNone, 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Score(c.severity, c.exploitability, c.likelihood, c.impact, DefaultWeights)
			if got != c.want {
				t.Errorf("Score(%v, %v, %v, %v) = %d, want %d",
					c.severity, c.exploitability, c.likelihood, c.impact, got, c.want)
			}
		})
	}
}

func TestScoreOrdering(t *testing.T) {
	low := Score(v1alpha1.LevelMedium, v1alpha1.RatingLow, v1alpha1.RatingLow, v1alpha1.RatingLow, DefaultWeights)
	high := Score(v1alpha1.LevelMedium, v1alpha1.RatingHigh, v1alpha1.RatingHigh, v1alpha1.RatingHigh, DefaultWeights)
	if low >= high {
		t.Errorf("Score(low ratings)=%d >= Score(high ratings)=%d", low, high)
	}
}

func TestScoreZeroWeightsFallBack(t *testing.T) {
	got := Score(v1alpha1.LevelCritical, v1alpha1.RatingCritical, v1alpha1.RatingCritical,
		v1alpha1.RatingCritical, Weights{})
	if got != 100 {
		t.Errorf("Score(zero weights) = %d, want 100 via default weights", got)
	}
}
