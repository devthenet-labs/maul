// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"math/rand"
	"testing"

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
// outcome, and the diff is not one maul could not read or the forge cut.
// A diff the prompt's own bound cut, or one with files not shown in full,
// is still focused (the prompt then says every outcome is affected).
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
		{"a diff the prompt bound cut", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{ShownBytes: 10, ReadBytes: 40000}
		}, true},
		{"files not shown in full", func(h *EvaluateHandoff) {
			h.FixDiffCut = &FixDiffCut{Files: []string{"vendor/big.go"}, MoreFiles: 2}
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
// handoffs: a round is change-focused exactly when an independent oracle
// agrees, and a focused round always has a non-blank diff, a result for
// every outcome and a diff nothing of which was unavailable or forge-cut.
func TestChangeFocusedMatchesItsOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(focusSeed))
	for i := range 2000 {
		h := randomFocusHandoff(rng)
		want := oracleFocused(h)
		if got := h.changeFocused(); got != want {
			t.Fatalf("case %d: changeFocused = %v, oracle %v, for %+v", i, got, want, h)
		}
		if !want {
			continue
		}
		have := map[string]bool{}
		for _, v := range h.PriorVerdicts {
			have[v.Outcome] = true
		}
		for _, o := range h.Outcomes {
			if !have[o.ID] {
				t.Fatalf("case %d: focused with no result for %s", i, o.ID)
			}
		}
		if h.FixDiffCut != nil && (len(h.FixDiffCut.Unavailable) > 0 || len(h.FixDiffCut.ForgeCut) > 0) {
			t.Fatalf("case %d: focused on a diff %+v that is not whole of what was read", i, h.FixDiffCut)
		}
	}
}

func randomFocusHandoff(rng *rand.Rand) EvaluateHandoff {
	ids := []string{"a", "b", "c", "d"}
	var h EvaluateHandoff
	for _, id := range ids[:1+rng.Intn(len(ids))] {
		h.Outcomes = append(h.Outcomes, acceptance.PlanOutcome{ID: id})
	}
	for _, id := range ids {
		if rng.Intn(4) > 0 {
			h.PriorVerdicts = append(h.PriorVerdicts, PriorVerdict{Outcome: id, Result: []string{"passed", "failed",
				"unsure"}[rng.Intn(3)]})
		}
	}
	h.FixDiff = []string{"", " ", "+x\n"}[rng.Intn(3)]
	h.PriorRound = rng.Intn(2) * 3
	if rng.Intn(2) == 0 {
		h.FixDiffCut = &FixDiffCut{ReadBytes: rng.Intn(3)}
		if rng.Intn(3) == 0 {
			h.FixDiffCut.Unavailable = []string{"x"}
		}
		if rng.Intn(3) == 0 {
			h.FixDiffCut.ForgeCut = []string{"y"}
		}
		if rng.Intn(3) == 0 {
			h.FixDiffCut.Files = []string{"f"}
		}
	}
	return h
}

func oracleFocused(h EvaluateHandoff) bool {
	if len(h.FixDiff) == 0 || h.PriorRound == 0 {
		return false
	}
	blank := true
	for _, r := range h.FixDiff {
		if r != ' ' && r != '\n' {
			blank = false
		}
	}
	if blank {
		return false
	}
	if c := h.FixDiffCut; c != nil && (len(c.Unavailable) > 0 || len(c.ForgeCut) > 0) {
		return false
	}
	for _, o := range h.Outcomes {
		found := false
		for _, v := range h.PriorVerdicts {
			found = found || v.Outcome == o.ID
		}
		if !found {
			return false
		}
	}
	return true
}
