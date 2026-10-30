// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package harness

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestByID(t *testing.T) {
	tests := []struct {
		id   string
		ok   bool
		name string
	}{
		{"claude", true, "Claude Code"},
		{"codex", true, "OpenAI Codex"},
		{"copilot", true, "GitHub Copilot"},
		{"fake", true, "Fake"},
		{"cursor", false, ""},
		{"", false, ""},
	}
	for _, tt := range tests {
		h, ok := ByID(tt.id)
		if ok != tt.ok {
			t.Errorf("ByID(%q) ok = %v, want %v", tt.id, ok, tt.ok)
			continue
		}
		if ok && (h.ID() != tt.id || h.Name() != tt.name) {
			t.Errorf("ByID(%q) = (%q, %q), want (%q, %q)", tt.id, h.ID(), h.Name(), tt.id, tt.name)
		}
	}
}

func TestAvailableFake(t *testing.T) {
	// The fake harness runs through cat, which every unix PATH carries.
	if path, ok := Available(NewFake()); !ok || path == "" {
		t.Errorf("Available(fake) = (%q, %v), want cat found on PATH", path, ok)
	}
}

func TestHarnessesImplementUsageScanner(t *testing.T) {
	for _, h := range All() {
		if _, ok := h.(UsageScanner); !ok {
			t.Errorf("harness %q does not implement UsageScanner", h.ID())
		}
	}
}

// TestCheckPosture: every harness accepts the postures it renders; copilot
// refuses evaluate, which it cannot express, rather than running it as its
// default; and a posture no harness knows is refused by all of them.
func TestCheckPosture(t *testing.T) {
	unknown := SandboxEvaluate + 1
	for _, tt := range []struct {
		harness string
		refused []Sandbox
	}{
		{"claude", []Sandbox{unknown}},
		{"codex", []Sandbox{unknown}},
		{"copilot", []Sandbox{SandboxEvaluate, unknown}},
		{"fake", []Sandbox{unknown}},
	} {
		h, ok := ByID(tt.harness)
		if !ok {
			t.Fatalf("no harness %q", tt.harness)
		}
		for _, s := range []Sandbox{SandboxDefault, SandboxReadOnly, SandboxWorkspaceWrite, SandboxEvaluate, unknown} {
			err := CheckPosture(h, s)
			if want := slices.Contains(tt.refused, s); want != errors.Is(err, ErrPostureUnsupported) {
				t.Errorf("CheckPosture(%s, %d) = %v, want refused = %v", tt.harness, s, err, want)
			}
		}
	}
}

// fakeCLI drops an executable stand-in for the harness's first CLI name
// into dir and returns its path.
func fakeCLI(t *testing.T, dir string, h Harness) string {
	t.Helper()
	p := filepath.Join(dir, h.CLI()[0])
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAvailableIgnoresBinDir: Available resolves from PATH and nothing
// else, even in a pod whose injected binaries sit in MAUL_BIN_DIR. The
// injected CLI is resolved by AvailableIn alone (agentrun's preflight, then
// pinCLI), which never falls back to PATH; an Available that preferred the
// injected directory but fell back when it was empty would bless exactly
// the substitution injection exists to prevent.
func TestAvailableIgnoresBinDir(t *testing.T) {
	injected := fakeCLI(t, t.TempDir(), NewFake())
	t.Setenv("MAUL_BIN_DIR", filepath.Dir(injected))
	got, ok := Available(NewFake())
	if !ok || got == "" || got == injected {
		t.Errorf("Available(fake) = (%q, %v), want cat from PATH, not the injected %q", got, ok, injected)
	}
}

func TestAvailableIn(t *testing.T) {
	dir := t.TempDir()
	if got, ok := AvailableIn(NewFake(), dir); ok {
		t.Errorf("AvailableIn(empty dir) = (%q, true), want not found and no PATH fallback", got)
	}
	// A non-executable file is not a CLI.
	if err := os.WriteFile(filepath.Join(dir, "cat"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := AvailableIn(NewFake(), dir); ok {
		t.Errorf("AvailableIn(non-executable) = (%q, true), want not found", got)
	}
	if err := os.Remove(filepath.Join(dir, "cat")); err != nil {
		t.Fatal(err)
	}
	want := fakeCLI(t, dir, NewFake())
	if got, ok := AvailableIn(NewFake(), dir); !ok || got != want {
		t.Errorf("AvailableIn = (%q, %v), want %q", got, ok, want)
	}
}
