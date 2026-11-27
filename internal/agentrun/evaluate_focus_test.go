// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/templates"
)

// focusSeed seeds TestChangeFocusedMatchesItsOracle.
const focusSeed = 20261127

// focusedHandoff is a round after a fix that is change-focused: a whole
// diff and a previous result for every outcome.
func focusedHandoff() EvaluateHandoff {
	return EvaluateHandoff{
		Round: 2, PriorRound: 1, FixDiff: "+k := 32\n",
		Outcomes: []acceptance.PlanOutcome{
			{ID: "rate-battle", Outcome: "Rating a battle moves the winner's rating up.", Kind: acceptance.OutcomeKindChange},
			{ID: "leaderboard", Outcome: "The leaderboard still lists every player.", Kind: acceptance.OutcomeKindKeep},
		},
		PriorVerdicts: []PriorVerdict{{Outcome: "rate-battle", Result: "failed"}, {Outcome: "leaderboard", Result: "passed"}},
	}
}

// TestChangeFocusedEachConditionAlone: a round is change-focused only when
// the handoff carries a diff, a previous round and a result for every
// outcome, and the diff is the whole fix: none of it unavailable, cut by
// the forge, cut by the prompt's own bound, or a file not shown in full.
// The light look at an untouched outcome is for a diff the evaluator has
// read whole, so a cut diff is never focused.
func TestChangeFocusedEachConditionAlone(t *testing.T) {
	tests := []struct {
		name string
		edit func(*EvaluateHandoff)
		want bool
	}{
		{"all conditions hold", func(*EvaluateHandoff) {}, true},
		{"round one: no diff", func(h *EvaluateHandoff) { h.FixDiff, h.PriorRound, h.PriorVerdicts = "", 0, nil }, false},
		{"no diff", func(h *EvaluateHandoff) { h.FixDiff = "" }, false},
		{"a blank diff", func(h *EvaluateHandoff) { h.FixDiff = " \n" }, false},
		{"no previous round", func(h *EvaluateHandoff) { h.PriorRound = 0 }, false},
		{"no previous results", func(h *EvaluateHandoff) { h.PriorVerdicts = nil }, false},
		{"an outcome with no result", func(h *EvaluateHandoff) { h.PriorVerdicts = h.PriorVerdicts[:1] }, false},
		{"a result for another outcome", func(h *EvaluateHandoff) { h.PriorVerdicts[1].Outcome = "gone" }, false},
		{"a diff maul could not read", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{Unavailable: []string{"acme/web"}}
		}, false},
		{"a diff the forge cut", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{ForgeCut: []string{"acme/api"}}
		}, false},
		{"a diff the controller's bound cut", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{ShownBytes: 10, ReadBytes: 40000}
		}, false},
		{"a diff over the prompt's bound", func(h *EvaluateHandoff) {
			h.FixDiff = strings.Repeat("+k := 32\n", templates.FixDiffMaxBytes/9+1)
		}, false},
		{"a diff just within the prompt's bound", func(h *EvaluateHandoff) {
			h.FixDiff = strings.Repeat("+", templates.FixDiffMaxBytes-1) + "\n"
		}, true},
		{"files not shown in full", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{Files: []string{"vendor/big.go"}}
		}, false},
		{"more files not shown in full", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{MoreFiles: 2}
		}, false},
		{"a cut that leaves nothing out", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{ShownBytes: 40, ReadBytes: 40}
		}, true},
	}
	for _, tt := range tests {
		h := focusedHandoff()
		tt.edit(&h)
		if got := h.changeFocused(); got != tt.want {
			t.Errorf("%s: changeFocused = %v, want %v", tt.name, got, tt.want)
		}
		if got := h.prompt("", "", templates.StageLimits{}).Focused; got != tt.want {
			t.Errorf("%s: the prompt's Focused = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestChangeFocusedMatchesItsOracle is a seeded property over random
// handoffs, blank diffs of every kind of white space, diffs either side of
// the prompt's bound and every way a cut can leave something out included:
// a round is change-focused exactly when an oracle written from the rule's
// words (not from changeFocused's code) agrees.
func TestChangeFocusedMatchesItsOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(focusSeed))
	seen := map[bool]int{}
	for i := range 4000 {
		h := randomFocusHandoff(rng)
		want := oracleFocused(h)
		seen[want]++
		if got := h.changeFocused(); got != want {
			t.Fatalf("case %d: changeFocused = %v, oracle %v, for a %d-byte diff, cut %+v, round %d, results %+v",
				i, got, want, len(h.FixDiff), h.FixDiffCut, h.PriorRound, h.PriorVerdicts)
		}
	}
	if seen[true] < 100 || seen[false] < 100 {
		t.Fatalf("the generator is lopsided: %v", seen)
	}
}

func randomFocusHandoff(rng *rand.Rand) EvaluateHandoff {
	ids := []string{"a", "b", "c", "d"}
	var h EvaluateHandoff
	for _, id := range ids[:1+rng.Intn(len(ids))] {
		h.Outcomes = append(h.Outcomes, acceptance.PlanOutcome{ID: id})
	}
	for _, id := range ids {
		if rng.Intn(6) > 0 {
			h.PriorVerdicts = append(h.PriorVerdicts, PriorVerdict{Outcome: id, Result: []string{"passed", "failed",
				"unsure", "accepted", "quarantined"}[rng.Intn(5)]})
		}
	}
	h.FixDiff = []string{"", " ", "\t\r\n", "\u00a0\n", "+x\n", "+x\n", "+x\n",
		strings.Repeat("+", templates.FixDiffMaxBytes), strings.Repeat("+", templates.FixDiffMaxBytes+1)}[rng.Intn(9)]
	h.PriorRound = []int{0, 1, 3}[rng.Intn(3)]
	if rng.Intn(2) == 0 {
		read := rng.Intn(3)
		h.FixDiffCut = &FixDiffCut{ReadBytes: read, ShownBytes: read - rng.Intn(2)}
		if rng.Intn(4) == 0 {
			h.FixDiffCut.Unavailable = []string{"x"}
		}
		if rng.Intn(4) == 0 {
			h.FixDiffCut.ForgeCut = []string{"y"}
		}
		if rng.Intn(4) == 0 {
			h.FixDiffCut.Files = []string{"f"}
		}
		if rng.Intn(4) == 0 {
			h.FixDiffCut.MoreFiles = 1
		}
	}
	return h
}

// oracleFocused is the rule as the brief states it: a previous round, a
// diff with something in it that the evaluator is shown whole, and a
// result for every outcome. The generator's diffs are ASCII or a no-break
// space, so their length is what the prompt measures against its bound.
func oracleFocused(h EvaluateHandoff) bool {
	hasContent := strings.IndexFunc(h.FixDiff, func(r rune) bool { return !unicode.IsSpace(r) }) >= 0
	if h.PriorRound == 0 || !hasContent || len(h.FixDiff) > templates.FixDiffMaxBytes {
		return false
	}
	if c := h.FixDiffCut; c != nil && (c.ShownBytes < c.ReadBytes || len(c.Unavailable)+len(c.ForgeCut)+
		len(c.Files)+c.MoreFiles > 0) {
		return false
	}
	for _, o := range h.Outcomes {
		if !slices.ContainsFunc(h.PriorVerdicts, func(v PriorVerdict) bool { return v.Outcome == o.ID }) {
			return false
		}
	}
	return true
}
