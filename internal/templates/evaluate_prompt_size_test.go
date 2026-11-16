// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
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

// TestEvaluatePromptFitsOneArgument: the evaluate prompt is one argument of
// the CLI's command line, so at every part's bound it still renders within
// EvaluatePromptMaxBytes, under Linux's bound on one argument. What it
// leaves out to fit, it says it left out: the refused checks' bodies first,
// then changed paths, then the oldest refusals, then the fix diff, then the
// request; never an outcome or a previous round's result. (Outcomes of
// 4-byte characters at their bounds can still be too long: see
// TestEvaluatePromptRefusesOverTheBound.)
func TestEvaluatePromptFitsOneArgument(t *testing.T) {
	if EvaluatePromptMaxBytes >= 128<<10 {
		t.Fatalf("EvaluatePromptMaxBytes = %d, not under MAX_ARG_STRLEN (128 KiB with its NUL)",
			EvaluatePromptMaxBytes)
	}
	for _, ws := range []bool{false, true} {
		p := maxEvaluatePrompt(t, ws, "a")
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatalf("workspace %v: %v", ws, err)
		}
		if len(got) > EvaluatePromptMaxBytes {
			t.Errorf("workspace %v: %d bytes, over %d", ws, len(got), EvaluatePromptMaxBytes)
		}
		// At these bounds it gives up the bodies, the paths, the refusals
		// and the fix diff, and keeps the request.
		for _, want := range []string{"more paths]", "earlier refused checks are not shown", "[The diff is not shown",
			strings.Repeat("r", RequestQuoteMaxBytes)} {
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

// TestEvaluatePromptLeavesOutTheRequestLast: outcomes of 4-byte characters
// leave out the request too, last, and say so.
func TestEvaluatePromptLeavesOutTheRequestLast(t *testing.T) {
	p := maxEvaluatePrompt(t, false, wide)
	p.Outcomes = p.Outcomes[:18]
	got, err := RenderEvaluatePrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > EvaluatePromptMaxBytes || !strings.Contains(got, "[The request is not shown") ||
		!strings.Contains(got, "[The diff is not shown") {
		t.Errorf("%d bytes; want within %d with the request and the diff said to be left out", len(got),
			EvaluatePromptMaxBytes)
	}
}

// TestEvaluatePromptShedsOnlyWhatItMust: a prompt within the bound leaves
// out nothing; one just over it, only the oldest refused body.
func TestEvaluatePromptShedsOnlyWhatItMust(t *testing.T) {
	p := testEvaluatePrompt()
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
	p.FixDiff = strings.Repeat("d", EvaluatePromptMaxBytes-len(small)+2000)
	if len(p.FixDiff) > FixDiffMaxBytes {
		t.Fatalf("the fixture cannot reach the bound: a %d-byte fix diff", len(p.FixDiff))
	}
	got, err := RenderEvaluatePrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > EvaluatePromptMaxBytes {
		t.Fatalf("%d bytes, over %d", len(got), EvaluatePromptMaxBytes)
	}
	if strings.Contains(got, `"id": "o-1.c0"`) || !strings.Contains(got, `"id": "o-1.c3"`) ||
		strings.Count(got, "Its body is not shown.") != 1 {
		t.Error("not only the oldest refused body was left out")
	}
	for _, not := range []string{"more paths]", "earlier refused checks", "[The diff is not shown",
		"[The request is not shown"} {
		if strings.Contains(got, not) {
			t.Errorf("the prompt left out more than the oldest body: %q", not)
		}
	}
}

// TestEvaluatePromptRefusesOverTheBound: outcomes and a previous round's
// results are never left out, so a prompt over the bound with every other
// part left out (here: 20 outcomes of 4-byte characters at their bounds)
// is refused, never cut.
func TestEvaluatePromptRefusesOverTheBound(t *testing.T) {
	p := maxEvaluatePrompt(t, false, wide)
	if got, err := RenderEvaluatePrompt(p); err == nil {
		t.Fatalf("rendered %d bytes, want a refusal", len(got))
	}
}
