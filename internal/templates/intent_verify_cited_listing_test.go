// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"
)

// bigCitedCalls are n cited calls of one outcome, numbered from first, each
// sending and returning as much as a call may show, its request filled with
// fill: every other one the evaluator's own (a page script when ownVerb is
// browser, else a program), the rest http requests; every one a page script
// of its own when ownVerb is browser-all. Each call's request
// starts with a token naming its seq, its target.
func bigCitedCalls(first int64, n int, fill, ownVerb string) []VerifyCitedCall {
	calls := make([]VerifyCitedCall, 0, n)
	pad := strings.Repeat(fill, 2*verifyClaimInputMaxBytes/len(fill))
	for i := range n {
		seq := first + int64(i)
		c := VerifyCitedCall{Seq: seq, Verb: "http", Input: fmt.Sprintf("GET /tgt-%d/%s", seq, pad),
			Output: strings.Repeat("out ", verifyClaimOutputMaxBytes)}
		if i%2 == 1 || ownVerb == "browser-all" {
			c.Verb, c.Own, c.Exit = strings.TrimSuffix(ownVerb, "-all"), true, 3
			c.Input = fmt.Sprintf(`{"argv":["tgt-%d","%s"]}`, seq, pad)
		}
		calls = append(calls, c)
	}
	return calls
}

// citedHead is how a comment names a cited call, its own label included.
func citedHead(c VerifyCitedCall) string {
	head := fmt.Sprintf("Call %d, %s", c.Seq, verbWords[c.Verb])
	switch {
	case c.Own && c.Verb == "browser":
		head += ", the evaluator's own page script"
	case c.Own:
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
	calls := bigCitedCalls(1, VerifyCitedCallsMax, "a", "exec")
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
// 8 calls of the longest request and output, the requests made of what
// defang and fence lengthen most), the issue comment and the pull request's
// section still render within their bounds, and every cited call of every
// outcome is listed with its seq, verb and own label, and with the start of
// its target where that fits: what fitVerify leaves out is the calls'
// bodies, which the comment says, never the cited section whole, and never
// the whole comment.
func TestFitVerifyListsEveryCitedCall(t *testing.T) {
	for _, tt := range []struct{ name, fill, ownVerb string }{
		{"plain", "a", "exec"},
		{"mentions", "@a ", "browser"},
		{"references", "#1", "browser"},
		{"backticks", "`", "exec"},
		{"invisible", "\u200b", "browser"},
		{"every call a page script of references", "#1", "browser-all"},
		{"every call a page script of mentions", "@a ", "browser-all"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := VerifySection{Observed: true, Progress: VerifyProgress{State: VerifyFailing, Verification: 99,
				FixesUsed: 9, MaxFixes: 9, Passing: 10, Total: 20, Outcomes: true}}
			var all []VerifyCitedCall
			for i := range 20 {
				calls := bigCitedCalls(int64(8*i+1), VerifyCitedCallsMax, tt.fill, tt.ownVerb)
				all = append(all, calls...)
				s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o%02d-%s", i,
					strings.Repeat("x", 28)), Statement: strings.Repeat("The statement @a #1 goes on. ", 17),
					Kind: "change", Result: OutcomeResultFailed, Reason: "seen-not-to-hold",
					Note: strings.Repeat("n", 600), Cited: calls})
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
				var heads []string
				for _, call := range all {
					if !strings.Contains(c.out, citedHead(call)) {
						heads = append(heads, citedHead(call))
					}
				}
				if len(heads) > 0 {
					t.Errorf("%s does not list %d of the cited calls, first %q", c.name, len(heads), heads[0])
				}
				if !strings.Contains(c.out, evidenceCutLead) || !strings.Contains(c.out, "cited call bodies") {
					t.Errorf("%s does not say it left cited call bodies out:\n%.600s", c.name, c.out[len(c.out)-600:])
				}
			}
			if tt.name == "plain" {
				if missing := listsEveryCall(section, all); len(missing) > 0 {
					t.Errorf("the section does not show %d targets, first %q", len(missing), missing[0])
				}
			}
		})
	}
}

// TestFitVerifyFallsBackToCallHeads (FA 4): a section whose listings, with
// short statements and no evidence, are still over the limit (the
// evaluator's requests are its own and only bounded) lists every cited call
// by its head alone, within the limit, rather than refusing the comment.
func TestFitVerifyFallsBackToCallHeads(t *testing.T) {
	s := VerifySection{Observed: true}
	var all []VerifyCitedCall
	for i := range 20 {
		calls := bigCitedCalls(int64(300+8*i+1), VerifyCitedCallsMax, "#1", "browser-all")
		all = append(all, calls...)
		s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o%02d-%s", i, strings.Repeat("x", 28)),
			Statement: strings.Repeat("#1 ", 200), Kind: "change", Result: OutcomeResultUnsure,
			Reason: "held-finding", Cited: calls})
	}
	render := func(v verifyView) (string, error) {
		out, err := render("intent_verify_section.md.tmpl", v)
		return strings.TrimRight(out, "\n"), err
	}
	v := buildVerifyView(s, true)
	ps := v.pieces()
	floor, err := render(v.withEvidence(ps, make([]bool, len(ps))))
	if err != nil {
		t.Fatal(err)
	}
	limit := len(floor) - 1
	out, err := fitVerify(s, limit, render)
	if err != nil {
		t.Fatalf("over the listings' floor (%d bytes): %v", len(floor), err)
	}
	if len(out) > limit {
		t.Errorf("%d bytes, over %d", len(out), limit)
	}
	for _, c := range all {
		if !strings.Contains(out, citedHead(c)) {
			t.Errorf("%q is not listed by its head", citedHead(c))
			break
		}
	}
	if !strings.Contains(out, "what each sent and what came back are left out") {
		t.Errorf("the heads' lead does not say what is left out")
	}
}
