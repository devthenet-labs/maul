// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"errors"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// outcomesBlock is DESIGN 2.3's approved plan.
const outcomesBlock = `acceptance:
  - id: winner-rises
    outcome: Rating a battle moves the winner above the loser on the leaderboard.
    from: "ratings should update after every battle"
    kind: change
  - id: upset-gains-more
    outcome: Beating a higher-rated player gains more points than beating a lower-rated one.
    from: "use Elo, not +1/-1"
  - id: leaderboard-still-loads
    outcome: The leaderboard page still lists every player.
    kind: keep
  - id: sounds-right
    outcome: The victory sound plays once.
    manual: true
    reason: only a human can judge how it sounds
`

func TestParsePlanOutcomes(t *testing.T) {
	parsers := map[string]func([]byte) (*Plan, error){"ParsePlan": ParsePlan, "ParsePlanInput": ParsePlanInput}
	for name, parse := range parsers {
		t.Run(name, func(t *testing.T) {
			p, err := parse([]byte(planAcceptance(outcomesBlock)))
			if err != nil {
				t.Fatalf("%s() error = %v", name, err)
			}
			if p.Acceptance != nil {
				t.Errorf("Acceptance = %v, want no criteria", p.Acceptance)
			}
			want := []acceptance.PlanOutcome{
				{ID: "winner-rises", Outcome: "Rating a battle moves the winner above the loser on the leaderboard.",
					From: "ratings should update after every battle", Kind: acceptance.OutcomeKindChange},
				{ID: "upset-gains-more", From: "use Elo, not +1/-1",
					Outcome: "Beating a higher-rated player gains more points than beating a lower-rated one."},
				{ID: "leaderboard-still-loads", Outcome: "The leaderboard page still lists every player.",
					Kind: acceptance.OutcomeKindKeep},
				{ID: "sounds-right", Outcome: "The victory sound plays once.", Manual: true,
					Reason: "only a human can judge how it sounds"},
			}
			if len(p.Outcomes) != len(want) {
				t.Fatalf("Outcomes = %+v, want %d", p.Outcomes, len(want))
			}
			for i := range want {
				if p.Outcomes[i] != want[i] {
					t.Errorf("Outcomes[%d] = %+v, want %+v", i, p.Outcomes[i], want[i])
				}
			}
			if err := p.CheckEvaluator(); err != nil {
				t.Errorf("CheckEvaluator() = %v, want an outcomes plan admitted", err)
			}
		})
	}
}

// TestParsePlanCriteriaUnchanged: a slice-1 criteria plan still parses as
// before, with no outcomes; only CheckEvaluator refuses it, for a replan.
func TestParsePlanCriteriaUnchanged(t *testing.T) {
	p, err := ParsePlan([]byte(planAcceptance(acceptanceBlock)))
	if err != nil {
		t.Fatalf("ParsePlan() error = %v", err)
	}
	if len(p.Acceptance) != 4 || p.Outcomes != nil {
		t.Fatalf("Acceptance = %d criteria, Outcomes = %v", len(p.Acceptance), p.Outcomes)
	}
	err = p.CheckEvaluator()
	if !errors.Is(err, ErrSliceOneCriteria) || !strings.Contains(err.Error(), "replan") {
		t.Fatalf("CheckEvaluator() = %v, want ErrSliceOneCriteria asking for a replan", err)
	}
	manualOnly, err := ParsePlan([]byte(planAcceptance(
		"acceptance:\n  - {id: a, kind: manual, statement: it sounds good}\n")))
	if err != nil {
		t.Fatalf("ParsePlan(manual criterion) error = %v", err)
	}
	if !errors.Is(manualOnly.CheckEvaluator(), ErrSliceOneCriteria) {
		t.Error("CheckEvaluator() admitted a manual slice-1 criterion")
	}
	for _, src := range []string{validPlan, planAcceptance("acceptance: []\n")} {
		p, err := ParsePlan([]byte(src))
		if err != nil {
			t.Fatalf("ParsePlan() error = %v", err)
		}
		if p.Outcomes != nil || p.CheckEvaluator() != nil {
			t.Errorf("no acceptance: Outcomes = %v, CheckEvaluator() = %v", p.Outcomes, p.CheckEvaluator())
		}
	}
}

func TestParsePlanOutcomeErrors(t *testing.T) {
	outcome := func(ls ...string) string { return "acceptance:\n  - " + strings.Join(ls, "\n    ") + "\n" }
	one := "{id: a, outcome: it works}"
	tests := []struct {
		name, block, want string
	}{
		{"steps on an outcome", outcome("id: a", "outcome: it works", "steps:", "  - request: {path: /}",
			"    expect: {status: 200}"), `acceptance[0] carries steps`},
		{"request and expect", outcome("id: a", "outcome: it works", "request: {path: /}", "expect: {status: 200}"),
			`acceptance[0] carries request, expect`},
		{"target", outcome("id: a", "outcome: it works", "target: api"), "carries target"},
		{"argv", outcome("id: a", "outcome: it works", "argv: [/bin/true]"), "carries argv"},
		{"mixed list", outcome(one) + "  - {id: b, kind: http, statement: s, target: api, request: {path: /}}\n",
			"acceptance[1] carries statement, request, target"},
		{"criterion before outcome", "acceptance:\n  - {id: b, kind: manual, statement: s}\n  - " + one + "\n",
			"acceptance[0] carries statement"},
		{"unknown outcome key", outcome("id: a", "outcome: it works", "flavour: x"), "field flavour not found"},
		{"bad kind", outcome("id: a", "outcome: it works", "kind: http"), `kind "http" is not one of change, keep`},
		{"no outcome text", outcome("id: a", "from: a line", "outcome: ''"), "outcome is required"},
		{"duplicate id", outcome(one) + "  - " + one + "\n", "id is used twice"},
		{"multi-line outcome", outcome("id: a", "outcome: |", "  one", "  two"), `outcome "a"`},
		{"too many", "acceptance:\n" + strings.Repeat("  - {id: a, outcome: x}\n", 21), "more than 20 outcomes"},
		{"not a list", "acceptance: {id: a, outcome: x}\n", "cannot unmarshal !!map"},
		{"manual not a bool", outcome("id: a", "outcome: x", "manual: maybe"), "into bool"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlan([]byte(planAcceptance(tc.block)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParsePlan() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParsePlanOutcomesHiddenCharacters: the document-wide visibility check
// covers outcomes like every other value.
func TestParsePlanOutcomesHiddenCharacters(t *testing.T) {
	block := "acceptance:\n  - {id: a, outcome: \"safe \\u202e evil\"}\n"
	if _, err := ParsePlan([]byte(planAcceptance(block))); err == nil {
		t.Fatal("ParsePlan() accepted a bidi override in an outcome")
	}
}
