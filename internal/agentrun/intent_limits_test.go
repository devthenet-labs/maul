// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
)

// ungrantedPlanLimits are what a plan Job with no grant runs on and its
// prompt states: the investigate stage's limits and wall clock.
func ungrantedPlanLimits(cfg Config) templates.StageLimits {
	return templates.StageLimits{
		MaxTurns: cfg.InvestigateMaxTurns, TokenBudget: cfg.InvestigateTokenBudget, Timeout: cfg.InvestigateTimeout,
	}
}

// ungrantedBuildLimits are what a build Job with no grant runs on and its
// prompt states: the automated budget and the remediate stage's wall clock.
func ungrantedBuildLimits(cfg Config) templates.StageLimits {
	return templates.StageLimits{
		MaxTurns: cfg.RemediateAutoMaxTurns, TokenBudget: cfg.RemediateAutoTokenBudget, Timeout: cfg.RemediateTimeout,
	}
}

// argvFlag returns the value after name in argv.
func argvFlag(t *testing.T, argv []string, name string) string {
	t.Helper()
	i := slices.Index(argv, name)
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv lacks %s: %q", name, argv)
	}
	return argv[i+1]
}

// TestPlanPromptStatesItsLimits: the plan prompt states the limits the run
// is actually held to — the grant once the stage's ceiling has clamped it,
// the ceiling with no grant, and the stage's wall clock — not the raw
// grant, and states the build's ceiling apart from them.
func TestPlanPromptStatesItsLimits(t *testing.T) {
	tests := []struct {
		name      string
		granted   [2]int
		wantTurns int
		wantTok   int
	}{
		{"no grant states the stage's limits", [2]int{0, 0}, 25, 150000},
		{"a lower grant is stated", [2]int{10, 50000}, 10, 50000},
		{"a grant above the ceiling states the ceiling", [2]int{400, 9000000}, 25, 150000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := intentConfig(t, PhasePlan, &out) // investigate limits 25/150000
			brokeredClaude(t, &cfg)
			cfg.GrantedMaxTurns, cfg.GrantedTokenBudget = tt.granted[0], tt.granted[1]
			cfg.InvestigateTimeout = 20 * time.Minute
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"reports/plan.md": goodPlan,
			}}}}
			if ev := onlyEvent(t, cfg, fx, &out); ev.Plan == nil || ev.Plan.Outcome != envelope.OutcomeOK {
				t.Fatalf("event = %+v, want an ok plan", ev)
			}
			argv := fx.specs[0].Argv
			if got := argvFlag(t, argv, "--max-turns"); got != strconv.Itoa(tt.wantTurns) {
				t.Errorf("--max-turns = %s, want %d", got, tt.wantTurns)
			}
			prompt := argvFlag(t, argv, "-p")
			own := fmt.Sprintf("This stage may take at most %d agent turns, %d output tokens and 20 minutes.",
				tt.wantTurns, tt.wantTok)
			if !strings.Contains(prompt, own) {
				t.Errorf("the prompt does not state the run's own limits %q:\n%s", own, prompt)
			}
			if want := "A build of this plan can be granted at most 240 agent turns and 1200000 output " +
				"tokens."; !strings.Contains(prompt, want) {
				t.Errorf("the prompt does not state the build's ceiling %q", want)
			}
		})
	}
}

// TestBuildPromptStatesItsLimits: a build's prompt states the limits the
// run is held to — its grant, clamped to the stage's ceiling, or the
// automated budget with no grant — and the stage's wall clock, while its
// posture stays the workspace-write one, with no write scope.
func TestBuildPromptStatesItsLimits(t *testing.T) {
	tests := []struct {
		name      string
		granted   [2]int
		wantTurns int
		wantTok   int
	}{
		{"no grant states the automated budget", [2]int{0, 0}, 80, 400000},
		{"a grant below the automated budget is stated", [2]int{40, 100000}, 40, 100000},
		{"a grant above the ceiling states the ceiling", [2]int{400, 9000000}, 240, 1200000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := intentConfig(t, PhaseBuild, &out) // auto 80/400000, manual 240/1200000
			brokeredClaude(t, &cfg)
			cfg.GrantedMaxTurns, cfg.GrantedTokenBudget = tt.granted[0], tt.granted[1]
			cfg.RemediateTimeout = 45 * time.Minute
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess,
				writes:    map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript},
				repoWrite: map[string]string{"app.js": "version();\n"},
			}}}
			if rem := onlyEvent(t, cfg, fx, &out).Remediation; rem == nil || !rem.Success {
				t.Fatalf("build = %+v, want success", rem)
			}
			argv := fx.specs[0].Argv
			if got := argvFlag(t, argv, "--max-turns"); got != strconv.Itoa(tt.wantTurns) {
				t.Errorf("--max-turns = %s, want %d", got, tt.wantTurns)
			}
			if got := argvFlag(t, argv, "--allowedTools"); got != "Read Glob Grep Edit Write NotebookEdit Bash" {
				t.Errorf("--allowedTools = %q, want the unscoped workspace-write posture", got)
			}
			own := fmt.Sprintf("This run may take at most %d agent turns, %d output tokens and 45 minutes.",
				tt.wantTurns, tt.wantTok)
			if prompt := argvFlag(t, argv, "-p"); !strings.Contains(prompt, own) {
				t.Errorf("the prompt does not state the run's own limits %q:\n%s", own, prompt)
			}
		})
	}
}

// TestStageLimitsUnknownStageErrors: every intent stage resolves its own
// limits, and a stage that is not one of them is an error, never the plan
// stage's ceiling (or any other's).
func TestStageLimitsUnknownStageErrors(t *testing.T) {
	tests := []struct {
		phase              Phase
		wantTurns, wantTok int
		wantErr            bool
	}{
		{PhasePlan, 25, 150000, false},
		{PhaseBuild, 80, 400000, false},
		{PhaseEvaluate, 25, 150000, false},
		{PhaseCheckReview, 25, 150000, false},
		{PhaseInvestigate, 0, 0, true},
		{PhaseRemediate, 0, 0, true},
		{Phase("deploy"), 0, 0, true},
		{Phase(""), 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg := newConfig(t, t.TempDir(), &out)
			cfg.Phase = tt.phase
			turns, tok, err := New(cfg, nil).stageLimits()
			if (err != nil) != tt.wantErr {
				t.Fatalf("stageLimits() error = %v, want error %v", err, tt.wantErr)
			}
			if turns != tt.wantTurns || tok != tt.wantTok {
				t.Errorf("stageLimits() = %d, %d; want %d, %d", turns, tok, tt.wantTurns, tt.wantTok)
			}
		})
	}
}

// TestRoleStageLimitsFromTheGrant: an evaluate or checkreview run is held to
// its grant, which may lower the investigate stage's ceiling but never
// raise it. An evaluate stage's first run is given its turns less those it
// keeps for its report (reportReserve: 2 of 25, 2 of 6).
func TestRoleStageLimitsFromTheGrant(t *testing.T) {
	tests := []struct {
		name         string
		granted      int
		wantTurns    string
		wantEvaluate string
	}{
		{"no grant runs on the ceiling", 0, "25", "23"},
		{"a lower grant", 6, "6", "4"},
		{"a grant above the ceiling is clamped", 400, "25", "23"},
	}
	for _, phase := range []Phase{PhaseEvaluate, PhaseCheckReview} {
		for _, tt := range tests {
			t.Run(string(phase)+"/"+tt.name, func(t *testing.T) {
				var out bytes.Buffer
				cfg, ws := roleConfig(t, phase, roleHandoff(phase), &out)
				brokeredClaude(t, &cfg)
				cfg.GrantedMaxTurns = tt.granted
				write := map[string]string{"scratch/report.md": goodEvaluation}
				if phase == PhaseCheckReview {
					write = map[string]string{"scratch/review-1.md": reviewReport("proves")}
				}
				fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: write}}}
				onlyEvent(t, cfg, fx, &out)
				want := tt.wantTurns
				if phase == PhaseEvaluate {
					want = tt.wantEvaluate
				}
				if got := argvFlag(t, fx.specs[0].Argv, "--max-turns"); got != want {
					t.Errorf("--max-turns = %s, want %s", got, want)
				}
			})
		}
	}
}

// TestRunUnknownPhaseIsFatal: a phase no stage handles is a fatal before
// any command runs, never a remediation (the stage a phase that was neither
// investigate nor an intent stage used to fall through to).
func TestRunUnknownPhaseIsFatal(t *testing.T) {
	var out bytes.Buffer
	cfg, _ := remediateConfig(t, goodInvestigation, &out)
	cfg.Phase = "deploy"
	fx := &fakeExec{}
	err := New(cfg, fx).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("Run() error = %v, want the unknown phase named", err)
	}
	if len(fx.specs) != 0 {
		t.Errorf("commands run = %d, want none", len(fx.specs))
	}
	if evs := events(t, out.String()); len(evs) != 1 || evs[0].Type != envelope.TypeFatal {
		t.Errorf("events = %+v, want one fatal", evs)
	}
}
