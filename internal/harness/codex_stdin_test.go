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

// TestCodexPromptReachesTheCLIOnStdin: a prompt past Linux's bound on one
// argument (MAX_ARG_STRLEN, 128 KiB) reaches the CLI byte for byte, because
// it is `codex exec -`'s standard input and never an argument. The real
// pinned CLI (rust-v0.167.0) reads a "-" prompt from standard input whole,
// neither trimmed nor wrapped (exec's resolve_prompt, StdinPromptBehavior::
// Forced); hack/fake-agent/codex, the scripted CLI the e2e suites drive,
// stands in for it here, accepts the prompt only that way and records it.
func TestCodexPromptReachesTheCLIOnStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted CLI is a POSIX shell script")
	}
	_, here, _, _ := runtime.Caller(0)
	cli := filepath.Join(filepath.Dir(here), "..", "..", "hack", "fake-agent", "codex")
	prompt := "BEGIN-SENTINEL\n" + strings.Repeat("a line of the request\n", 10000) + "END-SENTINEL\n\n"
	if len(prompt) <= 128<<10 {
		t.Fatalf("prompt of %d bytes is not past MAX_ARG_STRLEN", len(prompt))
	}
	spec := NewCodex().PromptSpec(t.TempDir(), PromptRequest{Prompt: prompt, Model: "m", Sandbox: SandboxEvaluate})
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
	got, err := os.ReadFile(filepath.Join(home, ".fake-codex", "prompt-none"))
	if err != nil {
		t.Fatalf("%v (exit %d, stderr %q)", err, res.ExitCode, res.StderrTail)
	}
	if string(got) != prompt {
		t.Errorf("the CLI read %d bytes, want the prompt's %d intact", len(got), len(prompt))
	}
}

// TestScriptedCodexRefusesAnEmptyPrompt: the scripted CLI ends as the real
// pinned one does on `codex exec -` with nothing to read (rust-v0.167.0,
// read_prompt_from_stdin: a prompt that is empty once trimmed prints "No
// prompt provided via stdin." and exits 1), before any model request, so an
// e2e suite cannot pass on a run whose prompt never reached standard input.
func TestScriptedCodexRefusesAnEmptyPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted CLI is a POSIX shell script")
	}
	_, here, _, _ := runtime.Caller(0)
	cli := filepath.Join(filepath.Dir(here), "..", "..", "hack", "fake-agent", "codex")
	for name, stdin := range map[string]string{"empty": "", "blank": " \n\t\r\n"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			spec := NewCodex().PromptSpec(t.TempDir(), PromptRequest{Prompt: stdin, Model: "m"})
			spec.Argv = slices.Clone(spec.Argv)
			spec.Argv[0] = cli
			spec.Env = append(slices.Clone(spec.Env), "HOME="+home, "MAUL_WORKSPACE="+spec.Dir, "MAUL_PHASE=plan")
			res, err := (&runner.Exec{}).Run(context.Background(), spec, 30*time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != 1 || !strings.Contains(res.StderrTail, "No prompt provided via stdin.") ||
				len(res.Stdout) != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q, want exit 1 and the real CLI's refusal, no event",
					res.ExitCode, res.Stdout, res.StderrTail)
			}
			if _, err := os.Stat(filepath.Join(home, ".fake-codex", "calls")); err == nil {
				t.Error("a model request was sent for an empty prompt")
			}
		})
	}
}

// TestEvaluateHarnessesTakeThePromptOnStdin: every harness that can hold a
// run to the evaluate posture takes its prompt on standard input, never as
// an argument, so the evaluate prompt has one bound whatever its harness
// (templates.EvaluatePromptMaxBytes), and no argument of the command line
// can reach Linux's 128 KiB. The same holds for every posture of the
// harnesses intents run on, claude and codex.
func TestEvaluateHarnessesTakeThePromptOnStdin(t *testing.T) {
	const probe = "maul-prompt-probe"
	for _, h := range All() {
		var postures []Sandbox
		switch h.ID() {
		case "claude", "codex":
			postures = []Sandbox{SandboxDefault, SandboxReadOnly, SandboxWorkspaceWrite, SandboxEvaluate}
		default:
			if CheckPosture(h, SandboxEvaluate) == nil {
				postures = []Sandbox{SandboxEvaluate}
			}
		}
		for _, s := range postures {
			req := PromptRequest{Prompt: probe, Model: "m", Sandbox: s, SystemPromptAppend: "append"}
			spec := h.PromptSpec("/ws", req)
			if slices.ContainsFunc(spec.Argv, func(a string) bool { return strings.Contains(a, probe) }) {
				t.Errorf("%s, posture %d: the prompt is on the command line: %q", h.ID(), s, spec.Argv)
			}
			if h.ID() != "fake" && !strings.Contains(spec.Stdin, probe) {
				t.Errorf("%s, posture %d: Stdin = %q, want the prompt", h.ID(), s, spec.Stdin)
			}
		}
	}
}
