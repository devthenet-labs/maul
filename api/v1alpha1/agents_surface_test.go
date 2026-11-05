// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidCLIBinary(t *testing.T) {
	for path, want := range map[string]bool{
		"/work/bin/bbrender": true, "/work/bin/cmd/bbrender": true, "/work/bin/a_b.c-d": true,
		"/work/bin/" + strings.Repeat("a", 64): true, "": false,
		"/work/bin": false, "/work/bin/": false, "work/bin/x": false, "/usr/bin/x": false,
		"/work/bin/a/b/c": false, "/work/bin/../x": false, "/work/bin/./x": false, "/work/bin/.x": false,
		"/work/bin//x": false, "/work/bin/x/": false, "/work/bin/a b": false, "/work/bin/x\n": false,
		"/work/binx/y": false, "/work/bin/" + strings.Repeat("a", 65): false, "/work/bin/é": false,
	} {
		if got := ValidCLIBinary(path); got != want {
			t.Errorf("ValidCLIBinary(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestValidCLIBuild(t *testing.T) {
	goBuild := []string{"go", "build", "-o", "/work/bin/bbrender", "./cmd/bbrender"}
	many := func(n int) []string {
		argv := []string{"make", "PREFIX=/work/bin"}
		for len(argv) < n {
			argv = append(argv, "x")
		}
		return argv
	}
	for _, tt := range []struct {
		name string
		argv []string
		want bool
	}{
		{"go build -o", goBuild, true},
		{"make PREFIX", []string{"make", "install", "PREFIX=/work/bin"}, true},
		{"16 arguments", many(MaxCLIBuildArgs), true},
		{"an argument of 256 bytes", []string{"go", "build", "-o", "/work/bin/" + strings.Repeat("a", 246)}, true},
		{"no argument", nil, false},
		{"17 arguments", many(MaxCLIBuildArgs + 1), false},
		{"an argument of 257 bytes", []string{"go", "build", "-o", "/work/bin/" + strings.Repeat("a", 247)}, false},
		{"an empty argument", []string{"go", "", "/work/bin/x"}, false},
		{"a newline", []string{"go", "build\n", "-o", "/work/bin/x"}, false},
		{"a NUL", []string{"go", "build", "-o", "/work/bin/x\x00"}, false},
		{"no output under /work/bin", []string{"go", "build", "./..."}, false},
		{"an output beside /work/bin", []string{"go", "build", "-o", "/work/binx"}, false},
	} {
		if got := ValidCLIBuild(tt.argv); got != tt.want {
			t.Errorf("ValidCLIBuild(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCLIBuildTimeout(t *testing.T) {
	for _, tt := range []struct {
		name string
		cli  *CLISurface
		want time.Duration
	}{
		{"nil", nil, DefaultCLIBuildTimeout},
		{"unset", &CLISurface{}, DefaultCLIBuildTimeout},
		{"set", &CLISurface{BuildTimeout: &metav1.Duration{Duration: 15 * time.Minute}}, 15 * time.Minute},
		{"over the maximum", &CLISurface{BuildTimeout: &metav1.Duration{Duration: time.Hour}}, MaxCLIBuildTimeout},
	} {
		if got := tt.cli.Timeout(); got != tt.want {
			t.Errorf("%s: Timeout() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAgentsSurface(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    *ProjectSurfaces
		want bool
	}{
		{"nil", nil, false},
		{"browser", &ProjectSurfaces{Browser: &BrowserSurface{Entry: "/"}}, false},
		{"cli", &ProjectSurfaces{CLI: &CLISurface{}}, true},
		{"library", &ProjectSurfaces{Library: &LibrarySurface{Language: LibraryLanguageGo}}, true},
	} {
		if got := tt.s.Agents(); got != tt.want {
			t.Errorf("%s: Agents() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestVerificationPlacement(t *testing.T) {
	for _, tt := range []struct {
		placement VerificationPlacement
		agents    bool
	}{{"", false}, {PlacementSlot, false}, {PlacementAgents, true}} {
		s := PreviewVerificationSpec{Placement: tt.placement}
		if got := s.IsAgents(); got != tt.agents {
			t.Errorf("placement %q: IsAgents() = %v, want %v", tt.placement, got, tt.agents)
		}
	}
}

func TestCheckRunSide(t *testing.T) {
	for tree, want := range map[TreeSide]TreeSide{"": TreeHead, TreeHead: TreeHead, TreeBase: TreeBase} {
		if got := (CheckRun{Tree: tree}).Side(); got != want {
			t.Errorf("CheckRun{Tree: %q}.Side() = %q, want %q", tree, got, want)
		}
	}
}

func TestValidCommitSHA(t *testing.T) {
	for sha, want := range map[string]bool{
		strings.Repeat("a", 40): true, "0123456789abcdef0123456789abcdef01234567": true,
		"": false, strings.Repeat("a", 39): false, strings.Repeat("a", 41): false, strings.Repeat("A", 40): false,
		strings.Repeat("g", 40): false,
	} {
		if got := ValidCommitSHA(sha); got != want {
			t.Errorf("ValidCommitSHA(%q) = %v, want %v", sha, got, want)
		}
	}
}
