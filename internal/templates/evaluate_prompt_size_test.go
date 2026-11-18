// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// wide is a 4-byte character: an outcome's characters at their widest.
const wide = "\U0001F600"

// maxEvaluatePrompt is an evaluate prompt with every part the handoff
// carries at its bound: the request and the fix diff at theirs, 20 outcomes
// of char at their character bounds, every changed path, prior result,
// replay, refusal and not-yet-proof the prompt shows, and refused bodies
// past theirs.
func maxEvaluatePrompt(t *testing.T, workspace bool, char string) EvaluatePrompt {
	t.Helper()
	p := testEvaluatePrompt()
	if workspace {
		p.Components, p.Tools.Browser, p.Tools.LookAroundRoutes = nil, false, 0
		p.Workspace = &EvaluateWorkspace{Surface: SurfaceLibrary, Language: "go"}
	}
	p.Request = strings.Repeat("r", RequestQuoteMaxBytes)
	p.FixDiff = strings.Repeat("d", FixDiffMaxBytes)
	p.Outcomes = nil
	for i := range acceptance.MaxOutcomes {
		p.Outcomes = append(p.Outcomes, acceptance.PlanOutcome{ID: fmt.Sprintf("o-%d", i),
			Outcome: strings.Repeat(char, acceptance.MaxOutcomeChars), From: strings.Repeat(char, acceptance.MaxFromChars)})
	}
	p.ChangedPaths = nil
	for i := range ChangedPathsMax {
		p.ChangedPaths = append(p.ChangedPaths, ChangedPath{Path: fmt.Sprintf("%03d", i) +
			strings.Repeat("p", changedPathMaxBytes), Added: 99999, Deleted: 99999})
	}
	p.PriorRound = 1
	p.PriorVerdicts, p.PriorReplay, p.PriorRefused, p.NotYetProof = nil, nil, nil, nil
	for i := range acceptance.MaxOutcomes {
		p.PriorVerdicts = append(p.PriorVerdicts, PriorVerdict{Outcome: fmt.Sprintf("o-%d", i), Result: "unsure",
			Reason: strings.Repeat("v", priorReasonMaxBytes)})
	}
	for i := range priorReplaysMax {
		p.PriorReplay = append(p.PriorReplay, PriorReplay{Check: strings.Repeat("c", 70) + fmt.Sprint(i),
			Outcome: strings.Repeat("o", 64), Result: "pass"})
		p.NotYetProof = append(p.NotYetProof, NotYetProof{Check: strings.Repeat("c", 70) + fmt.Sprint(i),
			Outcome: strings.Repeat("o", 64)})
	}
	for i := range PriorRefusedMax {
		p.PriorRefused = append(p.PriorRefused, PriorRefusal{Check: fmt.Sprintf("o-1.c%d", i), Outcome: "o-1",
			Reason: strings.Repeat("w", priorReasonMaxBytes), Body: bigCheck(t, i, 7000)})
	}
	return p
}

// TestEvaluatePromptFitsItsBound: on a harness that takes the prompt as one
// argument of its command line (OnArgv: codex), with the request and the
// fix diff at their bounds and every part it may leave out past its own, the
// evaluate prompt still renders within EvaluatePromptArgvMaxBytes, under
// the 128 KiB Linux allows one argument. What it leaves out to fit, it says
// it left out: the refused checks' bodies first, then changed paths, then
// the oldest refusals; never the request, the fix diff (the guard against a
// fix that special-cases a check), an outcome or a previous round's result.
func TestEvaluatePromptFitsItsBound(t *testing.T) {
	if EvaluatePromptArgvMaxBytes >= 128<<10 {
		t.Fatalf("EvaluatePromptArgvMaxBytes = %d, not under 128 KiB (MAX_ARG_STRLEN, its NUL included)",
			EvaluatePromptArgvMaxBytes)
	}
	for _, ws := range []bool{false, true} {
		p := maxEvaluatePrompt(t, ws, "a")
		p.OnArgv = true
		// A quarter of the previous round's results, so the parts that always
		// stay fit beside the request and the fix diff at their bounds.
		p.PriorVerdicts, p.PriorReplay, p.NotYetProof = p.PriorVerdicts[:5], p.PriorReplay[:10], p.NotYetProof[:10]
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatalf("workspace %v: %v", ws, err)
		}
		if len(got) > EvaluatePromptArgvMaxBytes {
			t.Errorf("workspace %v: %d bytes, over %d", ws, len(got), EvaluatePromptArgvMaxBytes)
		}
		for _, want := range []string{"more paths]", "earlier refused checks are not shown",
			strings.Repeat("r", RequestQuoteMaxBytes), strings.Repeat("d", FixDiffMaxBytes)} {
			if !strings.Contains(got, want) {
				t.Errorf("workspace %v: the prompt does not hold %.40q", ws, want)
			}
		}
		if strings.Contains(got, "The refused check, as") {
			t.Errorf("workspace %v: a refused body is shown though later parts were left out", ws)
		}
		for _, o := range p.Outcomes {
			if !strings.Contains(got, o.ID+" (a change)\n   "+o.Outcome) {
				t.Errorf("workspace %v: outcome %s is not shown whole", ws, o.ID)
			}
		}
		if strings.Count(got, "was judged unsure. Why, quoted: ") != len(p.PriorVerdicts) ||
			strings.Count(got, "replayed pass") != len(p.PriorReplay) {
			t.Errorf("workspace %v: a previous round's result is left out", ws)
		}
	}
}

// TestEvaluatePromptShedsOnlyWhatItMust: a prompt within the bound leaves
// out nothing; one just over it, only the oldest refused body. Shown on the
// command line's bound, which a fix diff within its own can reach.
func TestEvaluatePromptShedsOnlyWhatItMust(t *testing.T) {
	p := testEvaluatePrompt()
	p.OnArgv = true
	p.Request = strings.Repeat("r", RequestQuoteMaxBytes)
	p.PriorRound = 1
	for i := range acceptance.MaxOutcomes {
		p.PriorVerdicts = append(p.PriorVerdicts, PriorVerdict{Outcome: fmt.Sprintf("o-%d", i), Result: "unsure",
			Reason: strings.Repeat("v", priorReasonMaxBytes)})
	}
	for i := range 4 {
		p.PriorRefused = append(p.PriorRefused, PriorRefusal{Check: fmt.Sprintf("o-1.c%d", i), Outcome: "o-1",
			Reason: "weak", Body: bigCheck(t, i, 7000)})
	}
	p.FixDiff = "d"
	small, err := RenderEvaluatePrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(small, "Its body is not shown") || strings.Contains(small, "within its size bound") {
		t.Fatal("a prompt within the bound left something out")
	}
	// 2000 bytes over: one body (over 7000 bytes shown) is enough.
	p.FixDiff = strings.Repeat("d", EvaluatePromptArgvMaxBytes-len(small)+2000)
	if len(p.FixDiff) > FixDiffMaxBytes {
		t.Fatalf("the fixture cannot reach the bound: a %d-byte fix diff", len(p.FixDiff))
	}
	got, err := RenderEvaluatePrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > EvaluatePromptArgvMaxBytes {
		t.Fatalf("%d bytes, over %d", len(got), EvaluatePromptArgvMaxBytes)
	}
	if strings.Contains(got, `"id": "o-1.c0"`) || !strings.Contains(got, `"id": "o-1.c3"`) ||
		strings.Count(got, "Its body is not shown.") != 1 {
		t.Error("not only the oldest refused body was left out")
	}
	for _, not := range []string{"more paths]", "earlier refused checks"} {
		if strings.Contains(got, not) {
			t.Errorf("the prompt left out more than the oldest body: %q", not)
		}
	}
}

// TestEvaluatePromptRefusesOverTheBound: the request, the fix diff,
// outcomes and a previous round's results are never left out, so a prompt
// over the command line's bound with every other part left out (here: every
// part at its bound) is refused, never cut: the run fails as maul's own
// failure and the round is never judged on a prompt without them.
func TestEvaluatePromptRefusesOverTheBound(t *testing.T) {
	for _, char := range []string{"a", wide} {
		for _, ws := range []bool{false, true} {
			p := maxEvaluatePrompt(t, ws, char)
			p.OnArgv = true
			if got, err := RenderEvaluatePrompt(p); err == nil {
				t.Errorf("%q, workspace %v: rendered %d bytes, want a refusal", char, ws, len(got))
			}
		}
	}
}

// evaluateHeadroomMin is the least room the parts an evaluate prompt always
// keeps must leave under its bound on a harness that reads it on standard
// input: a prompt addition that eats into it fails here, not in a live
// render.
const evaluateHeadroomMin = 8 << 10

// evaluateArgvHeadroomMin is that room under the command line's bound for
// TestEvaluatePromptFitsItsBound's case (a quarter of the previous round's
// results) on a preview, the larger prompt: 93 bytes when it was set, as
// before the bound followed the harness, since what that prompt always
// keeps is unchanged. Everything at its bound cannot fit there at all
// (TestEvaluatePromptRefusesOverTheBound). On codex, the command line is
// what an evaluate prompt addition must fit.
const evaluateArgvHeadroomMin = 64

// alwaysKept is the size of p rendered with every part it may leave out
// left out as shedOne leaves them out, its notices included: what
// RenderEvaluatePrompt can never shed.
func alwaysKept(t *testing.T, p EvaluatePrompt) int {
	t.Helper()
	p.PriorRefused, p.shed = slices.Clone(p.PriorRefused), evaluateShed{}
	for p.shedOne() {
	}
	s, err := renderEvaluate(p)
	if err != nil {
		t.Fatal(err)
	}
	return len(s)
}

// TestEvaluatePromptHeadroom: on a harness that reads the prompt on its
// standard input (claude), every part at its bound, 4-byte characters
// included, renders with no refusal, and what it always keeps leaves at
// least evaluateHeadroomMin under EvaluatePromptMaxBytes; on the command
// line's bound, TestEvaluatePromptFitsItsBound's case leaves at least
// evaluateArgvHeadroomMin.
func TestEvaluatePromptHeadroom(t *testing.T) {
	for _, char := range []string{"a", wide} {
		for _, ws := range []bool{false, true} {
			p := maxEvaluatePrompt(t, ws, char)
			got, err := RenderEvaluatePrompt(p)
			if err != nil {
				t.Fatalf("%q, workspace %v: %v", char, ws, err)
			}
			if len(got) > EvaluatePromptMaxBytes {
				t.Errorf("%q, workspace %v: %d bytes, over %d", char, ws, len(got), EvaluatePromptMaxBytes)
			}
			n := alwaysKept(t, p)
			t.Logf("%q, workspace %v: standard input: always kept %d bytes, %d under the bound", char, ws, n,
				EvaluatePromptMaxBytes-n)
			if n > EvaluatePromptMaxBytes-evaluateHeadroomMin {
				t.Errorf("%q, workspace %v: the parts always kept are %d bytes, %d under the bound, want at "+
					"least %d", char, ws, n, EvaluatePromptMaxBytes-n, evaluateHeadroomMin)
			}
		}
	}
	for _, ws := range []bool{false, true} {
		p := maxEvaluatePrompt(t, ws, "a")
		p.OnArgv = true
		p.PriorVerdicts, p.PriorReplay, p.NotYetProof = p.PriorVerdicts[:5], p.PriorReplay[:10], p.NotYetProof[:10]
		n := alwaysKept(t, p)
		t.Logf("workspace %v: command line: always kept %d bytes, %d under the bound", ws, n,
			EvaluatePromptArgvMaxBytes-n)
		if n > EvaluatePromptArgvMaxBytes-evaluateArgvHeadroomMin {
			t.Errorf("workspace %v: on the command line, the parts always kept are %d bytes, %d under the "+
				"bound, want at least %d", ws, n, EvaluatePromptArgvMaxBytes-n, evaluateArgvHeadroomMin)
		}
	}
}
