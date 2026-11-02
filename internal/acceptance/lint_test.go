// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestLintOutcomes(t *testing.T) {
	tests := []struct {
		name string
		o    PlanOutcome
		want []OutcomeFlag
	}{
		{"own data", PlanOutcome{ID: "a", Outcome: "A beat you save at 90 BPM shows 90 bpm (2.7 s) on its page.",
			From: "show the beat's length"}, nil},
		{"every", PlanOutcome{ID: "a", Outcome: "The leaderboard lists every player."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"each, any case", PlanOutcome{ID: "a", Outcome: "EACH beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "each"}}},
		{"all", PlanOutcome{ID: "a", Outcome: "All battles show a winner."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "all"}}},
		{"other", PlanOutcome{ID: "a", Outcome: "Voting does not move the other beats."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "other"}}},
		{"others", PlanOutcome{ID: "a", Outcome: "Mine is listed above the others."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "others"}}},
		{"existing", PlanOutcome{ID: "a", Outcome: "Existing beats keep their names."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "existing"}}},
		{"still works", PlanOutcome{ID: "a", Outcome: "The sequencer still   works.", Kind: OutcomeKindKeep},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "still works"}}},
		{"the first one only", PlanOutcome{ID: "a", Outcome: "Every beat and each battle and all players."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		// Inside a word is not the word.
		{"inside words", PlanOutcome{ID: "a", Outcome: "Eachother, overall, small, mothers and allow are fine."},
			nil},
		{"narrowed from", PlanOutcome{ID: "a", Outcome: "A beat you create links to its own page.",
			From: "Each beat in Fresh beats links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintNarrowedFrom, Word: "each"}}},
		{"from and outcome both quantify", PlanOutcome{ID: "a", Outcome: "Every beat links to its page.",
			From: "Every beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"from other is not narrowing", PlanOutcome{ID: "a", Outcome: "A beat you create links to its page.",
			From: "the other beats link too"}, nil},
		{"manual is a person's", PlanOutcome{ID: "a", Outcome: "Every drum sounds good.", Manual: true,
			From: "each drum"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LintOutcomes([]PlanOutcome{tt.o})
			if !slices.Equal(got, tt.want) {
				t.Errorf("LintOutcomes = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestLintOutcomesOrderAndPurity: flags come in plan order, and linting
// never changes the outcomes it reads.
func TestLintOutcomesOrderAndPurity(t *testing.T) {
	os := []PlanOutcome{
		{ID: "a", Outcome: "A beat you create shows its name."},
		{ID: "b", Outcome: "Every beat shows its name."},
		{ID: "c", Outcome: "A beat you create links to its page.", From: "each beat links to its page"},
	}
	before := slices.Clone(os)
	got := LintOutcomes(os)
	want := []OutcomeFlag{{Outcome: "b", Rule: LintSharedData, Word: "every"},
		{Outcome: "c", Rule: LintNarrowedFrom, Word: "each"}}
	if !slices.Equal(got, want) {
		t.Errorf("LintOutcomes = %+v, want %+v", got, want)
	}
	if !slices.Equal(os, before) {
		t.Error("LintOutcomes changed its input")
	}
}

// TestLintOutcomesProperty: on random outcome text, a flag's word is one of
// the lint's own words (never the outcome's text), at most one flag of each
// rule is raised per outcome, a manual outcome is never flagged, and an
// outcome flagged shared-data holds its word as a whole word.
func TestLintOutcomesProperty(t *testing.T) {
	words := []string{"every", "each", "all", "other", "others", "existing", "still", "works", "a", "beat",
		"you", "create", "Each,", "ALL.", "eachother", "overall", "‮", "`x`", "@team"}
	own := map[string]bool{"every": true, "each": true, "all": true, "other": true, "others": true,
		"existing": true, "still works": true}
	rng := rand.New(rand.NewSource(7))
	text := func() string {
		n := rng.Intn(8)
		ws := make([]string, n)
		for i := range ws {
			ws[i] = words[rng.Intn(len(words))]
		}
		return strings.Join(ws, " ")
	}
	for range 2000 {
		o := PlanOutcome{ID: "o", Outcome: text(), From: text(), Manual: rng.Intn(5) == 0}
		got := LintOutcomes([]PlanOutcome{o})
		seen := map[LintRule]bool{}
		for _, f := range got {
			if !own[f.Word] {
				t.Fatalf("%+v: word %q is not the lint's own", o, f.Word)
			}
			if seen[f.Rule] {
				t.Fatalf("%+v: rule %s raised twice", o, f.Rule)
			}
			seen[f.Rule] = true
			if f.Rule == LintSharedData && !hasWord(lintWords(o.Outcome), f.Word) {
				t.Fatalf("%+v: shared-data word %q is not a word of the outcome", o, f.Word)
			}
		}
		if o.Manual && len(got) > 0 {
			t.Fatalf("%+v: a manual outcome is flagged", o)
		}
	}
}
