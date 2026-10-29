// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/runner"
)

var update = flag.Bool("update", false, "rewrite golden files")

// stageInvocation runs one stage through the claude harness — brokered, with
// a fixed session id — and renders the one command it executes (argv, the
// working directory and the extra environment) with the per-test workspace
// path replaced by /workspace, so the rendering is the same on every machine.
func stageInvocation(t *testing.T, phase Phase, s step, ws string, cfg Config) string {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Phase = phase
	cfg.InvestigateHarness, cfg.RemediateHarness = "claude", "claude"
	cfg.BrokerTokenFile = tokenFile
	s.ws = ws
	fx := &fakeExec{steps: []step{s}}
	a := New(cfg, fx)
	a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fx.specs) != 1 {
		t.Fatalf("commands run = %d, want the one stage command", len(fx.specs))
	}
	return renderInvocation(fx.specs[0], ws)
}

// renderInvocation writes a command spec one argument per block, so a golden
// diff shows exactly which argument moved.
func renderInvocation(spec runner.CommandSpec, ws string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "dir: %s\n", spec.Dir)
	for i, arg := range spec.Argv {
		fmt.Fprintf(&b, "--- argv[%d]\n%s\n", i, arg)
	}
	for _, kv := range spec.Env {
		fmt.Fprintf(&b, "--- env\n%s\n", kv)
	}
	return strings.ReplaceAll(b.String(), ws, "/workspace")
}

// goldenFile compares got with testdata/name, rewriting it under -update.
func goldenFile(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestFindingStageInvocationsUnchanged pins the exact command each Finding
// stage runs — the rendered prompt, every CLI flag and the brokered
// environment — so work on other stages that shares their helpers
// (templates, the harness, the budget grant, the broker env) is proven not
// to move them. The goldens were captured before any other stage existed.
//
// They have moved once on purpose, in the harness, for every stage at once:
// a postured claude run reads settings from the user source alone
// (--setting-sources user, --strict-mcp-config), so the working tree's
// .claude/ settings and .mcp.json cannot change what the stage may run, and
// it loads the tree's CLAUDE.md through the working tree added as a
// directory (--add-dir and CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD).
// The prompts and every other flag are byte-for-byte what they were.
//
// The investigate golden has moved a second time on purpose, with the
// read-only posture narrowed for both read-only stages at once: it has no
// shell (--tools lists the file tools alone, Bash is denied, and the git
// allow rules are gone), its writes are scoped to the reports directory as
// the plan stage's are, and its prompt says so. The remediate golden does
// not move: the writable posture is unchanged.
//
// The remediate golden has moved once on purpose, in its prompt alone: it
// asks the agent to prove the bug (the regression test first, seen to fail
// on the unfixed code, then the fix and the same test passing) and to record
// that in the report's reproduction block. Every flag and the environment
// are byte-for-byte what they were.
func TestFindingStageInvocationsUnchanged(t *testing.T) {
	t.Run("investigate", func(t *testing.T) {
		ws := newWorkspace(t)
		var out bytes.Buffer
		got := stageInvocation(t, PhaseInvestigate, step{
			stdout: streamSuccess,
			writes: map[string]string{"reports/investigation.md": goodInvestigation},
		}, ws, newConfig(t, ws, &out))
		goldenFile(t, "invocation_investigate.txt", got)
	})
	t.Run("remediate", func(t *testing.T) {
		var out bytes.Buffer
		cfg, ws := remediateConfig(t, goodInvestigation, &out)
		cfg.GrantedMaxTurns, cfg.GrantedTokenBudget = 120, 600000
		got := stageInvocation(t, PhaseRemediate, step{
			stdout:    streamSuccess,
			writes:    map[string]string{"reports/remediation.md": goodRemediation, "commit.sh": commitScript},
			repoWrite: map[string]string{"app.js": "escaped();\n"},
		}, ws, cfg)
		goldenFile(t, "invocation_remediate.txt", got)
	})
}

// TestRoleStageInvocations pins the command an evaluate and a checkreview
// stage run: the evaluate posture (harness.SandboxEvaluate), in the scratch
// directory, on the investigate stage's model, brokered. Beyond the goldens
// it states the posture's load-bearing properties outright: the CLI runs in
// scratch and never in the workspace, no directory is added (neither
// AddDirs nor the posture's own), and the shell is held to maul-tool.
func TestRoleStageInvocations(t *testing.T) {
	tests := []struct {
		phase           Phase
		handoff, golden string
		write           map[string]string
	}{
		{PhaseEvaluate, evaluateHandoff, "invocation_evaluate.txt",
			map[string]string{"scratch/report.md": goodEvaluation}},
		{PhaseCheckReview, reviewHandoff(1), "invocation_checkreview.txt",
			map[string]string{"scratch/review-1.md": reviewReport("proves")}},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, tt.phase, tt.handoff, &out)
			got := stageInvocation(t, tt.phase, step{stdout: streamSuccess, writes: tt.write}, ws, cfg)
			goldenFile(t, tt.golden, got)
			if !strings.HasPrefix(got, "dir: /workspace/scratch\n") {
				t.Errorf("the stage does not run in the scratch directory:\n%s", got)
			}
			for _, banned := range []string{"--add-dir", "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD"} {
				if strings.Contains(got, banned) {
					t.Errorf("the evaluate posture renders %s:\n%s", banned, got)
				}
			}
			for _, want := range []string{"--restricted", "Bash(maul-tool:*)", "Read(//workspace/scratch/**)"} {
				if !strings.Contains(got, want) {
					t.Errorf("the evaluate posture lacks %s:\n%s", want, got)
				}
			}
		})
	}
}
