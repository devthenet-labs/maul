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
		// "all" and "other" name the check's own items as often as shared
		// ones ("all 16 steps", "the other beat you create"), so neither is
		// flagged on its own.
		{"all of its own", PlanOutcome{ID: "a", Outcome: "A beat you save plays all 16 steps in order."}, nil},
		{"the other of its own", PlanOutcome{ID: "a",
			Outcome: "A vote for one of two beats you create moves only that beat, not the other beat you create."},
			nil},
		{"others", PlanOutcome{ID: "a", Outcome: "Mine is listed above the others."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "others"}}},
		{"existing", PlanOutcome{ID: "a", Outcome: "Existing beats keep their names."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "existing"}}},
		{"still works", PlanOutcome{ID: "a", Outcome: "The sequencer still   works.", Kind: OutcomeKindKeep},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "still works"}}},
		// O7-B-2, beatbattle-49's keep outcome on two capped, shared lists.
		{"still work", PlanOutcome{ID: "leaderboard-ratings-unaffected", Outcome: "The main leaderboard and the " +
			"ratings list still work exactly as before, unaffected by the new stakes field.", Kind: OutcomeKindKeep},
			[]OutcomeFlag{{Outcome: "leaderboard-ratings-unaffected", Rule: LintSharedData, Word: "still work"}}},
		{"work after still, not still work", PlanOutcome{ID: "a", Outcome: "A beat you save is still there; " +
			"its steps work."}, nil},
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
		// L6-B-BROWSER-3: a field named Every (capitalised inside a
		// sentence, or in a code span) is a name, not a quantifier; gate
		// 5's plan, 8 of 10 outcomes of which were flagged for it.
		{"a field named Every", PlanOutcome{ID: "a",
			Outcome: "On /, with Every at 4, pressing Fill kick turns on exactly kick steps 1, 5, 9 and 13.",
			From:    "Pressing a track's fill button with `Every` at N turns on that track's steps 1, 1+N, ..."}, nil},
		{"a field named Every after a colon", PlanOutcome{ID: "a", Outcome: "On /, with Every set to 17, the " +
			"error line reads: Every must be a whole number from 1 to 16.",
			From: "the page's error line says `Every must be a whole number from 1 to 16`."}, nil},
		{"the Every field", PlanOutcome{ID: "a",
			Outcome: "At 1280 by 800 on /, the Every field and the three fill buttons are visible."}, nil},
		{"a code span", PlanOutcome{ID: "a", Outcome: "The `each` query parameter of a beat you create is kept."},
			nil},
		{"the field and the quantifier", PlanOutcome{ID: "a",
			Outcome: "With Every at 4, every saved beat shows its fill."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"a sentence that starts with Every", PlanOutcome{ID: "a",
			Outcome: "A beat you save loads back. Every beat in Fresh beats links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"a quoted sentence start", PlanOutcome{ID: "a", Outcome: `"Each beat links to its page."`},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "each"}}},
		{"a list item that starts with Every", PlanOutcome{ID: "a", Outcome: "- Every beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"an emphasised sentence start", PlanOutcome{ID: "a", Outcome: "**Every** beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"narrowed from a list item", PlanOutcome{ID: "a", Outcome: "A beat you create links to its page.",
			From: "- Each beat in Fresh beats links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintNarrowedFrom, Word: "each"}}},
		// A list marker is not a word before it: from quotes issue lines.
		{"narrowed from a numbered item", PlanOutcome{ID: "a", Outcome: "A beat you create links to its page.",
			From: "1) Each beat in Fresh beats links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintNarrowedFrom, Word: "each"}}},
		{"narrowed from a lettered item", PlanOutcome{ID: "a", Outcome: "A beat you create links to its page.",
			From: "(a) Every beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintNarrowedFrom, Word: "every"}}},
		{"narrowed from a task item", PlanOutcome{ID: "a", Outcome: "A beat you create links to its page.",
			From: "- [x] Every beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintNarrowedFrom, Word: "every"}}},
		{"a numbered outcome that starts with Each", PlanOutcome{ID: "a", Outcome: "12) Each beat links to its page."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "each"}}},
		{"capitals for emphasis", PlanOutcome{ID: "a", Outcome: "The board lists EVERY player."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"an unclosed backtick hides nothing", PlanOutcome{ID: "a", Outcome: "The ` mark shows on every beat."},
			[]OutcomeFlag{{Outcome: "a", Rule: LintSharedData, Word: "every"}}},
		{"a name never joins a phrase", PlanOutcome{ID: "a", Outcome: "The pad still Lights works.",
			Kind: OutcomeKindKeep}, nil},
		{"a field name in from is not narrowing", PlanOutcome{ID: "a", Outcome: "A beat you create fills.",
			From: "the Every field fills a track"}, nil},
		{"narrowed from a quantifier beside a name", PlanOutcome{ID: "a", Outcome: "A beat you create fills.",
			From: "with `Every` at N, each saved beat fills"},
			[]OutcomeFlag{{Outcome: "a", Rule: LintNarrowedFrom, Word: "each"}}},
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
	words := []string{"every", "each", "all", "other", "others", "existing", "still", "works", "work", "a", "beat",
		"you", "create", "Each,", "ALL.", "eachother", "overall", "\u202e", "`x`", "@team"}
	own := map[string]bool{"every": true, "each": true, "all": true, "other": true, "others": true,
		"existing": true, "still works": true, "still work": true}
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

// TestLintGateFivePlan (L6-B-BROWSER-3): the outcomes of gate 5's plan about
// a number field named Every, 8 of 10 of which were flagged "every", raise
// no flag.
func TestLintGateFivePlan(t *testing.T) {
	for _, o := range []string{
		"At 1280 by 800 on /, the Every field and the three fill buttons are visible, readable, clickable, and do " +
			"not overlap the BPM slider, Swing slider, Play or Stop.",
		"On /, a beat you name, give a handle, fill (kick with Every 4) and save appears in Fresh beats, and " +
			"clicking its name loads the grid back with kick steps 1, 5, 9, 13 on.",
		"On /, after an invalid Every shows the error, setting Every to 4 and pressing Fill kick fills the kick " +
			"track and hides the error message.",
		"On /, after the Swing slider there is a number field labelled Every starting at 4, then buttons showing " +
			"Kick, Snare and Hat with accessible names Fill kick, Fill snare and Fill hat, before the Beat name field.",
		"On /, after you click hat steps 2 and 3 on, Fill hat with Every 4 leaves hat steps 1, 5, 9, 13 on and " +
			"steps 2 and 3 off.",
		"On /, with Every at 4, pressing Fill kick turns on exactly kick steps 1, 5, 9 and 13 and no other kick step.",
		"On /, with Every at any whole number 1 to 16, Fill snare turns on exactly snare steps 1, 1+N, 1+2N ... up " +
			"to 16, for example N 3 gives 1, 4, 7, 10, 13, 16 and N 16 gives only step 1.",
		"On /, with Every set to a value that is not a whole number from 1 to 16 (empty, 17 or 2.5), pressing any " +
			"fill button changes no pad and the error line reads: Every must be a whole number from 1 to 16.",
	} {
		if got := LintOutcomes([]PlanOutcome{{ID: "o", Outcome: o}}); len(got) != 0 {
			t.Errorf("%q: flags %+v, want none", o, got)
		}
	}
}
