// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/harness"
	"github.com/devthenet-labs/maul/internal/provider"
)

func TestCliModel(t *testing.T) {
	tests := []struct {
		name      string
		canonical string
		harnessID string
		modelMap  map[string]string
		want      string
	}{
		{"registry id without a map", "anthropic/claude-sonnet-5", "claude", nil, "claude-sonnet-5"},
		{"model map wins over the registry", "anthropic/claude-sonnet-5", "claude",
			map[string]string{"anthropic/claude-sonnet-5": "us.anthropic.claude-sonnet-5"},
			"us.anthropic.claude-sonnet-5"},
		{"unmapped model falls back to the registry", "anthropic/claude-opus-5", "claude",
			map[string]string{"anthropic/claude-sonnet-5": "us.anthropic.claude-sonnet-5"},
			"claude-opus-5"},
		{"unknown id falls back to the bare id", "anthropic/claude-next-99", "claude", nil, "claude-next-99"},
		{"unqualified unknown id passes through", "mystery", "claude", nil, "mystery"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{cfg: Config{ModelMap: tt.modelMap}}
			if got := a.cliModel(tt.canonical, tt.harnessID); got != tt.want {
				t.Errorf("cliModel(%q, %q) = %q, want %q", tt.canonical, tt.harnessID, got, tt.want)
			}
		})
	}
}

func TestBrokerTokenInjectedPerStage(t *testing.T) {
	ws := newWorkspace(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("stale-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cfg := newConfig(t, ws, &out)
	cfg.BrokerTokenFile = tokenFile
	fx := &fakeExec{steps: []step{
		{ws: ws, stdout: streamSuccess, writes: map[string]string{
			"reports/investigation.md": goodInvestigation,
		}},
	}}
	agent := New(cfg, fx)

	// The kubelet rotates the projection; the stage must read the file at
	// run time, not at construction.
	if err := os.WriteFile(tokenFile, []byte("fresh-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(fx.specs) != 1 {
		t.Fatalf("stages run = %d, want 1", len(fx.specs))
	}
	want := "ANTHROPIC_CUSTOM_HEADERS=X-Maul-Broker-Token: fresh-token"
	if !slices.Contains(fx.specs[0].Env, want) {
		t.Errorf("stage env = %v, want it to contain %q", fx.specs[0].Env, want)
	}
	// The raw token is registered for transcript scrubbing.
	if !slices.Contains(agent.scrub, "fresh-token") {
		t.Errorf("scrub values = %v, want the raw token registered", agent.scrub)
	}
}

func TestBrokerTokenMissingFailsTheStage(t *testing.T) {
	ws := newWorkspace(t)
	var out bytes.Buffer
	cfg := newConfig(t, ws, &out)
	cfg.BrokerTokenFile = filepath.Join(t.TempDir(), "absent")
	fx := &fakeExec{}

	if err := New(cfg, fx).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v (stage outcomes are events, not errors)", err)
	}
	evs := events(t, out.String())
	if len(evs) != 1 || evs[0].Investigation == nil {
		t.Fatalf("events = %+v, want one investigation event", evs)
	}
	if evs[0].Investigation.Outcome != envelope.OutcomeRuntimeError {
		t.Errorf("outcome = %q, want runtime error for an unreadable token", evs[0].Investigation.Outcome)
	}
	if len(fx.specs) != 0 {
		t.Error("stage ran despite the unreadable broker token")
	}
}

func TestNoBrokerEnvWhenNotBrokered(t *testing.T) {
	ev := investigateRun(t) // newConfig sets no BrokerTokenFile
	if ev.Investigation.Outcome != envelope.OutcomeOK {
		t.Fatalf("outcome = %q", ev.Investigation.Outcome)
	}
}

func TestCredentialValuesIncludesRegistered(t *testing.T) {
	h, _ := harness.ByID("claude")
	got := credentialValues(h, "registered-token", "")
	if !slices.Contains(got, "registered-token") {
		t.Errorf("credentialValues = %v, want the registered value", got)
	}
	if slices.Contains(got, "") {
		t.Error("empty registered value kept")
	}
}

func TestFromEnvBrokerFields(t *testing.T) {
	env := map[string]string{
		"MAUL_REPO":              "acme/shop",
		"MAUL_FINDING":           "finding-1",
		"MAUL_BROKER_TOKEN_FILE": "/var/run/maul/broker/token",
		"MAUL_MODEL_MAP":         "anthropic/claude-sonnet-5=us.anthropic.claude-sonnet-5",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.BrokerTokenFile != "/var/run/maul/broker/token" {
		t.Errorf("BrokerTokenFile = %q", cfg.BrokerTokenFile)
	}
	if cfg.ModelMap["anthropic/claude-sonnet-5"] != "us.anthropic.claude-sonnet-5" {
		t.Errorf("ModelMap = %v", cfg.ModelMap)
	}

	env["MAUL_MODEL_MAP"] = "garbage"
	if _, err := FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), "MAUL_MODEL_MAP") {
		t.Errorf("FromEnv() error = %v, want a MAUL_MODEL_MAP rejection", err)
	}
}

// codexTokenFile writes a broker token file for a codex stage.
func codexTokenFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("codex-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestCodexBrokerEnvPerStage: a codex stage carries the caller token, read
// at run time, in provider.CodexTokenEnv and the placeholder key in its own
// variable, and its argv points codex's provider at the broker's route by
// variable name: the token is in the env alone, never in argv, and it is
// registered for scrubbing.
func TestCodexBrokerEnvPerStage(t *testing.T) {
	ws := newWorkspace(t)
	tokenFile := codexTokenFile(t)
	var out bytes.Buffer
	cfg := newConfig(t, ws, &out)
	cfg.InvestigateHarness, cfg.InvestigateModel = "codex", "openai/gpt-5.3-codex"
	cfg.BrokerTokenFile, cfg.CodexBaseURL = tokenFile, "http://broker.maul.svc:8080/openai/v1"
	fx := &fakeExec{steps: []step{{ws: ws, stdout: codexStream, writes: map[string]string{
		"reports/investigation.md": goodInvestigation,
	}}}}
	agent := New(cfg, fx)
	if err := os.WriteFile(tokenFile, []byte("fresh-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(fx.specs) != 1 {
		t.Fatalf("stages run = %d, want 1", len(fx.specs))
	}
	if ev := events(t, out.String()); len(ev) != 1 || ev[0].Investigation == nil ||
		ev[0].Investigation.Outcome != envelope.OutcomeOK {
		t.Fatalf("events = %+v", ev)
	}
	spec := fx.specs[0]
	for _, want := range []string{
		provider.CodexTokenEnv + "=fresh-token",
		provider.CodexPlaceholderKeyEnv + "=" + provider.PlaceholderAuthToken,
	} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("stage env = %v, want %q", spec.Env, want)
		}
	}
	for _, e := range spec.Env {
		if strings.HasPrefix(e, "ANTHROPIC_CUSTOM_HEADERS=") {
			t.Errorf("a codex stage got claude's header channel: %q", e)
		}
	}
	argv := strings.Join(spec.Argv, "\n")
	for _, want := range []string{
		`model_providers.maul.base_url="http://broker.maul.svc:8080/openai/v1"`,
		`model_providers.maul.env_key="` + provider.CodexPlaceholderKeyEnv + `"`,
		`model_providers.maul.env_http_headers={"` + provider.BrokerTokenHeader + `"="` + provider.CodexTokenEnv + `"}`,
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv = %q, want %s", spec.Argv, want)
		}
	}
	if strings.Contains(argv, "fresh-token") {
		t.Error("the token is on the command line")
	}
	if !slices.Contains(agent.scrub, "fresh-token") {
		t.Errorf("scrub values = %v, want the raw token registered", agent.scrub)
	}
}

// TestCodexRefusedUnbrokered: codex runs brokered only. Without a broker
// token file, or without the broker's route, a codex stage is a runtime
// error before any command runs, never a run on codex's own provider.
func TestCodexRefusedUnbrokered(t *testing.T) {
	for _, tt := range []struct {
		name, base string
		token      bool
		want       string
	}{
		{"no token file", "http://broker/openai/v1", false, "MAUL_BROKER_TOKEN_FILE is unset"},
		{"no route", "", true, provider.CodexBaseURLEnv + " is unset"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ws := newWorkspace(t)
			var out bytes.Buffer
			cfg := newConfig(t, ws, &out)
			cfg.InvestigateHarness, cfg.InvestigateModel = "codex", "openai/gpt-5.3-codex"
			cfg.CodexBaseURL = tt.base
			if tt.token {
				cfg.BrokerTokenFile = codexTokenFile(t)
			}
			fx := &fakeExec{}
			if err := New(cfg, fx).Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			ev := events(t, out.String())
			if len(ev) != 1 || ev[0].Investigation == nil ||
				ev[0].Investigation.Outcome != envelope.OutcomeRuntimeError ||
				!strings.Contains(ev[0].Investigation.Detail, tt.want) {
				t.Fatalf("events = %+v, want a runtime error naming %q", ev, tt.want)
			}
			if len(fx.specs) != 0 {
				t.Error("codex ran unbrokered")
			}
		})
	}
}

func TestFromEnvCodexBaseURL(t *testing.T) {
	env := map[string]string{
		"MAUL_REPO": "acme/shop", "MAUL_FINDING": "f",
		provider.CodexBaseURLEnv: "http://broker:8080/openai/v1",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexBaseURL != "http://broker:8080/openai/v1" {
		t.Errorf("CodexBaseURL = %q", cfg.CodexBaseURL)
	}
	if !slices.Contains(ConfigEnvKeys(), provider.CodexBaseURLEnv) {
		t.Errorf("ConfigEnvKeys misses %s", provider.CodexBaseURLEnv)
	}
}
