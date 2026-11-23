// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/transcript"
)

// buildSubagentsEnv is a build Job's environment granted subagents.
func buildSubagentsEnv() map[string]string {
	return map[string]string{"MAUL_REPO": "acme/shop", "MAUL_FINDING": "run-1", "MAUL_PHASE": "build",
		"MAUL_BUILD_SUBAGENTS": "true"}
}

// TestFromEnvBuildSubagents: MAUL_BUILD_SUBAGENTS is "true" or unset, any
// other value refused by name; it is a configuration key, so a
// repository-image Job that does not set it blanks it.
func TestFromEnvBuildSubagents(t *testing.T) {
	for value, want := range map[string]bool{"": false, "true": true} {
		env := buildSubagentsEnv()
		env["MAUL_BUILD_SUBAGENTS"] = value
		cfg, err := FromEnv(func(k string) string { return env[k] })
		if err != nil || cfg.BuildSubagents != want {
			t.Errorf("FromEnv(%q) = %v, %v; want %v", value, cfg.BuildSubagents, err, want)
		}
	}
	env := buildSubagentsEnv()
	env["MAUL_BUILD_SUBAGENTS"] = "1"
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), `MAUL_BUILD_SUBAGENTS="1"`) {
		t.Errorf("FromEnv(1) error = %v, want it refused by name", err)
	}
	if !slices.Contains(ConfigEnvKeys(), "MAUL_BUILD_SUBAGENTS") {
		t.Errorf("ConfigEnvKeys() = %v, want MAUL_BUILD_SUBAGENTS among them", ConfigEnvKeys())
	}
}

// TestFromEnvRefusesSubagentsOffTheBuild: only an intent build (its first
// build and every revise round) on claude may be granted subagents. The key
// on any other phase, or on a build that runs another harness, is refused
// at startup, never quietly ignored: codex and copilot render no posture
// for it, and the fake harness runs no agent.
func TestFromEnvRefusesSubagentsOffTheBuild(t *testing.T) {
	for _, phase := range []Phase{PhaseInvestigate, PhaseRemediate, PhasePlan, PhaseEvaluate} {
		env := buildSubagentsEnv()
		env["MAUL_PHASE"] = string(phase)
		_, err := FromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "MAUL_BUILD_SUBAGENTS") ||
			!strings.Contains(err.Error(), string(phase)) {
			t.Errorf("phase %s: FromEnv error = %v, want the grant refused off the build", phase, err)
		}
	}
	for _, h := range []string{"codex", "copilot", "fake"} {
		env := buildSubagentsEnv()
		env["MAUL_REMEDIATE_HARNESS"] = h
		_, err := FromEnv(func(k string) string { return env[k] })
		if err == nil || !strings.Contains(err.Error(), "MAUL_BUILD_SUBAGENTS") || !strings.Contains(err.Error(), h) {
			t.Errorf("harness %s: FromEnv error = %v, want the grant refused on it", h, err)
		}
	}
	// The other stage's harness is not the build's: a build on claude is
	// granted whatever the investigate harness says.
	env := buildSubagentsEnv()
	env["MAUL_INVESTIGATE_HARNESS"] = "codex"
	if _, err := FromEnv(func(k string) string { return env[k] }); err != nil {
		t.Errorf("FromEnv with a codex investigate harness = %v, want the claude build granted", err)
	}
}

// argAfter is the value following flag in argv, "" when it is absent.
func argAfter(argv []string, flag string) string {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

// TestBuildSubagentsInvocation pins a granted build's command: the
// subagents posture (Agent and Task allowed, the levers in --settings) and
// the prompt's subagents section; and its report repair, which is granted
// none, runs exactly the command an ungranted build's repair runs.
func TestBuildSubagentsInvocation(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildGranted(t, &out, 0)
	fx := &fakeExec{steps: []step{c.first(ws, c.bad), {
		ws: ws, stdout: streamResumed, writes: map[string]string{c.report: c.good},
	}}}
	a := New(cfg, fx)
	a.newSessionID = func() string { return "00000000-0000-4000-8000-000000000001" }
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fx.specs) != 2 {
		t.Fatalf("commands = %d, want the first run and one repair", len(fx.specs))
	}
	first, repair := fx.specs[0], fx.specs[1]
	if deny := strings.Fields(argAfter(first.Argv, "--disallowedTools")); slices.Contains(deny, "Agent") ||
		slices.Contains(deny, "Task") || !slices.Contains(deny, "Workflow") {
		t.Errorf("first run denies %v, want Agent and Task allowed and Workflow still denied", deny)
	}
	if argAfter(first.Argv, "--settings") == "" {
		t.Errorf("first run argv %v, want the subagents levers in --settings", first.Argv)
	}
	if !strings.Contains(first.Stdin, "## Subagents") {
		t.Error("the granted build's prompt has no subagents section")
	}
	if deny := strings.Fields(argAfter(repair.Argv, "--disallowedTools")); !slices.Contains(deny, "Agent") ||
		!slices.Contains(deny, "Task") || slices.Contains(repair.Argv, "--settings") {
		t.Errorf("repair argv %v, want Agent and Task denied and no --settings", repair.Argv)
	}
	goldenFile(t, "invocation_build_subagents.txt", renderInvocation(first, ws))
	// The repair is the ungranted build's repair, byte for byte.
	goldenFile(t, "invocation_build_repair.txt", renderInvocation(repair, ws))
}

// TestUngrantedBuildPromptHasNoSubagents: a build without the grant keeps
// the prompt and the posture without subagents.
func TestUngrantedBuildPromptHasNoSubagents(t *testing.T) {
	var out bytes.Buffer
	cfg, ws, c := buildGranted(t, &out, 0)
	cfg.BuildSubagents = false
	fx := &fakeExec{steps: []step{c.first(ws, c.good)}}
	if stage := eventStage(t, onlyEvent(t, cfg, fx, &out)); stage.Outcome != envelope.OutcomeOK {
		t.Fatalf("outcome = %q (%s)", stage.Outcome, stage.Detail)
	}
	if strings.Contains(fx.specs[0].Stdin, "## Subagents") || slices.Contains(fx.specs[0].Argv, "--settings") {
		t.Error("an ungranted build was offered subagents")
	}
}

// addWorktree adds a linked worktree at rel in the clone at dir, on a
// branch of its own, holding an untracked file, as an Agent call's
// isolation "worktree" leaves one there.
func addWorktree(dir, rel string) error {
	branch := "worktree-" + filepath.Base(rel)
	cmd := exec.Command("git", "worktree", "add", "-q", "-b", branch, rel)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return &exec.ExitError{ProcessState: cmd.ProcessState, Stderr: out}
	}
	return os.WriteFile(filepath.Join(dir, rel, "child-work.txt"), []byte("left behind\n"), 0o644)
}

// linkedWorktrees lists the clone's worktrees other than its own.
func linkedWorktrees(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	var linked []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && path != dir {
			linked = append(linked, path)
		}
	}
	return linked
}

// TestBuildRemovesSubagentWorktrees: an Agent call's isolation "worktree"
// cannot be denied, and leaves a worktree under the tree's
// .claude/worktrees. A granted build removes every one there before the
// report is read and commit.sh runs, and says so in the transcript, so the
// change packages; work left only there is not part of it. A worktree
// anywhere else is not the CLI's and is kept. An ungranted build, which
// cannot start a subagent, removes nothing: a worktree its own commands
// left in the tree still fails commit.sh's clean-tree check, as before.
func TestBuildRemovesSubagentWorktrees(t *testing.T) {
	const left = ".claude/worktrees/agent-a1b2c3"
	t.Run("granted", func(t *testing.T) {
		var out bytes.Buffer
		cfg, ws, c := buildGranted(t, &out, 0)
		repo := cfg.repoDir()
		const kept = "../kept-worktree"
		if err := addWorktree(repo, kept); err != nil {
			t.Fatal(err)
		}
		keptPath := filepath.Join(filepath.Dir(repo), "kept-worktree")
		first := c.first(ws, c.good)
		first.inRepo = func(dir string) error { return addWorktree(dir, left) }
		stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
		if stage.Outcome != envelope.OutcomeOK {
			t.Fatalf("outcome = %q (%s), want the change packaged", stage.Outcome, stage.Detail)
		}
		rem := events(t, out.String())[0].Remediation
		if rem == nil || !rem.Success || rem.Changeset == nil {
			t.Fatalf("remediation = %+v, want a packaged change", rem)
		}
		for _, f := range rem.Changeset.Upserts {
			if strings.Contains(f.Path, ".claude") || strings.Contains(f.Path, "child-work") {
				t.Errorf("changeset carries %s from the removed worktree", f.Path)
			}
		}
		if _, err := os.Stat(filepath.Join(repo, left)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (%v)", left, err)
		}
		if got := linkedWorktrees(t, repo); !slices.Equal(got, []string{keptPath}) {
			t.Errorf("linked worktrees %v, want only the one outside the tree, %s", got, keptPath)
		}
		var notice *transcript.Turn
		for _, turn := range turns(t, out.String()) {
			if turn.Kind == transcript.KindNotice && strings.Contains(turn.Text, "worktree") {
				notice = &turn
			}
		}
		if notice == nil || !strings.Contains(notice.Text, left) {
			t.Errorf("notice %+v, want one naming the removed %s", notice, left)
		}
	})
	t.Run("not granted", func(t *testing.T) {
		var out bytes.Buffer
		cfg, ws, c := buildGranted(t, &out, 0)
		cfg.BuildSubagents = false
		first := c.first(ws, c.good)
		first.inRepo = func(dir string) error { return addWorktree(dir, left) }
		stage := eventStage(t, onlyEvent(t, cfg, &fakeExec{steps: []step{first}}, &out))
		if stage.Outcome != envelope.OutcomeCommitFailed {
			t.Errorf("outcome = %q (%s), want commit_failed: nothing removed the worktree", stage.Outcome,
				stage.Detail)
		}
		if got := linkedWorktrees(t, cfg.repoDir()); len(got) != 1 {
			t.Errorf("linked worktrees %v, want the run's left alone", got)
		}
	})
}
