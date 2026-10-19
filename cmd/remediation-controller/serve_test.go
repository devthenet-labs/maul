// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/cli"
	"github.com/bitwise-media-group/patchy/internal/credgate"
)

// serveOpts resolves args through the real serve flag surface, as the
// binary does, without starting anything.
func serveOpts(t *testing.T, args ...string) *cli.Options {
	t.Helper()
	opts := cli.NewOptions()
	root := cli.NewControllerRoot("remediation-controller", "test", opts)
	serve := newServeCmd(opts)
	serve.RunE = func(*cobra.Command, []string) error { return nil }
	root.AddCommand(serve)
	root.SetArgs(append([]string{"serve"}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return opts
}

// TestAgentEnvDefaults pins the PATCHY_* handoff every remediation pod
// receives with no flags set. The budgets match the ones the investigation
// controller quotes in the analysis prompt by default; the allowlist is not
// in it (the spawner enforces it controller-side).
func TestAgentEnvDefaults(t *testing.T) {
	want := map[string]string{
		"PATCHY_REMEDIATE_TIMEOUT":             "45m0s",
		"PATCHY_REMEDIATE_IDLE_TIMEOUT":        "20m0s",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "80",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "400000",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "240",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "1200000",
	}
	if got := agentEnv(serveOpts(t)); !maps.Equal(got, want) {
		t.Errorf("agentEnv =\n%v\nwant\n%v", got, want)
	}
}

func TestAgentEnvFollowsFlags(t *testing.T) {
	t.Setenv("PATCHY_REMEDIATE_TIMEOUT", "2h")
	got := agentEnv(serveOpts(t, "--remediate-idle-timeout", "0", "--remediate-auto-max-turns", "1",
		"--remediate-auto-token-budget", "2", "--remediate-manual-max-turns", "3",
		"--remediate-manual-token-budget", "4"))
	want := map[string]string{
		"PATCHY_REMEDIATE_TIMEOUT":             "2h0m0s",
		"PATCHY_REMEDIATE_IDLE_TIMEOUT":        "0s",
		"PATCHY_REMEDIATE_AUTO_MAX_TURNS":      "1",
		"PATCHY_REMEDIATE_AUTO_TOKEN_BUDGET":   "2",
		"PATCHY_REMEDIATE_MANUAL_MAX_TURNS":    "3",
		"PATCHY_REMEDIATE_MANUAL_TOKEN_BUDGET": "4",
	}
	if !maps.Equal(got, want) {
		t.Errorf("agentEnv =\n%v\nwant\n%v", got, want)
	}
}

// TestServeStartupErrors: each misconfiguration fails startup naming its
// cause, before a manager is built or any cluster call is made.
func TestServeStartupErrors(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	with := func(extra ...string) []string {
		return append([]string{"--namespace", "p", "--fake-agent-image", "fake:1"}, extra...)
	}
	for _, tt := range []struct {
		name    string
		env     string
		args    []string
		wantErr string
	}{
		{name: "no namespace", wantErr: "namespace is required"},
		{name: "no runner", args: []string{"--namespace", "p"}, wantErr: "no agent runner configured"},
		{name: "namespace from the pod", env: "patchy", wantErr: "no agent runner configured"},
		{name: "bad copilot credential env", args: []string{"--namespace", "p", "--copilot-agent-image", "c:1",
			"--copilot-secret-env", "OPENAI_API_KEY"}, wantErr: "--copilot-secret-env"},
		{name: "unparseable disk wall", args: with("--agent-ephemeral-storage", "big"),
			wantErr: "--agent-ephemeral-storage"},
		{name: "bad agent resources", args: with("--agent-cpu-limit", "nope"), wantErr: "--agent-cpu-limit"},
		{name: "network profiles without repository images", args: with("--network-profiles-enabled"),
			wantErr: "--network-profiles-enabled requires --repository-images"},
		{name: "network profiles without a profile", args: with("--network-profiles-enabled", "--repository-images",
			"--agent-ephemeral-storage", "8Gi"), wantErr: "--network-profiles is empty"},
		{name: "zero changeset cap", args: with("--changeset-max-entries", "0"),
			wantErr: "--changeset-max-entries must be positive"},
		{name: "negative idle timeout", args: with("--remediate-idle-timeout", "-1m"),
			wantErr: "--remediate-idle-timeout must not be negative"},
		{name: "missing kubeconfig", args: with("--kubeconfig", filepath.Join(t.TempDir(), "absent")),
			wantErr: "kubeconfig"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("POD_NAMESPACE", tt.env)
			}
			err := serve(t.Context(), serveOpts(t, tt.args...))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("serve error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestUsageCompactorOnlyUnderAttribute: the UsageRollups' compaction loop
// is registered only under attribution mode attribute. Off (the default),
// the chart grants no access to UsageRollups, so a loop would log a refused
// list every hour on an install that never opted in.
func TestUsageCompactorOnlyUnderAttribute(t *testing.T) {
	if c := usageCompactor(nil, nil, "patchy", attribution.ModeOff, nil); c != nil {
		t.Errorf("mode off registers a compactor: %+v", c)
	}
	c := usageCompactor(nil, nil, "patchy", attribution.ModeAttribute, nil)
	if c == nil || c.Namespace != "patchy" {
		t.Errorf("mode attribute compactor = %+v, want one for namespace patchy", c)
	}
	own, err := ownSettings(serveOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if c := usageCompactor(nil, nil, "patchy", own.attribution, nil); c != nil {
		t.Errorf("the default flags (mode %q) register a compactor", own.attribution)
	}
	if own.credentials != credgate.ModeOff || own.accountURL != "" {
		t.Errorf("the default flags hold runs: %+v", own)
	}
}

// TestUserCredentialsFromEnv: the chart's PATCHY_USER_CREDENTIALS and
// PATCHY_ACCOUNT_URL reach the settings; enforce without attribution stops
// startup.
func TestUserCredentialsFromEnv(t *testing.T) {
	t.Setenv("PATCHY_ATTRIBUTION_MODE", "attribute")
	t.Setenv("PATCHY_USER_CREDENTIALS", "enforce")
	t.Setenv("PATCHY_ACCOUNT_URL", "https://status.example.com/#/account")
	own, err := ownSettings(serveOpts(t))
	if err != nil || own.credentials != credgate.ModeEnforce || own.accountURL != "https://status.example.com/#/account" {
		t.Fatalf("ownSettings = %+v, %v", own, err)
	}
	t.Setenv("PATCHY_ATTRIBUTION_MODE", "off")
	if _, err := ownSettings(serveOpts(t)); err == nil {
		t.Error("enforce without attribution started")
	}
}

// TestCodexStartsUnderUserCredentials: per-user credentials cover OpenAI
// keys, so a fleet with codex enabled starts under every
// --user-credentials mode (the temporary guard of the codex broker design's
// group 2 is gone), and so does a claude-only fleet. It drives startupFleet,
// serve's one path from the flags and the credentials mode to the fleet, so
// a guard put back there fails it.
func TestCodexStartsUnderUserCredentials(t *testing.T) {
	// An unroutable broker URL: the readiness probe is advisory only.
	const broker = "http://127.0.0.1:1"
	for _, mode := range []string{"off", "attribute", "enforce"} {
		for _, tc := range []struct {
			harness string
			args    []string
		}{
			{"codex", []string{"--codex-agent-image", "codex:1", "--broker-url", broker,
				"--model-allowlist", "openai/gpt-5.3-codex", "--remediate-model", "openai/gpt-5.3-codex"}},
			{"claude", []string{"--claude-agent-image", "claude:1", "--broker-url", broker}},
		} {
			t.Run(mode+" "+tc.harness, func(t *testing.T) {
				t.Setenv("PATCHY_ATTRIBUTION_MODE", "attribute")
				t.Setenv("PATCHY_USER_CREDENTIALS", mode)
				fl, err := startupFleet(t.Context(), serveOpts(t, tc.args...), fake.NewClientset())
				if err != nil {
					t.Fatalf("startupFleet: %v", err)
				}
				if string(fl.credentials) != mode || !slices.Equal(fl.enabled, []string{tc.harness}) {
					t.Fatalf("fleet = %s %v, want %s [%s]", fl.credentials, fl.enabled, mode, tc.harness)
				}
			})
		}
	}
}
