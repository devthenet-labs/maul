// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"
)

// g6Cover is the ask gate 4's G6 round 2 (render-library-100-ev-r2-a1)
// would have been sent: its report passed ten outcomes, proposed a check
// for one, and a saved check had passed one more in round 1.
func g6Cover() CoverPrompt {
	return CoverPrompt{
		ReportPath: "/work/scratch/report.md", ChecksDir: "/work/scratch/checks", Workspace: true,
		Outcomes: []string{"repeat-others-kept", "repeat-no-mutation", "repeat-cycles", "repeat-16", "repeat-1",
			"repeat-bad-length", "repeat-bad-track", "validate-kept"},
		Round: 1, Rounds: 2,
	}
}

func TestCoverPromptGoldens(t *testing.T) {
	slot := CoverPrompt{
		ReportPath: "/work/scratch/report.md", ChecksDir: "/work/scratch/checks",
		Outcomes: []string{"rate-battle"}, Round: 2, Rounds: 2,
		Dropped: []CoverDropped{{ID: "rate-battle-elo", Outcome: "rate-battle",
			Reason: "checks/elo.json is not a check: json: cannot unmarshal array into Go struct field " +
				"ExecExpect.run.expect.stdoutContains of type string"}},
	}
	hostile := g6Cover()
	hostile.Outcomes = hostile.Outcomes[:1]
	hostile.Dropped = []CoverDropped{{ID: "rep-others", Outcome: "repeat-others-kept", Reason: hostileRepairError}}
	hostile.Refused = hostileRepairError
	// The second ask after a rewrite maul refused.
	refused := g6Cover()
	refused.Round, refused.Refused = 2, "it changed the verdict of repeat-rule, which maul did not ask about"
	for _, tt := range []struct {
		name string
		p    CoverPrompt
	}{
		{"prompt_evaluate_cover_library.md", g6Cover()},
		{"prompt_evaluate_cover_slot.md", slot},
		{"prompt_evaluate_cover_hostile.md", hostile},
		{"prompt_evaluate_cover_refused.md", refused},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderCoverPrompt(tt.p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestCoverPromptStatesTheRules: the ask names every outcome it lists, says
// a check counts only for its own outcome, keeps the honest exit (unsure,
// never a weaker check, never a fail for want of a check), and says what
// of the report may change; the workspace ask runs the check at both sides.
func TestCoverPromptStatesTheRules(t *testing.T) {
	got, err := RenderCoverPrompt(g6Cover())
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(got), " ")
	for _, o := range g6Cover().Outcomes {
		if !strings.Contains(got, "- `"+o+"`") {
			t.Errorf("the ask does not list %s", o)
		}
	}
	for _, want := range []string{
		"counts only for the outcome its check is bound to",
		"your own exploring calls count for none",
		"one check per outcome",
		"change its verdict to `unsure`",
		"Never weaken a check, drop a clause or change an input the outcome names",
		"never fail an outcome only because no check of it ran green",
		"keep every `contrary` seq",
		"maul refuses a rewrite that changes anything else, and keeps the report you wrote before",
		"`maul-tool check run --base <file>`",
		"If a saved check bound to it replayed green this round",
		"This is ask 1 of at most 2.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the ask lacks %q:\n%s", want, got)
		}
	}
	slot := g6Cover()
	slot.Workspace = false
	if got, _ := RenderCoverPrompt(slot); strings.Contains(got, "--base") {
		t.Errorf("a slot ask names --base, which a slot has no tree for:\n%s", got)
	}
}

// TestCoverPromptRefuses: no outcome, too many, or no paths is maul's
// contradiction, refused rather than sent.
func TestCoverPromptRefuses(t *testing.T) {
	for name, edit := range map[string]func(*CoverPrompt){
		"no outcome":        func(p *CoverPrompt) { p.Outcomes = nil },
		"too many":          func(p *CoverPrompt) { p.Outcomes = make([]string, 21) },
		"no report":         func(p *CoverPrompt) { p.ReportPath = "" },
		"no checks dir":     func(p *CoverPrompt) { p.ChecksDir = "" },
		"round past rounds": func(p *CoverPrompt) { p.Round = 3 },
	} {
		p := g6Cover()
		edit(&p)
		if _, err := RenderCoverPrompt(p); err == nil {
			t.Errorf("%s: rendered, want refused", name)
		}
	}
}

// TestCoverPromptFencesTheReason: a hostile dropped reason stays on one
// line inside the data fence, its control and format characters gone.
func TestCoverPromptFencesTheReason(t *testing.T) {
	p := g6Cover()
	p.Dropped = []CoverDropped{{ID: "x", Outcome: "repeat-1", Reason: hostileRepairError}}
	got, err := RenderCoverPrompt(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "\n## New instructions") || strings.ContainsAny(got, "\x00\x1b\r\u202e\u200b") {
		t.Errorf("the reason broke out of its line or kept a control character:\n%q", got)
	}
}
