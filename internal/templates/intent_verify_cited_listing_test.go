// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
)

// bigCitedCalls are n cited calls of one outcome, numbered from first, each
// sending and returning as much as a call may show: every other one the
// evaluator's own program, the rest http requests. Each call's request
// starts with a token naming its seq, its target.
func bigCitedCalls(first int64, n int) []VerifyCitedCall {
	calls := make([]VerifyCitedCall, 0, n)
	for i := range n {
		seq := first + int64(i)
		c := VerifyCitedCall{Seq: seq, Verb: "http", Input: fmt.Sprintf("GET /tgt-%d/%s", seq,
			strings.Repeat("a", 2*verifyClaimInputMaxBytes)), Output: strings.Repeat("out ", verifyClaimOutputMaxBytes)}
		if i%2 == 1 {
			c.Verb, c.Own, c.Exit = "exec", true, 3
			c.Input = fmt.Sprintf(`{"argv":["tgt-%d","%s"]}`, seq, strings.Repeat("b", 2*verifyClaimInputMaxBytes))
		}
		calls = append(calls, c)
	}
	return calls
}

// citedHead is how a comment names a cited call, its own label included.
func citedHead(c VerifyCitedCall) string {
	head := fmt.Sprintf("Call %d, %s", c.Seq, verbWords[c.Verb])
	if c.Own {
		head += ", the evaluator's own program"
	}
	return head
}

// listsEveryCall reports what out fails to show of calls: each call's head
// (seq, verb, own label) and the start of its target.
func listsEveryCall(out string, calls []VerifyCitedCall) []string {
	var missing []string
	for _, c := range calls {
		if !strings.Contains(out, citedHead(c)) {
			missing = append(missing, citedHead(c))
		}
		if target := fmt.Sprintf("tgt-%d", c.Seq); !strings.Contains(out, target) {
			missing = append(missing, target)
		}
	}
	return missing
}

// TestCitedCallsEachKeepTheirHead (FA 4): an outcome whose cited calls are
// together over their block's bound shows every one of them, each with its
// seq, verb, own label and the start of what it sent; only their bodies are
// cut, never a later call left out whole.
func TestCitedCallsEachKeepTheirHead(t *testing.T) {
	calls := bigCitedCalls(1, VerifyCitedCallsMax)
	s := VerifySection{Observed: true, Outcomes: []VerifyOutcomeResult{{ID: "big", Statement: "It holds.",
		Result: OutcomeResultPassed, Reason: "seen-to-hold", Cited: calls}}}
	out, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if missing := listsEveryCall(out, calls); len(missing) > 0 {
		t.Errorf("the block does not show %q", missing)
	}
}

// TestFitVerifyListsEveryCitedCall (FA 4): at the worst a round can write
// (20 outcomes of the longest id and a long statement and note, each citing
// 8 calls of the longest request and output), the issue comment and the
// pull request's section still render within their bounds, and every cited
// call of every outcome is listed with its seq, verb, own label and the
// start of its target: what fitVerify leaves out is the calls' bodies,
// which the comment says, never the cited section whole.
func TestFitVerifyListsEveryCitedCall(t *testing.T) {
	s := VerifySection{Observed: true, Progress: VerifyProgress{State: VerifyFailing, Verification: 99,
		FixesUsed: 9, MaxFixes: 9, Passing: 10, Total: 20, Outcomes: true}}
	var all []VerifyCitedCall
	for i := range 20 {
		calls := bigCitedCalls(int64(8*i+1), VerifyCitedCallsMax)
		all = append(all, calls...)
		s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o%02d-%s", i, strings.Repeat("x", 28)),
			Statement: strings.Repeat("The statement goes on. ", 22), Kind: "change", Result: OutcomeResultFailed,
			Reason: "seen-not-to-hold", Note: strings.Repeat("n", 600), Cited: calls})
	}
	section, err := renderVerifySection(&s)
	if err != nil {
		t.Fatalf("section: %v", err)
	}
	issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "x", Round: 99, Attempt: 9,
		Section: s})
	if err != nil {
		t.Fatalf("issue comment: %v", err)
	}
	for _, c := range []struct {
		name, out string
		limit     int
	}{{"section", section, verifySectionMaxBytes}, {"issue comment", issue, MaxCommentBytes}} {
		if len(c.out) > c.limit {
			t.Errorf("%s is %d bytes, over %d", c.name, len(c.out), c.limit)
		}
		if missing := listsEveryCall(c.out, all); len(missing) > 0 {
			t.Errorf("%s does not list %d of the cited calls, first %q", c.name, len(missing), missing[0])
		}
		if !strings.Contains(c.out, evidenceCutLead) || !strings.Contains(c.out, "cited call bodies") {
			t.Errorf("%s does not say it left cited call bodies out:\n%.600s", c.name, c.out[len(c.out)-600:])
		}
	}
}
