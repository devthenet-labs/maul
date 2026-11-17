// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/runner"
)

// TestClaudePromptReachesTheCLIOnStdin: a prompt past Linux's bound on one
// argument (MAX_ARG_STRLEN, 128 KiB) reaches the CLI byte for byte, on a
// first run and on a resume, because the prompt is the CLI's standard input
// and never an argument. hack/fake-agent/claude, the scripted CLI the e2e
// suites drive, stands in for the CLI and records the prompt it read.
func TestClaudePromptReachesTheCLIOnStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted CLI is a POSIX shell script")
	}
	_, here, _, _ := runtime.Caller(0)
	cli := filepath.Join(filepath.Dir(here), "..", "..", "hack", "fake-agent", "claude")
	prompt := "BEGIN-SENTINEL\n" + strings.Repeat("a line of the request\n", 10000) + "END-SENTINEL\n\n"
	if len(prompt) <= 128<<10 {
		t.Fatalf("prompt of %d bytes is not past MAX_ARG_STRLEN", len(prompt))
	}
	req := PromptRequest{Prompt: prompt, Model: "m", SessionID: claudeSessionID, Sandbox: SandboxReadOnly}
	c := NewClaude()
	for name, spec := range map[string]runner.CommandSpec{
		"first":  c.PromptSpec(t.TempDir(), req),
		"resume": c.ResumeSpec(t.TempDir(), claudeSessionID, req),
	} {
		t.Run(name, func(t *testing.T) {
			if slices.Contains(spec.Argv, prompt) {
				t.Fatal("the prompt is an argument of the command line")
			}
			home := t.TempDir()
			spec.Argv = slices.Clone(spec.Argv)
			spec.Argv[0] = cli
			// No MAUL_PHASE: the scripted CLI records the prompt and ends.
			spec.Env = append(slices.Clone(spec.Env), "HOME="+home, "MAUL_WORKSPACE="+spec.Dir, "MAUL_PHASE=")
			res, err := (&runner.Exec{}).Run(context.Background(), spec, 30*time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(home, ".fake-claude", "prompt-none"))
			if err != nil {
				t.Fatalf("%v (exit %d, stderr %q)", err, res.ExitCode, res.StderrTail)
			}
			if string(got) != prompt {
				t.Errorf("the CLI read %d bytes, want the prompt's %d intact", len(got), len(prompt))
			}
		})
	}
}
