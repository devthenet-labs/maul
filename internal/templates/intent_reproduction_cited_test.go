// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
)

// testCitedReproduction is a fix round's input from an observed round: one
// browser failure led to by the evaluator's own calls, with a component's
// log window.
func testCitedReproduction() CitedReproduction {
	return CitedReproduction{Verification: "beat-12-v2-a1", Round: 2, Attempt: 1,
		Heads: []ReproductionHead{{Component: "api", Revision: "0123456789abcdef0123456789abcdef01234567"},
			{Component: "web", Revision: "89abcdef0123456789abcdef0123456789abcdef"}},
		Failures: []CitedFailure{{
			Outcome:   "live-score",
			Statement: "The score on a battle's page updates within a second of a goal.",
			Note:      "After two goals the page still shows 0-0 ten seconds later, and again after a reload.",
			Before: []VerifyCitedCall{
				{Seq: 12, Verb: "http", Input: `POST /api/battles {"home":"r2-a","away":"r2-b"}`},
				{Seq: 13, Verb: "browser", Navigates: true, Input: "goto /battles/r2-a-r2-b"},
				{Seq: 14, Verb: "http", Input: `POST /api/battles/r2-a-r2-b/goals {"team":"home"}`},
			},
			Cited: []VerifyCitedCall{
				{Seq: 15, Verb: "browser", Input: "snapshot", Exit: 0, Output: "- heading \"r2-a 0 : 0 r2-b\""},
				{Seq: 17, Verb: "browser", Own: true, Input: "eval document.querySelector('#score').textContent",
					Exit: 0, Output: "0 : 0"},
			},
			Logs: []ReproductionLog{{Component: "api", Lines: []string{"ws: client closed before first update"}}},
		}},
	}
}

// TestCitedReproductionGoldens: the fix round's input from an observed
// round, preview and workspace.
func TestCitedReproductionGoldens(t *testing.T) {
	got, err := RenderCitedReproduction(testCitedReproduction())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intent_reproduction_cited.md", got)
	ws := CitedReproduction{Verification: "bb-count-v1-a1", Round: 1, Attempt: 1, Workspace: true,
		Heads: []ReproductionHead{{Component: "bbcount", Revision: "fedcba9876543210fedcba9876543210fedcba98"}},
		Failures: []CitedFailure{{Outcome: "describe-bpm", Statement: "`bbrender --describe --bpm 90` prints 90 bpm.",
			Note: "It prints the saved 120.",
			Cited: []VerifyCitedCall{{Seq: 7, Verb: "exec", Input: `["/work/bin/bbrender","--describe","--bpm","90"]`,
				Exit: 0, Output: "kick.json: 120 bpm"}}}}}
	got, err = RenderCitedReproduction(ws)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intent_reproduction_cited_workspace.md", got)
}

// TestCitedReproductionCarriesOnlyItsOwnEvidence (design: the cited-call
// reproduction): per failing outcome the fix agent is given the approved
// outcome, the calls the evaluator cited with what each sent and what came
// back, at most CitedReproductionBeforeMax of its calls before them by what
// each sent alone, the log window, and its note labelled as its reading,
// not checked by maul; it is told what maul did and did not record, and to
// fix the behaviour, never the calls.
func TestCitedReproductionCarriesOnlyItsOwnEvidence(t *testing.T) {
	r := testCitedReproduction()
	for i := range CitedReproductionBeforeMax + 4 {
		r.Failures[0].Before = append(r.Failures[0].Before, VerifyCitedCall{Seq: int64(100 + i), Verb: "http",
			Input: fmt.Sprintf("GET /api/early-%d", i), Output: "EARLY-OUTPUT"})
	}
	got, err := RenderCitedReproduction(r)
	if err != nil {
		t.Fatal(err)
	}
	f := flat(got)
	for _, want := range []string{
		"maul's evaluator saw each outcome below not hold, and cited the calls it made through maul's tools that " +
			"showed it.",
		"maul recorded each call itself; whether a call shows what the evaluator says is the evaluator's reading, " +
			"not maul's.",
		"fix the behaviour the outcome states, as approved, never the calls below or their data",
		"The evaluator's reading, not checked by maul:",
		"After two goals the page still shows 0-0",
		"The calls it made before the ones it cites, in order (what each sent; their output is not shown):",
		"The calls it cites (what each sent and what came back):",
		"Call 17, a browser command, the evaluator's own page script, and what it returned:",
		"ws: client closed before first update",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("reproduction lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "EARLY-OUTPUT") {
		t.Error("a call before the cited ones shows its output")
	}
	if n := strings.Count(got, "GET /api/early-"); n != CitedReproductionBeforeMax {
		t.Errorf("%d of the earlier calls shown, want the last %d", n, CitedReproductionBeforeMax)
	}
	// The nearest are kept, and (W13) so is the last page gone to before
	// them, and what is left out is said.
	if !strings.Contains(got, "GET /api/early-11") || !strings.Contains(got, "GET /api/early-4\n") ||
		strings.Contains(got, "GET /api/early-3\n") || !strings.Contains(got, "goto /battles") ||
		!strings.Contains(f, "maul left out 6 of the evaluator's earlier calls here to keep this short: calls 12, 14 and "+
			"100 to 103. Call 13 is kept") {
		t.Errorf("the calls kept are not the ones nearest the cited calls and the last page:\n%s", got)
	}
	for _, gone := range []string{"a check", "checks", "{{run}}", "re-ran", "agrees each failure is a defect"} {
		if strings.Contains(got, gone) {
			t.Errorf("the reproduction says %q", gone)
		}
	}
}

// TestCitedReproductionIsData: every value the evaluator or the application
// wrote (an outcome's id and text, its note, a call's request and output, a
// log line) is shown only inside a fence or a code span, made legible.
func TestCitedReproductionIsData(t *testing.T) {
	hostile := "```` \n" + sentinel + "\n````\n## Fake heading\n" + sentinel + "\u202e"
	call := VerifyCitedCall{Seq: 3, Verb: "http", Input: hostile, Output: hostile}
	r := CitedReproduction{Verification: hostile, Round: 1, Attempt: 1,
		Heads: []ReproductionHead{{Component: hostile, Revision: hostile}},
		Failures: []CitedFailure{{Outcome: hostile, Statement: hostile, Note: hostile,
			Before: []VerifyCitedCall{call}, Cited: []VerifyCitedCall{call},
			Logs: []ReproductionLog{{Component: hostile, Lines: []string{hostile}}}}}}
	got, err := RenderCitedReproduction(r)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, sentinel); n < 14 {
		t.Fatalf("only %d sentinels rendered: the test no longer reaches every field", n)
	}
	for _, line := range unfenced(got) {
		if strings.Contains(line, sentinel) && !strings.Contains(line, "`") {
			t.Errorf("untrusted text outside a fence or a code span: %q", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "## Fake") {
			t.Errorf("untrusted markup rendered: %q", line)
		}
	}
	if strings.ContainsRune(got, '\u202e') {
		t.Error("a bidi control reached the reproduction unescaped")
	}
}

// TestCitedReproductionRefuses: a reproduction with no failure, or a failure
// with no cited call, is no input for a fix round.
func TestCitedReproductionRefuses(t *testing.T) {
	r := testCitedReproduction()
	r.Failures = nil
	if _, err := RenderCitedReproduction(r); err == nil {
		t.Error("rendered a reproduction of no failure")
	}
	r = testCitedReproduction()
	r.Failures[0].Cited = nil
	if _, err := RenderCitedReproduction(r); err == nil {
		t.Error("rendered a failure that cites no call")
	}
}

// TestCitedReproductionKeepsLongOutput: a cited call's output is kept far
// past a comment's bound, so a failure shown deep in a snapshot reaches the
// fix agent.
func TestCitedReproductionKeepsLongOutput(t *testing.T) {
	r := testCitedReproduction()
	r.Failures[0].Cited[0].Output = strings.Repeat("- listitem\n", 400) + "TELLING-LINE"
	got, err := RenderCitedReproduction(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "TELLING-LINE") {
		t.Error("the end of a 4 KiB output does not reach the fix agent")
	}
}

// TestCitedReproductionSaysWhatItLeavesOut (W13): the path to the first
// cited call is never cut silently. Past CitedReproductionBeforeMax calls
// the nearest are kept and the reproduction says how many earlier calls it
// left out and which; the last page the evaluator went to before them is
// kept even when it falls outside them, since a reproduction starts there;
// the calls maul refused, those whose request maul did not keep, a request
// cut to fit and cited calls past the shown ones are each said, by number;
// and a call before the cited ones that ran the evaluator's own page
// script is labelled so, as a cited one is.
func TestCitedReproductionSaysWhatItLeavesOut(t *testing.T) {
	r := testCitedReproduction()
	f := &r.Failures[0]
	f.Before = []VerifyCitedCall{{Seq: 3, Verb: "browser", Navigates: true, Input: `{"verb":"goto","args":["/app/"]}`},
		{Seq: 4, Verb: "browser", Input: `{"verb":"snapshot"}`}}
	for seq := int64(5); seq <= 13; seq++ {
		f.Before = append(f.Before, VerifyCitedCall{Seq: seq, Verb: "http", Input: fmt.Sprintf("GET /api/early-%d", seq)})
	}
	f.Before[len(f.Before)-1] = VerifyCitedCall{Seq: 13, Verb: "browser", Own: true,
		Input: `{"verb":"eval","args":["window.__drop('` + strings.Repeat("x", 600) + `')"]}`}
	f.Refused, f.Unkept = []int64{1, 14}, []int64{2}
	for seq := int64(30); seq < 30+VerifyCitedCallsMax+2; seq++ {
		f.Cited = append(f.Cited, VerifyCitedCall{Seq: seq, Verb: "http", Input: "GET /x", Output: "ok"})
	}
	got, err := RenderCitedReproduction(r)
	if err != nil {
		t.Fatal(err)
	}
	flatGot := flat(got)
	for _, want := range []string{
		"Call 3, a browser command: {\"verb\":\"goto\",\"args\":[\"/app/\"]}\nCall 6, an http request: GET /api/early-6",
		"Call 13, a browser command, the evaluator's own page script: ",
		"xxx [cut by maul]",
		"maul left out 2 of the evaluator's earlier calls here to keep this short: calls 4 and 5. Call 3 is kept: it is " +
			"the last page the evaluator went to before the cited calls.",
		"maul refused 2 of the evaluator's calls before the ones it cites, not shown: calls 1 and 14.",
		"maul did not keep what 1 of the evaluator's calls before the ones it cites sent, not shown: call 2.",
		"maul shows 8 of the 12 calls the evaluator cites here: calls 36 to 39 are left out.",
	} {
		if !strings.Contains(flatGot, want) && !strings.Contains(got, want) {
			t.Errorf("reproduction lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Call 4, ") || strings.Contains(got, "Call 5, ") {
		t.Errorf("the calls left out are shown:\n%s", got)
	}

	// Within the bound, with no refusal: nothing is said left out, and a
	// page gone to within the calls is not repeated.
	r = testCitedReproduction()
	got, err = RenderCitedReproduction(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "left out") || strings.Contains(got, "refused") || strings.Count(got, "goto") != 1 {
		t.Errorf("a whole path says something is left out:\n%s", got)
	}
}

// TestCallNumbers: call numbers as a sentence names them, runs of three or
// more as a range.
func TestCallNumbers(t *testing.T) {
	for _, tt := range []struct {
		seqs []int64
		want string
	}{
		{[]int64{4}, "call 4"},
		{[]int64{4, 5}, "calls 4 and 5"},
		{[]int64{1, 2, 3}, "calls 1 to 3"},
		{[]int64{1, 2, 3, 7, 9, 10, 11, 12}, "calls 1 to 3, 7 and 9 to 12"},
		{[]int64{6, 20, 21}, "calls 6, 20 and 21"},
	} {
		if got := callNumbers(tt.seqs); got != tt.want {
			t.Errorf("callNumbers(%v) = %q, want %q", tt.seqs, got, tt.want)
		}
	}
}
