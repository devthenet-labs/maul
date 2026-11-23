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
// of char at their character bounds, and every changed path and prior
// result the prompt shows.
func maxEvaluatePrompt(t *testing.T, workspace bool, char string) EvaluatePrompt {
	t.Helper()
	p := testEvaluatePrompt()
	if workspace {
		p.Components, p.Tools.Browser, p.Tools.LookAroundRoutes = nil, false, 0
		p.Workspace = &EvaluateWorkspace{Surface: SurfaceLibrary, Language: "go"}
	}
	p.Request = strings.Repeat("r", RequestQuoteMaxBytes)
	// A cut diff and its note at their widest: every count, repositories
	// and files at their bounds, the diff with its files within
	// FixDiffMaxBytes, as the controller keeps them.
	p.FixDiff = strings.Repeat("d", FixDiffMaxBytes-FixDiffFilesMaxBytes)
	p.FixDiffCut = FixDiffCut{ShownBytes: len(p.FixDiff), ReadBytes: 1 << 30, MoreFiles: 1 << 30}
	for i := range 2 * fixDiffReposMax {
		repo := fmt.Sprintf("%02d", i) + strings.Repeat(char, 100)
		p.FixDiffCut.ForgeCut = append(p.FixDiffCut.ForgeCut, repo)
		p.FixDiffCut.Unavailable = append(p.FixDiffCut.Unavailable, repo)
	}
	for i := range FixDiffFilesMaxBytes {
		p.FixDiffCut.Files = append(p.FixDiffCut.Files, fmt.Sprintf("%04d", i)+strings.Repeat(char,
			FixDiffPathMaxBytes))
	}
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
	p.PriorVerdicts = nil
	for i := range acceptance.MaxOutcomes {
		p.PriorVerdicts = append(p.PriorVerdicts, PriorVerdict{Outcome: fmt.Sprintf("o-%d", i), Result: "unsure",
			Reason: strings.Repeat("v", priorReasonMaxBytes)})
	}
	return p
}

// shedBound is the bound the shedding tests hold a prompt to: under half of
// EvaluatePromptMaxBytes (the bound one argument of a command line has,
// which the evaluate prompt was held to while codex took it as one), so
// their fixtures, every part at its own bound, reach it; at
// EvaluatePromptMaxBytes they fit whole (TestEvaluatePromptHeadroom).
const shedBound = 128<<10 - 1

// TestEvaluatePromptFitsItsBound: with the request and the fix diff at their
// bounds and every part it may leave out past its own, the evaluate prompt
// still renders within its bound (here shedBound). What it leaves out to
// fit, it says it left out: changed paths; never the request, the fix diff
// (the guard against a fix that special-cases the calls a failure cited),
// an outcome or a previous round's result.
func TestEvaluatePromptFitsItsBound(t *testing.T) {
	for _, ws := range []bool{false, true} {
		p := maxEvaluatePrompt(t, ws, "a")
		// A few of the previous round's results (fewer verdicts beside the
		// cut fix diff's note), so the parts that always stay fit beside the
		// request and the fix diff at their bounds.
		p.PriorVerdicts = p.PriorVerdicts[:2]
		got, err := renderEvaluateWithin(p, shedBound)
		if err != nil {
			t.Fatalf("workspace %v: %v", ws, err)
		}
		if len(got) > shedBound {
			t.Errorf("workspace %v: %d bytes, over %d", ws, len(got), shedBound)
		}
		for _, want := range []string{"more paths]", strings.Repeat("r", RequestQuoteMaxBytes), p.FixDiff,
			notWhole} {
			if !strings.Contains(got, want) {
				t.Errorf("workspace %v: the prompt does not hold %.40q", ws, want)
			}
		}
		for _, o := range p.Outcomes {
			if !strings.Contains(got, o.ID+" (a change)\n   "+o.Outcome) {
				t.Errorf("workspace %v: outcome %s is not shown whole", ws, o.ID)
			}
		}
		if strings.Count(got, "was judged unsure. Why, quoted: ") != len(p.PriorVerdicts) {
			t.Errorf("workspace %v: a previous round's result is left out", ws)
		}
	}
}

// TestEvaluatePromptShedsOnlyWhatItMust: a prompt within the bound leaves
// out nothing; one just over it, only the newer half of the changed paths,
// counted. Shown on shedBound, which a fix diff within its own can reach.
func TestEvaluatePromptShedsOnlyWhatItMust(t *testing.T) {
	p := testEvaluatePrompt()
	p.Request = strings.Repeat("r", RequestQuoteMaxBytes)
	p.PriorRound = 1
	for i := range acceptance.MaxOutcomes {
		p.PriorVerdicts = append(p.PriorVerdicts, PriorVerdict{Outcome: fmt.Sprintf("o-%d", i), Result: "unsure",
			Reason: strings.Repeat("v", priorReasonMaxBytes)})
	}
	p.ChangedPaths = nil
	for i := range ChangedPathsMax {
		p.ChangedPaths = append(p.ChangedPaths, ChangedPath{Path: fmt.Sprintf("%03d/", i) +
			strings.Repeat("p", changedPathMaxBytes-4), Added: 1})
	}
	p.FixDiff = "d"
	small, err := renderEvaluateWithin(p, shedBound)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(small, "more paths]") {
		t.Fatal("a prompt within the bound left something out")
	}
	// 2000 bytes over: half the paths (about 30000 bytes) are enough.
	p.FixDiff = strings.Repeat("d", shedBound-len(small)+2000)
	if len(p.FixDiff) > FixDiffMaxBytes {
		t.Fatalf("the fixture cannot reach the bound: a %d-byte fix diff", len(p.FixDiff))
	}
	got, err := renderEvaluateWithin(p, shedBound)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > shedBound {
		t.Fatalf("%d bytes, over %d", len(got), shedBound)
	}
	if !strings.Contains(got, "[and 100 more paths]") || !strings.Contains(got, "099/") ||
		strings.Contains(got, "100/") {
		t.Error("not exactly the later half of the changed paths was left out")
	}
}

// TestEvaluatePromptRefusesOverTheBound: the request, the fix diff,
// outcomes and a previous round's results are never left out, so a prompt
// over its bound with every other part left out (here: every part at its
// bound, held to one byte under what it always keeps) is refused, never
// cut: the run fails as maul's own failure and the round is never judged on
// a prompt without them. At exactly what it always keeps, it renders.
func TestEvaluatePromptRefusesOverTheBound(t *testing.T) {
	for _, char := range []string{"a", wide} {
		for _, ws := range []bool{false, true} {
			p := maxEvaluatePrompt(t, ws, char)
			kept := alwaysKept(t, p)
			if got, err := renderEvaluateWithin(p, kept-1); err == nil {
				t.Errorf("%q, workspace %v: rendered %d bytes within %d, want a refusal", char, ws, len(got),
					kept-1)
			}
			if got, err := renderEvaluateWithin(p, kept); err != nil || len(got) != kept {
				t.Errorf("%q, workspace %v: within %d, got %d bytes, %v; want what it always keeps", char, ws,
					kept, len(got), err)
			}
		}
	}
}

// evaluateHeadroomMin is the least room the parts an evaluate prompt always
// keeps must leave under its bound: a prompt addition that eats into it
// fails here, not in a live render.
const evaluateHeadroomMin = 8 << 10

// alwaysKept is the size of p rendered with every part it may leave out
// left out as shedOne leaves them out, its notices included: what
// RenderEvaluatePrompt can never shed.
func alwaysKept(t *testing.T, p EvaluatePrompt) int {
	t.Helper()
	p.ChangedPaths, p.shed = slices.Clone(p.ChangedPaths), evaluateShed{}
	for p.shedOne() {
	}
	s, err := renderEvaluate(p)
	if err != nil {
		t.Fatal(err)
	}
	return len(s)
}

// TestEvaluatePromptHeadroom: every part at its bound, 4-byte characters
// included, renders with no refusal, and what it always keeps leaves at
// least evaluateHeadroomMin under EvaluatePromptMaxBytes, the one bound:
// every harness running the evaluate stage, claude and codex alike, reads
// the prompt on its standard input.
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
			t.Logf("%q, workspace %v: always kept %d bytes, %d under the bound", char, ws, n,
				EvaluatePromptMaxBytes-n)
			if n > EvaluatePromptMaxBytes-evaluateHeadroomMin {
				t.Errorf("%q, workspace %v: the parts always kept are %d bytes, %d under the bound, want at "+
					"least %d", char, ws, n, EvaluatePromptMaxBytes-n, evaluateHeadroomMin)
			}
		}
	}
}
