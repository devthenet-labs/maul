// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
)

// TestProposedCheckIDMatchesTheGrammar pins a proposed check's id to the
// saved-check grammar's (checks.Check.ID, the authority): the report admits
// exactly the ids checks.Validate admits, the ".<name>" suffix included, so
// a check the evaluator saved under a valid id can always be proposed, and
// a proposal never names an id no saved check can carry.
func TestProposedCheckIDMatchesTheGrammar(t *testing.T) {
	ids := []string{
		"winner-rises", "winner-rises.order", "w", "w.1", "a" + strings.Repeat("b", 31),
		"a" + strings.Repeat("b", 31) + "." + strings.Repeat("c", 32),
		"a" + strings.Repeat("b", 32), "winner-rises." + strings.Repeat("c", 33),
		"Winner", "1winner", "-winner", "winner_rises", "winner.", ".order", "winner..order",
		"winner.order.more", "winner.-order", "winner.Order", "", "winner rises",
	}
	for _, id := range ids {
		quoted, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		c, err := checks.Parse([]byte(`{"id":` + string(quoted) + `,"outcome":"winner-rises","form":"http",` +
			`"http":{"target":"api","steps":[{"request":{"method":"GET","path":"/players"},` +
			`"expect":{"status":200,"bodyContains":"alice"}}]}}`))
		if err != nil {
			t.Fatalf("checks.Parse(%q): %v", id, err)
		}
		grammar := checks.Validate(c, checks.Preview{}) == nil

		raw := strings.Replace(minimalProposal, "PROPOSED_ID", id, 1)
		_, perr := ParseEvaluation([]byte(raw))
		if report := perr == nil; report != grammar {
			t.Errorf("id %q: report admits it = %v (%v), the check grammar = %v", id, report, perr, grammar)
		}
	}
	// The verdicts keep the outcome grammar: no ".<name>" there.
	raw := strings.Replace(strings.Replace(minimalProposal, "PROPOSED_ID", "winner-rises.order", 1),
		"  - id: winner-rises\n", "  - id: winner-rises.order\n", 1)
	if _, err := ParseEvaluation([]byte(raw)); err == nil {
		t.Error("ParseEvaluation() admitted a verdict on a check id, want an outcome id only")
	}
}

// minimalProposal is a report proposing one check, PROPOSED_ID.
const minimalProposal = `---
verdicts:
  - id: winner-rises
    verdict: pass
    ledger: [4]
disputes: []
proposed:
  - id: "PROPOSED_ID"
    outcome: winner-rises
    digest: "` + digestA + `"
retire: []
anomalies: []
---
Checked the leaderboard.
`
