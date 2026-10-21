// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
)

// TestPlanPromptStatesTheBuildProfile: the build network profile a plan Job
// is told of (MAUL_BUILD_NETWORK_PROFILES) reaches the plan prompt as the
// fact that the build can fetch Go modules, and a plan Job told none says
// nothing of a profile; one that does not decode is a configuration error.
func TestPlanPromptStatesTheBuildProfile(t *testing.T) {
	for _, tt := range []struct {
		name, handoff string
	}{{"profile", `[{"url":"https://github.com/acme/shop","profile":"go"}]`}, {"none", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_FINDING": "target-1-plan-r1-a1",
				"MAUL_PHASE": string(PhasePlan), "MAUL_BUILD_NETWORK_PROFILES": tt.handoff}
			fromEnv, err := FromEnv(func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cfg, ws := intentConfig(t, PhasePlan, &out)
			cfg.BuildProfiles = fromEnv.BuildProfiles
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("caller-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.InvestigateHarness, cfg.BrokerTokenFile = "claude", tokenFile
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"reports/plan.md": goodPlan,
			}}}}
			if ev := onlyEvent(t, cfg, fx, &out); ev.Plan == nil || ev.Plan.Outcome != envelope.OutcomeOK {
				t.Fatalf("event = %+v, want an ok plan", ev)
			}
			argv := fx.specs[0].Argv
			i := slices.Index(argv, "-p")
			if i < 0 || i+1 >= len(argv) {
				t.Fatalf("argv lacks -p: %q", argv)
			}
			stated := strings.Contains(argv[i+1], "runs with the operator's network profile `go`")
			if stated != (tt.handoff != "") {
				t.Errorf("prompt states the profile = %v, want %v:\n%s", stated, tt.handoff != "", argv[i+1])
			}
		})
	}
	env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_BUILD_NETWORK_PROFILES": "not json"}
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), "MAUL_BUILD_NETWORK_PROFILES") {
		t.Errorf("FromEnv error = %v, want one naming MAUL_BUILD_NETWORK_PROFILES", err)
	}
}

// TestBuildProfilesCarryRoutes: a build profile handed with its registry
// routes reaches the plan's configuration with them, so the plan prompt
// names what that build can fetch.
func TestBuildProfilesCarryRoutes(t *testing.T) {
	env := map[string]string{"MAUL_REPO": "acme/shop", "MAUL_FINDING": "target-1-plan-r1-a1",
		"MAUL_PHASE": string(PhasePlan), "MAUL_BUILD_NETWORK_PROFILES": `[{"url":"https://github.com/acme/shop",` +
			`"profile":"web","routes":{"grants":[{"registry":"ca-npm","ecosystem":"npm","anyPackage":true}]}}]`}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.BuildProfiles) != 1 || cfg.BuildProfiles[0].Routes == nil ||
		len(cfg.BuildProfiles[0].Routes.Grants) != 1 || cfg.BuildProfiles[0].Routes.Grants[0].Registry != "ca-npm" {
		t.Errorf("BuildProfiles = %+v, want the web profile with its npm grant", cfg.BuildProfiles)
	}
}
