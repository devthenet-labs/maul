// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// baseRuleCase is one clause of the base rule as the prompts state it,
// and an agents round that clause is about: one change (or keep) outcome
// with one valid run check, reviewed proves, passing at the head with a
// token, the evaluator agreeing, and base as given.
type baseRuleCase struct {
	clause string
	keep   bool
	base   acceptance.BaseRuns
	counts bool
}

// baseRuleCases are the base rule's clauses (SPEC-S2 S2-20, decided: only
// a compile error, a tree a program cannot be built against and a base
// that did not build leave no base comparison; any other base error is no
// base evidence), each with what acceptance.Decide does on it.
func baseRuleCases() []baseRuleCase {
	run := func(r acceptance.RunResult, step int32, cause acceptance.RunCause) acceptance.BaseRuns {
		cr := acceptance.CheckRun{CheckDigest: "sha256:c", Result: r, Token: "b", Seq: 9, AssertionStep: step,
			Cause: cause}
		if step > 0 {
			cr.AssertionID = "marker x"
		}
		return acceptance.BaseRuns{Required: true, Runs: []acceptance.CheckRun{cr}}
	}
	return []baseRuleCase{
		{clause: "fails at the base by an assertion", base: run(acceptance.RunFail, 1, ""), counts: true},
		{clause: "its program does not compile against the base tree",
			base: run(acceptance.RunError, 0, acceptance.CauseCompile), counts: true},
		{clause: "the base tree cannot be built", base: acceptance.BaseRuns{Required: true, Unbuildable: true},
			counts: true},
		{clause: "or built against", base: run(acceptance.RunError, 0, acceptance.CauseVendoredTree), counts: true},
		{clause: "Any other error at the base (a timeout, processes left running, output too long to keep, a bind " +
			"or connection error) is no base evidence", base: run(acceptance.RunError, 0, ""), counts: false},
		{clause: "a check that passes at the base", base: run(acceptance.RunPass, 0, ""), counts: false},
		{clause: "unless it fails there by an assertion", keep: true, base: run(acceptance.RunFail, 1, ""),
			counts: false},
		{clause: "unless it fails there by an assertion", keep: true, base: run(acceptance.RunError, 0, ""),
			counts: true},
	}
}

// decideBase is Decide on c's round: whether its outcome passed.
func decideBase(c baseRuleCase) bool {
	base := c.base
	in := acceptance.Input{
		Outcomes: []acceptance.OutcomeRef{{ID: "o", Digest: "sha256:o", Keep: c.keep}},
		Checks:   []acceptance.StoredCheck{{Digest: "sha256:c", Outcome: "o", Form: acceptance.CheckFormRun, Valid: true}},
		Replay:   []acceptance.CheckRun{{CheckDigest: "sha256:c", Result: acceptance.RunPass, Token: "h", Seq: 3}},
		Reviews: []acceptance.CheckReview{{CheckDigest: "sha256:c", OutcomeDigest: "sha256:o",
			Answer: acceptance.ReviewProves}},
		Verdicts: []acceptance.EvaluatorVerdict{{Outcome: "o", Claim: acceptance.ClaimPass, Ledger: []int32{3}}},
		Base:     &base,
	}
	return acceptance.Decide(in).PerOutcome[0].Result == acceptance.ResultPassed
}

// TestBaseRulePinnedToDecide (S2-20, from S2-15): the check reviewer's and
// the evaluator's prompts state Decide's base rule in words; each clause
// they state is pinned to what Decide does on it, so a rule change cannot
// drift from the words, nor the words from the rule.
func TestBaseRulePinnedToDecide(t *testing.T) {
	review, err := RenderCheckReviewPrompt(testWorkspaceReview(t, ExampleCLIRunCheck, testReviewCLI))
	if err != nil {
		t.Fatal(err)
	}
	evaluate, err := RenderEvaluatePrompt(testWorkspacePrompt(testCLIWorkspace))
	if err != nil {
		t.Fatal(err)
	}
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	review, evaluate = flat(review), flat(evaluate)
	for _, c := range baseRuleCases() {
		if got := decideBase(c); got != c.counts {
			t.Errorf("%q (keep %v): Decide passes %v, the words say %v", c.clause, c.keep, got, c.counts)
		}
		if c.clause == "a check that passes at the base" {
			continue // the evaluator's words only
		}
		if !strings.Contains(review, c.clause) {
			t.Errorf("the check reviewer's prompt does not state %q", c.clause)
		}
	}
	// The evaluator is told the same clauses, as what its checks must do.
	for _, clause := range []string{"fail at the base by an assertion", "a check that passes at the base",
		"whose program does not compile against the base tree", "whose base tree cannot be built or built against",
		"Any other error at the base (a timeout, processes left running, output too long to keep, a bind or " +
			"connection error) is no base evidence"} {
		if !strings.Contains(evaluate, clause) {
			t.Errorf("the evaluator's prompt does not state %q", clause)
		}
	}
}
