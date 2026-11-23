// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
)

// checkEraWords are what an evaluate prompt of the observed evaluator never
// says: the saved-check machinery's verbs, keys and placeholders (retired
// with saved checks), each of which would teach a shape maul no longer
// reads. A report key is matched as YAML (`key:`), a verb as maul-tool's.
var checkEraWords = []string{
	"maul-tool check", "check results", "check run", "saved check", "replay", "proposed", "disputes", "retire",
	"contrary", "expect: fail", "{{run}}", "{{marker}}", "Saving checks", "What a check must cover",
	"A check that shows a defect", "reviewer judges", "proves", "/work/scratch/checks",
}

// checkEraKeys are the report keys the observed evaluator's report never
// carries; report.ParseEvaluation still accepts them until the switch is
// complete, so a parse alone cannot catch one.
var checkEraKeys = regexp.MustCompile(`(?m)^\s*-?\s*(disputes|proposed|retire|contrary|check_digest|replacement):`)

// observedVariants are every evaluate prompt variant: the slot round with and
// without its browser and sandbox, after a fix (whole, cut, unread) and after
// a judged round, and the cli and library workspaces.
func observedVariants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	add := func(name string, p EvaluatePrompt) {
		got, err := RenderEvaluatePrompt(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = got
	}
	add("slot", testEvaluatePrompt())
	p := testEvaluatePrompt()
	p.Tools = EvaluateTools{MaxToolCalls: 50}
	add("slot, http only", p)
	p = testEvaluatePrompt()
	p.Tools.Browser = false
	add("slot, sandbox only", p)
	p = testEvaluatePrompt()
	p.Tools.Sandbox = false
	add("slot, browser only", p)
	p = testEvaluatePrompt()
	p.Round, p.PriorRound = 2, 1
	p.FixDiff = "--- a/x\n+++ b/x\n"
	p.PriorVerdicts = []PriorVerdict{{Outcome: "winner-rises", Result: "failed", Reason: "no"}}
	// Retired inputs: the prompt shows none of them.
	p.ChecksDir = "/work/scratch/checks"
	p.PriorReplay = []PriorReplay{{Check: "c", Outcome: "winner-rises", Result: "pass"}}
	p.PriorRefused = []PriorRefusal{{Check: "c", Outcome: "winner-rises", Reason: "weak"}}
	p.NotYetProof = []NotYetProof{{Check: "c", Outcome: "winner-rises"}}
	add("slot, after a fix", p)
	p = testEvaluatePrompt()
	p.Round, p.PriorRound = 2, 1
	p.FixDiffCut = FixDiffCut{Unavailable: []string{"acme/api"}}
	add("slot, fix unread", p)
	add("cli", testWorkspacePrompt(EvaluateWorkspace{Surface: SurfaceCLI, Binary: "/work/bin/bbrender",
		Build: []string{"go", "build", "-o", "/work/bin/bbrender", "./cmd/bbrender"}}))
	add("library", testWorkspacePrompt(EvaluateWorkspace{Surface: SurfaceLibrary, Language: "go"}))
	return out
}

// TestEvaluatePromptHasNoCheckSections: no variant of the observed
// evaluator's prompt, and none of its report examples, teaches the
// saved-check machinery: no check section, verb, report key or placeholder.
func TestEvaluatePromptHasNoCheckSections(t *testing.T) {
	for name, got := range observedVariants(t) {
		for _, word := range checkEraWords {
			if strings.Contains(got, word) {
				t.Errorf("%s: the prompt says %q", name, word)
			}
		}
		if m := checkEraKeys.FindString(got); m != "" {
			t.Errorf("%s: the prompt's report carries %q", name, strings.TrimSpace(m))
		}
	}
	for name, ex := range map[string]string{"slot": ExampleEvaluationReport, "cli": ExampleCLIEvaluationReport,
		"library": ExampleLibraryEvaluationReport} {
		if m := checkEraKeys.FindString(ex); m != "" {
			t.Errorf("%s report example carries %q", name, strings.TrimSpace(m))
		}
		e, err := report.ParseEvaluation([]byte(ex))
		if err != nil {
			t.Fatalf("%s report example: %v", name, err)
		}
		var claims, cited int
		for _, v := range e.Verdicts {
			if v.Verdict == report.VerdictPass || v.Verdict == report.VerdictFail {
				claims++
				if len(v.Ledger) > 0 {
					cited++
				}
			}
			if len(v.Contrary) > 0 {
				t.Errorf("%s report example: %s lists contrary seqs", name, v.Outcome)
			}
		}
		if claims == 0 || cited != claims {
			t.Errorf("%s report example: %d of %d passes and fails cite a call", name, cited, claims)
		}
		if len(e.Disputes)+len(e.Proposed)+len(e.Retire) > 0 {
			t.Errorf("%s report example: %d disputes, %d proposed, %d retired", name, len(e.Disputes),
				len(e.Proposed), len(e.Retire))
		}
	}
}

// TestEvaluatePromptCheckEraWordsSeeOldPrompt: the words
// TestEvaluatePromptHasNoCheckSections looks for are the ones the check-era
// prompt said (its goldens as of v0.14.15, kept beside this test), so the
// test can fail: a word no prompt ever said would pass vacuously.
func TestEvaluatePromptCheckEraWordsSeeOldPrompt(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "checkera_prompt_evaluate.md"))
	if err != nil {
		t.Fatal(err)
	}
	old := string(data)
	var missing []string
	for _, word := range checkEraWords {
		if !strings.Contains(old, word) {
			missing = append(missing, word)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the check-era prompt never said %q: drop them or fix the fixture", missing)
	}
	if checkEraKeys.FindString(old) == "" {
		t.Error("the check-era prompt carries none of the retired report keys")
	}
}

// TestEvaluatePromptTeachesTheCitedEvidence: each variant names exactly the
// calls acceptance.ValidCite counts that its round offers, says the others
// are context, and labels the evaluator's own scripts and programs only
// where the round runs them.
func TestEvaluatePromptTeachesTheCitedEvidence(t *testing.T) {
	vs := observedVariants(t)
	for _, tt := range []struct {
		variant string
		counts  string
		own     string // "" for none
	}{
		{"slot", "a `maul-tool http`, `maul-tool browser` or `maul-tool exec` call that maul did not refuse, and " +
			"for a browser command, one that exited 0", "A page script you run with `eval`, or a program you run " +
			"with `maul-tool exec`, counts too"},
		{"slot, http only", "a `maul-tool http` call that maul did not refuse.", ""},
		{"slot, sandbox only", "a `maul-tool http` or `maul-tool exec` call that maul did not refuse.",
			"A program you run with `maul-tool exec` counts too"},
		{"slot, browser only", "a `maul-tool http` or `maul-tool browser` call that maul did not refuse, and for " +
			"a browser command, one that exited 0", "A page script you run with `eval` counts too"},
		{"cli", "a `maul-tool exec` or `maul-tool run` call at the head that maul did not refuse",
			"A program of yours is shown to the reviewers as your own program and what it printed"},
		{"library", "a `maul-tool run` call at the head that maul did not refuse",
			"It is shown to the reviewers as your own program and what it printed"},
	} {
		flat := strings.Join(strings.Fields(vs[tt.variant]), " ")
		if !strings.Contains(flat, tt.counts) {
			t.Errorf("%s: the prompt does not say what counts: %q", tt.variant, tt.counts)
		}
		if tt.own != "" && !strings.Contains(flat, tt.own) {
			t.Errorf("%s: the prompt does not label the evaluator's own programs: %q", tt.variant, tt.own)
		}
		if tt.own == "" && strings.Contains(flat, "your own script") {
			t.Errorf("%s: the prompt labels a script the round cannot run", tt.variant)
		}
	}
	// Only the first acceptance.MaxCites counted calls of a verdict are kept
	// and shown (W1's cap), so the prompt says so rather than "every call".
	capRule := fmt.Sprintf("Cite at most %d calls for each outcome: the ones that show what you saw, and only those. "+
		"maul keeps the calls of a verdict that count, the first %d by seq,", acceptance.MaxCites, acceptance.MaxCites)
	for name, got := range vs {
		if f := strings.Join(strings.Fields(got), " "); !strings.Contains(f, capRule) ||
			strings.Contains(f, "every call you cite") {
			t.Errorf("%s: the prompt does not state the cite cap: %q", name, capRule)
		}
	}
	for _, ws := range []string{"cli", "library"} {
		flat := strings.Join(strings.Fields(vs[ws]), " ")
		for _, want := range []string{
			"`maul-tool files` calls and every call at the base may be cited beside those, but never count on " +
				"their own.",
			"maul counts only what you saw at the head",
			"to see how the commit the branch started from behaves, if that helps you judge",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("%s: the prompt lacks %q", ws, want)
			}
		}
	}
}
