// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// ungrantedPlanLimits are what a plan Job with no grant runs on and its
// prompt states: the investigate stage's limits, its turns split between the
// first run and a wrap-up, and its wall clock.
func ungrantedPlanLimits(cfg Config) templates.StageLimits {
	s := splitGrant(cfg.InvestigateMaxTurns, cfg.InvestigateTimeout)
	return templates.StageLimits{
		MaxTurns: s.first, WrapUpTurns: s.reserve, TokenBudget: cfg.InvestigateTokenBudget,
		Timeout: cfg.InvestigateTimeout,
	}
}

// ungrantedBuildLimits are what a build Job with no grant runs on and its
// prompt states: the automated budget, its turns split between the first
// run and a wrap-up, and the remediate stage's wall clock.
func ungrantedBuildLimits(cfg Config) templates.StageLimits {
	s := splitGrant(cfg.RemediateAutoMaxTurns, cfg.RemediateTimeout)
	return templates.StageLimits{
		MaxTurns: s.first, WrapUpTurns: s.reserve, TokenBudget: cfg.RemediateAutoTokenBudget,
		Timeout: cfg.RemediateTimeout,
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
// the ceiling with no grant, less the turns held back for a wrap-up, which
// it states beside them, and the stage's wall clock — not the raw grant,
// and states the build's ceiling apart from them.
func TestPlanPromptStatesItsLimits(t *testing.T) {
	tests := []struct {
		name      string
		granted   [2]int
		wantTurns int
		wantWrap  int
		wantTok   int
	}{
		{"no grant states the stage's limits", [2]int{0, 0}, 20, 5, 150000},
		{"a grant too small to hold turns back is stated whole", [2]int{10, 50000}, 10, 0, 50000},
		{"a grant above the ceiling states the ceiling", [2]int{400, 9000000}, 20, 5, 150000},
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
			wrap := fmt.Sprintf("patchy gives you at most %d more turns", tt.wantWrap)
			if got := strings.Contains(prompt, "more turns"); got != (tt.wantWrap > 0) ||
				(got && !strings.Contains(prompt, wrap)) {
				t.Errorf("the prompt promises wrap-up turns = %v, want %q only when %d > 0", got, wrap, tt.wantWrap)
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
// automated budget with no grant, less the 6 turns held back for a wrap-up
// — and the stage's wall clock, while its posture stays the
// workspace-write one, with no write scope.
func TestBuildPromptStatesItsLimits(t *testing.T) {
	tests := []struct {
		name      string
		granted   [2]int
		wantTurns int
		wantTok   int
	}{
		{"no grant states the automated budget", [2]int{0, 0}, 74, 400000},
		{"a grant below the automated budget is stated", [2]int{40, 100000}, 34, 100000},
		{"a grant above the ceiling states the ceiling", [2]int{400, 9000000}, 234, 1200000},
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
