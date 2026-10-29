// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateOutcomes(t *testing.T) {
	ok := PlanOutcome{ID: "winner-rises", Outcome: "Rating a battle moves the winner above the loser."}
	with := func(f func(*PlanOutcome)) []PlanOutcome {
		o := ok
		f(&o)
		return []PlanOutcome{o}
	}
	many := make([]PlanOutcome, MaxOutcomes+1)
	for i := range many {
		many[i] = PlanOutcome{ID: "o" + strings.Repeat("a", i), Outcome: "x"}
	}
	tests := []struct {
		name string
		os   []PlanOutcome
		want string // "" = valid
	}{
		{"none", nil, ""},
		{"one", []PlanOutcome{ok}, ""},
		{"change", with(func(o *PlanOutcome) { o.Kind = OutcomeKindChange; o.From = "ratings should update" }), ""},
		{"keep", with(func(o *PlanOutcome) { o.Kind = OutcomeKindKeep }), ""},
		{"manual", with(func(o *PlanOutcome) { o.Manual = true; o.Reason = "only a human can judge the sound" }), ""},
		{"max", many[:MaxOutcomes], ""},
		{"too many", many, "more than 20 outcomes"},
		{"bad id", with(func(o *PlanOutcome) { o.ID = "Bad_ID" }), "id must match"},
		{"empty id", with(func(o *PlanOutcome) { o.ID = "" }), "id must match"},
		{"duplicate id", []PlanOutcome{ok, ok}, "id is used twice"},
		{"no outcome", with(func(o *PlanOutcome) { o.Outcome = "  " }), "outcome is required"},
		{"long outcome", with(func(o *PlanOutcome) { o.Outcome = strings.Repeat("é", MaxOutcomeChars+1) }), "over 500"},
		{"outcome at bound", with(func(o *PlanOutcome) { o.Outcome = strings.Repeat("é", MaxOutcomeChars) }), ""},
		{"multi-line outcome", with(func(o *PlanOutcome) { o.Outcome = "one\ntwo" }), "control character"},
		{"invisible outcome", with(func(o *PlanOutcome) { o.Outcome = "safe \u202e evil" }), "control character"},
		{"long from", with(func(o *PlanOutcome) { o.From = strings.Repeat("a", MaxFromChars+1) }), "over 300"},
		{"multi-line from", with(func(o *PlanOutcome) { o.From = "a\nb" }), "control character"},
		{"bad kind", with(func(o *PlanOutcome) { o.Kind = "http" }), `kind "http" is not one of change, keep`},
		{"long reason", with(func(o *PlanOutcome) { o.Reason = strings.Repeat("a", MaxReasonChars+1) }), "over 120"},
		{"not utf-8", with(func(o *PlanOutcome) { o.Outcome = "\xff" }), "not UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOutcomes(tc.os)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateOutcomes() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateOutcomes() = %v, want it to contain %q", err, tc.want)
			}
			var oe *OutcomeError
			if !errors.As(err, &oe) {
				t.Fatalf("ValidateOutcomes() = %T, want *OutcomeError", err)
			}
		})
	}
}

// TestOutcomeJSON pins the CRD form's tags: v1alpha1.AcceptanceOutcome
// mirrors them.
func TestOutcomeJSON(t *testing.T) {
	b, err := json.Marshal(PlanOutcome{ID: "a", Outcome: "o", From: "f", Kind: OutcomeKindKeep, Manual: true, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"a","outcome":"o","from":"f","kind":"keep","manual":true,"reason":"r"}`
	if string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
	b, _ = json.Marshal(PlanOutcome{ID: "a", Outcome: "o"})
	if string(b) != `{"id":"a","outcome":"o"}` {
		t.Errorf("minimal json = %s", b)
	}
}

func TestOutcomesDigest(t *testing.T) {
	a := PlanOutcome{ID: "a", Outcome: "the winner rises", From: "ratings update"}
	b := PlanOutcome{ID: "b", Outcome: "the board loads", Kind: OutcomeKindKeep}
	explicit := a
	explicit.Kind = OutcomeKindChange
	d := OutcomesDigest([]PlanOutcome{a, b})
	if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Fatalf("digest %q is not sha256:<hex>", d)
	}
	if got := OutcomesDigest([]PlanOutcome{explicit, b}); got != d {
		t.Errorf("an explicit change kind digests %s, the default %s: the default must be explicit first", got, d)
	}
	if OutcomesDigest([]PlanOutcome{b, a}) == d {
		t.Error("reordered outcomes digest alike: plan order is bound")
	}
	changed := a
	changed.From = "ratings update after every battle"
	if OutcomesDigest([]PlanOutcome{changed, b}) == d {
		t.Error("a changed from digests alike")
	}
	manual := a
	manual.Manual = true
	if OutcomesDigest([]PlanOutcome{manual, b}) == d {
		t.Error("a manual outcome digests alike")
	}
	if OutcomesDigest(nil) != OutcomesDigest([]PlanOutcome{}) {
		t.Error("nil and empty digest differently")
	}
}

func TestNormalizeOutcomes(t *testing.T) {
	in := []PlanOutcome{{ID: "a", Outcome: "x"}, {ID: "b", Outcome: "y", Kind: OutcomeKindKeep}}
	got := NormalizeOutcomes(in)
	if got[0].Kind != OutcomeKindChange || got[1].Kind != OutcomeKindKeep {
		t.Errorf("NormalizeOutcomes() kinds = %q, %q", got[0].Kind, got[1].Kind)
	}
	if in[0].Kind != "" {
		t.Error("NormalizeOutcomes changed its input")
	}
	if NormalizeOutcomes(nil) != nil {
		t.Error("NormalizeOutcomes(nil) != nil")
	}
}
