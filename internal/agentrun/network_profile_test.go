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
