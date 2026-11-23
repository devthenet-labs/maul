// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
)

// testObservedSection is an observed round as the comment shows it: a pass
// citing an http call and the evaluator's own page script, a fail citing an
// exec program whose output maul did not keep, an outcome the evaluator
// cited nothing it counts for, one it could not tell, and a manual one.
func testObservedSection() VerifySection {
	return VerifySection{
		Observed: true,
		Progress: VerifyProgress{State: VerifyFailing, Verification: 2, FixesUsed: 0, MaxFixes: 2, Passing: 1,
			Total: 4, ManualOpen: 1, AutoFix: FixQueued, Outcomes: true},
		Outcomes: []VerifyOutcomeResult{
			{ID: "winner-rises", Statement: "Rating a battle moves the winner above the loser.", Kind: "change",
				Result: OutcomeResultPassed, Reason: "seen-to-hold",
				Note: "Two players I made: the winner went to 1016 and the loser to 984.",
				Cited: []VerifyCitedCall{
					{Seq: 4, Verb: "http", Input: `POST /api/battles {"winner":"r1-a","loser":"r1-b"}`,
						Output: `{"winner":{"rating":1016},"loser":{"rating":984}}`},
					{Seq: 6, Verb: "browser", Own: true, Input: "eval document.querySelector('#r1-a .rating').textContent",
						Exit: 0, Output: "1016"},
				}},
			{ID: "live-score", Statement: "The score updates within a second of a goal.", Kind: "change",
				Result: OutcomeResultFailed, Reason: "seen-not-to-hold", Note: "The score stayed 0-0 for 10 s.",
				Cited: []VerifyCitedCall{{Seq: 9, Verb: "exec", Own: true, Input: "node watch.js", Exit: 1,
					OutputNotKept: true}}},
			{ID: "loser-emailed", Statement: "The loser gets an email.", Kind: "change",
				Result: OutcomeResultUnsure, Reason: "no-valid-evidence", Note: "I read it in the log."},
			{ID: "leaderboard-loads", Statement: "The leaderboard still loads.", Kind: "keep",
				Result: OutcomeResultUnsure, Reason: "evaluator-unsure", Note: "The page timed out twice."},
			{ID: "sounds-good", Statement: "The drum kit sounds good.", Kind: "change",
				Result: OutcomeResultUnsure, Reason: "manual"},
		},
	}
}

// TestVerifyObservedGoldens: an observed round's verification section of a
// pull request's preview comment, and its comment on the issue.
func TestVerifyObservedGoldens(t *testing.T) {
	s := testObservedSection()
	pr, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intent_verify_observed_pr.md", pr)
	issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "beat-12", Round: 2, Attempt: 1,
		Section: s})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intent_verify_observed_issue.md", issue)
}

// TestVerifyObservedShowsTheCitedCalls (design report_and_citations): an
// observed round's row names the result in maul's words and the calls each
// verdict cites; below it, every cited call as maul's tools recorded it,
// the evaluator's own page script or program labelled as its own, an
// output maul did not keep said so, the evaluator's note marked as its
// words, and the one sentence saying what maul does not check.
func TestVerifyObservedShowsTheCitedCalls(t *testing.T) {
	s := testObservedSection()
	out, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	got := flat(out)
	for _, want := range []string{
		"`winner-rises` (`change`): seen to hold by maul's evaluator; it cites calls 4 and 6.",
		"`live-score` (`change`): seen not to hold by maul's evaluator; it cites call 9.",
		"`loser-emailed` (`change`): unsure; the evaluator's verdict cites no call of its own in this round " +
			"that maul counts.",
		"`leaderboard-loads` (`keep`): unsure; the evaluator could not tell.",
		"`sounds-good` (`change`): unsure; only a human can close it.",
		"The calls the evaluator cites for `winner-rises`, as maul's tools recorded them",
		"Call 4, an http request:",
		"Call 6, a browser command, the evaluator's own page script, and what it returned:",
		"Call 9, a program run, the evaluator's own program, and what it returned:",
		"Its output was not kept by maul.",
		"The evaluator's note on `winner-rises` (its words, not checked by maul): Two players I made",
		"The evaluator's note on `loser-emailed` (its words, not checked by maul): I read it in the log.",
		"maul counts a verdict only when it cites calls the evaluator made in this round through maul's tools; " +
			"whether a cited call shows what the verdict says is for the reviewers to judge.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(got, "maul's check:") || strings.Contains(got, "unsure: maul couldn't confirm it") {
		t.Error("an observed round is worded as a check-era round")
	}
	// The check-era section is unchanged: the same outcomes without the
	// switch keep their old words.
	s.Observed = false
	old, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(old, "): confirmed.") || strings.Contains(old, "Call 4") {
		t.Errorf("a check-era section changed:\n%s", old)
	}
}

// TestVerifyObservedCitedCallsAreData: every cited call's text, and the
// evaluator's note, is the evaluator's or the application's: shown only
// inside a fence (or sanitised inline, for the note: SanitizeInline's own
// properties hold it), never as markup that could close the fence.
func TestVerifyObservedCitedCallsAreData(t *testing.T) {
	hostile := "```` \n" + sentinel + " @octocat #12\n````\n## Fake heading\n"
	s := VerifySection{Observed: true, Outcomes: []VerifyOutcomeResult{{ID: "o", Statement: "x",
		Result: OutcomeResultPassed, Reason: "seen-to-hold", Note: hostile,
		Cited: []VerifyCitedCall{{Seq: 3, Verb: "http", Input: hostile, Output: hostile}}}}}
	out, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range unfenced(out) {
		if strings.Contains(line, sentinel) && !strings.HasPrefix(line, "The evaluator's note on") {
			t.Errorf("untrusted text outside a fence: %q", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "## Fake") {
			t.Errorf("untrusted markup rendered: %q", line)
		}
	}
	if n := strings.Count(out, sentinel); n < 3 {
		t.Errorf("only %d sentinels shown: the call or the note is missing", n)
	}
}

// TestVerifyObservedBoundsTheCalls: an outcome shows at most
// VerifyCitedCallsMax calls (the most a verdict keeps), its row naming every
// one, and a comment over its size keeps every row and names what it left
// out.
func TestVerifyObservedBoundsTheCalls(t *testing.T) {
	calls := make([]VerifyCitedCall, 0, VerifyCitedCallsMax+3)
	for i := range VerifyCitedCallsMax + 3 {
		calls = append(calls, VerifyCitedCall{Seq: int64(i + 1), Verb: "http", Input: fmt.Sprintf("GET /p/%d", i),
			Output: strings.Repeat("o", 900)})
	}
	s := VerifySection{Observed: true}
	for i := range 20 {
		s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o-%d", i), Statement: "x",
			Result: OutcomeResultPassed, Reason: "seen-to-hold", Cited: calls, Note: strings.Repeat("n", 600)})
	}
	one := VerifySection{Observed: true, Outcomes: s.Outcomes[:1]}
	out, err := renderVerifySection(&one)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "Call "); n != VerifyCitedCallsMax {
		t.Errorf("%d calls shown, want %d", n, VerifyCitedCallsMax)
	}
	big, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if len(big) > verifySectionMaxBytes {
		t.Errorf("section is %d bytes, over %d", len(big), verifySectionMaxBytes)
	}
	for i := range 20 {
		if !strings.Contains(big, fmt.Sprintf("`o-%d` (`change`): seen to hold by maul's evaluator; it cites calls 1,", i)) {
			t.Errorf("row o-%d is not shown whole", i)
		}
	}
	if !strings.Contains(big, evidenceCutLead) || !strings.Contains(big, "cited calls") {
		t.Errorf("the section does not say it left cited calls out:\n%.400s", big[len(big)-400:])
	}
}
