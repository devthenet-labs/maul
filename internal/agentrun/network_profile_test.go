// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

// TestNetworkProfileReachesThePrompt: a remediate or build stage whose
// Config names a network profile tells the agent about the route instead
// of saying it has no network; without one the prompt says what it always
// has. The prompt is read off the claude CLI's argv.
func TestNetworkProfileReachesThePrompt(t *testing.T) {
	for _, phase := range []Phase{PhaseRemediate, PhaseBuild} {
		for _, profile := range []string{"go", ""} {
			t.Run(string(phase)+"/"+profile, func(t *testing.T) {
				var out bytes.Buffer
				var cfg Config
				var ws string
				outputs := map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript}
				if phase == PhaseBuild {
					cfg, ws = intentConfig(t, PhaseBuild, &out)
					tokenFile := filepath.Join(t.TempDir(), "token")
					if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					cfg.BrokerTokenFile = tokenFile
				} else {
					ws = newWorkspace(t)
					if err := os.WriteFile(filepath.Join(ws, "input", "investigation.md"),
						[]byte(goodInvestigation), 0o644); err != nil {
						t.Fatal(err)
					}
					cfg = newConfig(t, ws, &out)
					cfg.Phase = PhaseRemediate
					outputs = map[string]string{"reports/remediation.md": "---\nsuccess: false\nconfidence: 0.2\n---\n"}
				}
				cfg.RemediateHarness = "claude"
				cfg.NetworkProfile = profile
				fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: outputs,
					repoWrite: map[string]string{"app.js": "version();\n"}}}}
				if err := New(cfg, fx).Run(context.Background()); err != nil {
					t.Fatalf("Run: %v", err)
				}
				if len(fx.specs) == 0 || len(fx.specs[0].Argv) < 3 || fx.specs[0].Argv[1] != "-p" {
					t.Fatalf("stage commands = %+v, want a claude -p run", fx.specs)
				}
				prompt := strings.Join(strings.Fields(fx.specs[0].Argv[2]), " ")
				route := strings.Contains(prompt, "the operator's dependency route `go`")
				offline := strings.Contains(prompt, "no network access")
				if route != (profile != "") || offline != (profile == "") {
					t.Errorf("profile %q: route paragraph %v, offline sentence %v", profile, route, offline)
				}
			})
		}
	}
}

// TestNetworkRoutesReachThePrompt: a remediate or build stage whose Config
// carries a profile with registry grants describes each granted ecosystem
// instead of step 1's Go paragraph or the offline sentence.
func TestNetworkRoutesReachThePrompt(t *testing.T) {
	routes := templates.NetworkRoutes{Grants: []netprofile.PodGrant{
		{Registry: "ca-npm", Ecosystem: netprofile.EcosystemNPM, AnyPackage: true},
		{Registry: "ca-pypi", Ecosystem: netprofile.EcosystemPyPI, Packages: []string{"six"}},
	}}
	for _, phase := range []Phase{PhaseRemediate, PhaseBuild} {
		t.Run(string(phase), func(t *testing.T) {
			prompt := stagePrompt(t, phase, func(cfg *Config) {
				cfg.NetworkProfile, cfg.NetworkRoutes = "web", routes
			})
			for _, want := range []string{"the operator's dependency route `web` for: npm and PyPI.",
				"**npm**:", "through the registry `ca-npm`", "**pip**:", "`ca-pypi`", "`six`"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
			for _, banned := range []string{"no network access", "Go modules can be fetched", "**Go**"} {
				if strings.Contains(prompt, banned) {
					t.Errorf("prompt says %q", banned)
				}
			}
		})
	}
}

// stagePrompt runs a remediate or build stage with cfg edited and returns
// the prompt off the claude CLI's argv, whitespace collapsed.
func stagePrompt(t *testing.T, phase Phase, edit func(*Config)) string {
	t.Helper()
	var out bytes.Buffer
	var cfg Config
	var ws string
	outputs := map[string]string{"reports/build.md": goodBuild, "commit.sh": buildCommitScript}
	if phase == PhaseBuild {
		cfg, ws = intentConfig(t, PhaseBuild, &out)
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.BrokerTokenFile = tokenFile
	} else {
		ws = newWorkspace(t)
		if err := os.WriteFile(filepath.Join(ws, "input", "investigation.md"),
			[]byte(goodInvestigation), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg = newConfig(t, ws, &out)
		cfg.Phase = PhaseRemediate
		outputs = map[string]string{"reports/remediation.md": "---\nsuccess: false\nconfidence: 0.2\n---\n"}
	}
	cfg.RemediateHarness = "claude"
	edit(&cfg)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: outputs,
		repoWrite: map[string]string{"app.js": "version();\n"}}}}
	if err := New(cfg, fx).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fx.specs) == 0 || len(fx.specs[0].Argv) < 3 || fx.specs[0].Argv[1] != "-p" {
		t.Fatalf("stage commands = %+v, want a claude -p run", fx.specs)
	}
	return strings.Join(strings.Fields(fx.specs[0].Argv[2]), " ")
}
