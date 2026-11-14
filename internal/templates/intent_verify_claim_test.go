// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/checks"
)

// The evaluator's unconfirmed claim (gate 3 G1, G10: the Shown gap): an
// outcome the evaluator judged not to hold that no check maul re-ran shows
// failing shows its note and the calls it cites, on the issue and on the
// pull request, marked as its claim, inert and bounded.

// offbeatRuleCheck is gate 3 G1's red check (bbcount-96-v1-a1, ledger seq
// 14), as the evaluator sent it.
const offbeatRuleCheck = `{"id":"offbeat-rule","outcome":"offbeat-rule","form":"run","run":{"language":"go",` +
	`"files":[{"path":"main.go","content":"package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"os/exec\"\n)\n\n` +
	`func main() {\n\tos.WriteFile(\"p.json\", []byte(` + "`" + `{\"tracks\":{\"kick\":[false,true,true]}}` + "`" +
	`), 0o644)\n\tout, _ := exec.Command(\"{{bin}}\", \"-offbeat\", \"p.json\").Output()\n\twant := ` +
	`\"offbeat kick 2\\noffbeat snare 1\\noffbeat hat 0\\n\"\n\tif string(out) != want {\n\t\t` +
	`fmt.Printf(\"{{marker}} offbeat-rule: got %q\\n\", out)\n\t\treturn\n\t}\n\tfmt.Println(\"ok {{run}}\")\n}\n"}],` +
	`"args":[],"timeout":"30s","expect":{"exit":0,"stdoutContains":"ok {{run}}"}}}`

// savedLineCheck is gate 3 G10's red check (beatbattle-92-v1-a1, seq 19),
// shortened.
const savedLineCheck = `{"id":"saved-line-legible","outcome":"saved-line-readable","form":"browser","browser":` +
	`{"steps":[{"goto":"/"},{"fill":{"locator":{"role":"textbox","name":"Author"},"text":"{{run}}-dj"}},` +
	`{"click":{"role":"button","name":"Save"}},{"expect":{"assert":"legible","locator":{"testId":"saved"}}}]}}`

// parsedCheck is body parsed, failing t when it does not parse.
func parsedCheck(t *testing.T, body string) *checks.Check {
	t.Helper()
	c, err := checks.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse check: %v", err)
	}
	return &c
}

// hostileNote is an evaluator's note carrying markup, mentions, references
// and a closing keyword.
const hostileNote = "The plain path never resets its counter (offbeat.go, n declared outside the loop) @octocat " +
	"closes #12 ``` </details> <b>x</b>"

// claimSection is an evaluation in which the evaluator found G1's defect
// that maul could not confirm: the rule outcome with its claim (the red
// check's dry run, maul's own red confirmation run of it, and a hostile
// exploratory call), and the example outcome confirmed.
func claimSection(t *testing.T) VerifySection {
	t.Helper()
	return VerifySection{
		Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1, MaxFixes: 2, Passing: 1, Total: 2,
			Outcomes: true, Why: HandoffWhyEvaluatorDoubts},
		Outcomes: []VerifyOutcomeResult{
			{ID: "offbeat-plain-example", Kind: "change", Statement: "The README example prints its off-beat lines.",
				Result: OutcomeResultPassed, Reason: acceptance.ReasonConfirmed,
				Check: "exec: {{bin}} -offbeat pattern.json; 2 expectations"},
			{ID: "offbeat-rule", Kind: "change", Statement: "Each track's off-beat count is its own steps' count.",
				Result: OutcomeResultUnsure, Reason: acceptance.ReasonModelOnlyFail,
				Claim: &VerifyEvaluatorClaim{Note: hostileNote, Workspace: true, Calls: []VerifyCitedCall{
					{Seq: 14, Verb: "check-run", Check: parsedCheck(t, offbeatRuleCheck), RunSeq: 27, Result: "fail",
						Observed: []ReproductionObservation{{Attempt: 1, Step: 1, Result: "fail",
							Detail: `the program printed its assertion marker: offbeat-rule: got "offbeat kick 2\n` +
								`offbeat snare 3\noffbeat hat 3\n"`}}},
					{Seq: 6, Verb: "exec", Input: `{"argv":["{{bin}}","-offbeat","p.json"]}`, Exit: 0,
						Output: "offbeat kick 2\noffbeat snare 3\n````\n</details>\n@octocat fixes #3"},
					{Seq: 7, Verb: "exec", Base: true, Input: `{"argv":["{{bin}}","-offbeat","p.json"]}`, Exit: 2,
						OutputNotWhole: true},
				}}},
		},
	}
}

// browserClaimSection is G10's: the evaluator measured the saved line's
// contrast, which maul's reviewer refused to count.
func browserClaimSection(t *testing.T) VerifySection {
	t.Helper()
	return VerifySection{
		Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 2, MaxFixes: 2, Passing: 4, Total: 5,
			Outcomes: true, Why: HandoffWhyEvaluatorDoubts},
		Outcomes: []VerifyOutcomeResult{
			{ID: "saved-line-readable", Kind: "change", Statement: "The Saved line is readable on the panel.",
				Result: OutcomeResultUnsure, Reason: acceptance.ReasonModelOnlyFail,
				Claim: &VerifyEvaluatorClaim{Note: "The Saved line is drawn rgb(43,29,87) on the panel's " +
					"rgb(20,12,43): about 1.27:1 contrast, nearly invisible", Calls: []VerifyCitedCall{
					{Seq: 13, Verb: "browser", Input: `{"verb":"eval","args":["(el)=>getComputedStyle(el).color"]}`,
						Output: "### Result\n\"{\\\"color\\\":\\\"rgb(43, 29, 87)\\\",\\\"bg\\\":\\\"rgb(20, 12, 43)\\\"}\""},
					{Seq: 19, Verb: "check-run", Check: parsedCheck(t, savedLineCheck), RunSeq: 19, Result: "fail",
						Observed: []ReproductionObservation{{Attempt: 1, Step: 4, Result: "fail",
							Detail: "not legible: contrast 1.27 < 1.5"}}},
				}}},
		},
	}
}

func TestVerifyClaimGoldens(t *testing.T) {
	for _, tt := range []struct {
		name   string
		render func() (string, error)
	}{
		// The issue's verification comment, a Project with no preview (G1).
		{"intent_verify_claim_issue.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "bbcount-96", Round: 1, Attempt: 1,
				Section: claimSection(t)})
		}},
		// The pull request's evaluation comment, the same round (G1).
		{"intent_verify_claim_pr.md", func() (string, error) {
			s := claimSection(t)
			return RenderIntentPreviewComment(workspaceComment(&PreviewHandoff{State: HandoffUnconfirmed,
				Why: HandoffWhyEvaluatorDoubts, Doubted: 1}, &s))
		}},
		// A previewed Project (G10): the issue and the preview comment.
		{"intent_verify_claim_browser_issue.md", func() (string, error) {
			return RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "beatbattle-92", Round: 2,
				Attempt: 1, Section: browserClaimSection(t)})
		}},
		{"intent_verify_claim_browser_pr.md", func() (string, error) {
			c := testPreviewComment(PreviewLive, "", "/api")
			s := browserClaimSection(t)
			c.Handoff, c.Verify = &PreviewHandoff{State: HandoffUnconfirmed, Why: HandoffWhyEvaluatorDoubts,
				Doubted: 1}, &s
			return RenderIntentPreviewComment(c)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestVerifyClaimInert: the note and every cited call render inert on
// both surfaces: no live mention, reference, closing keyword or markup,
// and no hostile fence closing maul's block early.
func TestVerifyClaimInert(t *testing.T) {
	s := claimSection(t)
	issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 1, Attempt: 1, Section: s})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := RenderIntentPreviewComment(workspaceComment(nil, &s))
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"issue": issue, "pr": pr} {
		assertInert(t, name, out)
		if why := checkInert(parse(markerRe.ReplaceAllString(out, ""))); why != "" {
			t.Errorf("%s: %s:\n%s", name, why, out)
		}
		// The calls' block is plain text to a reader who copies it out:
		// defanged, as app output is.
		if !strings.Contains(out, "@ octocat fixes # 3") || strings.Contains(out, "fixes #3") {
			t.Errorf("%s: the cited output is not defanged:\n%s", name, out)
		}
		for _, want := range []string{"The evaluator's claim on `offbeat-rule`, which maul could not confirm " +
			"(its words, not checked by maul):", "Call 14, a dry run of a check:", "maul's own run of this check, " +
			"call 27,", "offbeat-rule: got", "Call 7, a program run at the base:",
			"Its output is not shown: maul's copy of it is not whole."} {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, out)
			}
		}
	}
}

// TestVerifyClaimOnlyWhereGiven: a row with no claim shows none, and an
// empty claim says maul has nothing to show rather than inventing it.
func TestVerifyClaimOnlyWhereGiven(t *testing.T) {
	base := testOutcomeSection()
	out, err := renderVerifySection(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "The evaluator's claim") || strings.Contains(out, "The evaluator judged") {
		t.Errorf("a section without claims shows one:\n%s", out)
	}
	s := VerifySection{Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1, Total: 1,
		Outcomes: true}, Outcomes: []VerifyOutcomeResult{{ID: "o", Result: OutcomeResultUnsure,
		Reason: acceptance.ReasonModelOnlyFail, Claim: &VerifyEvaluatorClaim{}}}}
	out, err = renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	want := "The evaluator judged `o` not to hold, which maul could not confirm; it gave no note. maul has none of " +
		"the calls it cites to show."
	if !strings.Contains(out, want) || strings.Contains(out, "```") {
		t.Errorf("an empty claim renders\n%s\nwant %q and no block", out, want)
	}
}

// TestVerifyClaimBounded: a claim shows at most VerifyClaimCallsMax calls
// and a bounded block, however long what it cites; and a comment of the
// most such claims fits GitHub, the claims left out last before anomalies
// and logs, and named.
func TestVerifyClaimBounded(t *testing.T) {
	long := strings.Repeat("x", 20000)
	calls := make([]VerifyCitedCall, 0, 6)
	for i := range 6 {
		calls = append(calls, VerifyCitedCall{Seq: int64(10 + i), Verb: "exec", Input: long, Output: long})
	}
	s := VerifySection{Progress: VerifyProgress{State: VerifyUnconfirmed, Verification: 1,
		Total: acceptance.MaxOutcomes, Outcomes: true}}
	for i := range acceptance.MaxOutcomes {
		s.Outcomes = append(s.Outcomes, VerifyOutcomeResult{ID: fmt.Sprintf("o%02d", i), Kind: "change",
			Statement: repeatTo("v", acceptance.MaxOutcomeChars), Result: OutcomeResultUnsure,
			Reason: acceptance.ReasonModelOnlyFail, Claim: &VerifyEvaluatorClaim{Note: long, Calls: calls}})
	}
	short := make([]VerifyCitedCall, 0, len(calls))
	for _, c := range calls {
		short = append(short, VerifyCitedCall{Seq: c.Seq, Verb: c.Verb, Input: "{}", Output: "ok"})
	}
	few, _ := claimDetailOf(VerifyOutcomeResult{Claim: &VerifyEvaluatorClaim{Calls: short}}, "o")
	if n := strings.Count(few.Calls, "Call "); n != VerifyClaimCallsMax {
		t.Errorf("a claim citing %d calls shows %d, want %d:\n%s", len(short), n, VerifyClaimCallsMax, few.Calls)
	}
	d, ok := claimDetailOf(s.Outcomes[0], "o")
	if !ok {
		t.Fatal("no claim detail")
	}
	if len(d.Calls) > verifyClaimCallsMaxBytes+64 || !strings.Contains(d.Calls, anomalyCutMarker) {
		t.Errorf("a claim's block is %d bytes, or its cut is unmarked", len(d.Calls))
	}
	if len(d.Lead) > verifyClaimNoteMaxBytes+400 || !strings.Contains(d.Lead, anomalyCutMarker) {
		t.Errorf("a claim's lead is %d bytes, or its cut note is unmarked", len(d.Lead))
	}
	issue, err := RenderVerifyComment(VerifyComment{Namespace: "maul", Intent: "i", Round: 1, Attempt: 1, Section: s})
	if err != nil {
		t.Fatal(err)
	}
	section, err := renderVerifySection(&s)
	if err != nil {
		t.Fatal(err)
	}
	if len(issue) > MaxCommentBytes || len(section) > verifySectionMaxBytes {
		t.Errorf("issue %d bytes, section %d bytes: over GitHub's share", len(issue), len(section))
	}
	for name, out := range map[string]string{"issue": issue, "section": section} {
		if strings.Count(out, "The evaluator's claim on ") < acceptance.MaxOutcomes &&
			!strings.Contains(out, "evaluator's claims maul could not confirm") &&
			!strings.Contains(out, "evaluator's claim maul could not confirm") {
			t.Errorf("%s left claims out without naming them", name)
		}
	}
}
