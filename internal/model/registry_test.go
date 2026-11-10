// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package model

import (
	"strings"
	"testing"
)

// TestBuiltinInvariants guards the registry's structural rules: ids are
// provider-qualified, the ProviderID prefix matches the id, the harness in each
// Supported map is one the registry knows, every model is supported by at least
// one harness, and Preferred is always one of those harnesses. A bad
// Supported/Preferred entry would silently send the wrong --model string to a
// CLI, so these are load-bearing.
func TestBuiltinInvariants(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range builtins() {
		if seen[m.ID] {
			t.Errorf("duplicate model id %q", m.ID)
		}
		seen[m.ID] = true

		prov, rest, ok := strings.Cut(m.ID, "/")
		if !ok || rest == "" {
			t.Errorf("model id %q is not provider-qualified", m.ID)
		}
		if prov != m.ProviderID {
			t.Errorf("model %q: id prefix %q != ProviderID %q", m.ID, prov, m.ProviderID)
		}
		if !IsProviderID(m.ProviderID) {
			t.Errorf("model %q: unknown ProviderID %q", m.ID, m.ProviderID)
		}
		if len(m.Supported) == 0 {
			t.Errorf("model %q: no supported harness", m.ID)
		}
		for h := range m.Supported {
			if !IsKnownHarnessID(h) {
				t.Errorf("model %q: unknown supported harness %q", m.ID, h)
			}
		}
		if _, ok := m.Supported[m.Preferred]; !ok {
			t.Errorf("model %q: Preferred %q not in Supported %v", m.ID, m.Preferred, m.Supported)
		}
		// Every builtin carries published rates. Pricing is optional in the
		// type (a config override may name a model with none), but a builtin
		// that lost them would silently record zero-cost runs for the codex
		// harness, which reports no cost of its own.
		if m.InputUSD == nil || m.OutputUSD == nil {
			t.Errorf("model %q: missing pricing (%v/%v)", m.ID, m.InputUSD, m.OutputUSD)
		}
		// Both vendors publish a cache-read rate near a tenth of base input,
		// and cache reads dominate a long agent session, so a builtin that
		// omitted one would price them 10x over.
		if m.CachedInputUSD == nil {
			t.Errorf("model %q: missing cached-input rate", m.ID)
		}
	}
}

func TestModelIdentity(t *testing.T) {
	m, ok := ModelByID(builtins(), "anthropic/claude-sonnet-5")
	if !ok {
		t.Fatal("anthropic/claude-sonnet-5 missing from registry")
	}
	if m.Key() != "anthropic/claude-sonnet-5" {
		t.Errorf("Key() = %q, want anthropic/claude-sonnet-5", m.Key())
	}
	if m.BareID() != "claude-sonnet-5" {
		t.Errorf("BareID() = %q, want claude-sonnet-5", m.BareID())
	}
	if id, ok := m.CLIModelID(HarnessClaude); !ok || id != "claude-sonnet-5" {
		t.Errorf("claude CLI id = %q (%v), want claude-sonnet-5", id, ok)
	}
	if _, ok := m.CLIModelID(HarnessCodex); ok {
		t.Error("claude-sonnet-5 should not be supported by codex")
	}
	if m.Preferred != HarnessClaude {
		t.Errorf("Preferred = %q, want claude", m.Preferred)
	}
}

func TestCodexModel(t *testing.T) {
	m, ok := ModelByID(builtins(), "openai/gpt-5.3-codex")
	if !ok {
		t.Fatal("openai/gpt-5.3-codex missing from registry")
	}
	if id, ok := m.CLIModelID(HarnessCodex); !ok || id != "gpt-5.3-codex" {
		t.Errorf("codex CLI id = %q (%v), want gpt-5.3-codex", id, ok)
	}
	if m.Preferred != HarnessCodex {
		t.Errorf("Preferred = %q, want codex", m.Preferred)
	}
	if m.InputUSD == nil || m.CachedInputUSD == nil || m.OutputUSD == nil {
		t.Fatalf("pricing = %v/%v/%v, want 1.75/0.175/14.00 per MTok",
			m.InputUSD, m.CachedInputUSD, m.OutputUSD)
	}
	if *m.InputUSD != 1.75 || *m.CachedInputUSD != 0.175 || *m.OutputUSD != 14.00 {
		t.Errorf("pricing = %v/%v/%v, want 1.75/0.175/14.00 per MTok",
			*m.InputUSD, *m.CachedInputUSD, *m.OutputUSD)
	}
}

// TestBuiltinRates pins every builtin's published list rates (USD per MTok:
// input, cache read, output) and its vendor-native CLI id, so a rate cannot
// drift from the vendor's price page unnoticed. Claude Haiku 5.5 is priced at
// its long-prompt tier (above 100K input tokens): the registry holds one rate
// per class, and the higher one never undercounts spend.
func TestBuiltinRates(t *testing.T) {
	tests := []struct {
		id, cli         string
		in, cached, out float64
	}{
		{"anthropic/claude-haiku-4-5", "claude-haiku-4-5", 1.00, 0.10, 5.00},
		{"anthropic/claude-haiku-5-5", "claude-haiku-5-5", 0.50, 0.05, 2.50},
		{"anthropic/claude-sonnet-5", "claude-sonnet-5", 2.00, 0.20, 10.00},
		{"anthropic/claude-sonnet-5-5", "claude-sonnet-5-5", 2.00, 0.10, 10.00},
		{"anthropic/claude-opus-5", "claude-opus-5", 5.00, 0.50, 25.00},
		{"anthropic/claude-opus-5-5", "claude-opus-5-5", 4.00, 0.20, 20.00},
		{"anthropic/claude-fable-5", "claude-fable-5", 10.00, 1.00, 50.00},
		{"anthropic/claude-fable-5-2", "claude-fable-5-2", 8.00, 0.20, 40.00},
		{"openai/gpt-5.3-codex", "gpt-5.3-codex", 1.75, 0.175, 14.00},
		{"openai/gpt-5.6-luna", "gpt-5.6-luna", 1.00, 0.10, 6.00},
		{"openai/gpt-5.6-terra", "gpt-5.6-terra", 2.50, 0.25, 15.00},
		{"openai/gpt-5.6-sol", "gpt-5.6-sol", 5.00, 0.50, 30.00},
		{"openai/gpt-6-astra", "gpt-6-astra", 10.00, 1.00, 50.00},
	}
	if n := len(builtins()); n != len(tests) {
		t.Fatalf("registry has %d models, the rate table covers %d: update both together", n, len(tests))
	}
	for _, tt := range tests {
		m, ok := ModelByID(builtins(), tt.id)
		if !ok {
			t.Errorf("%s missing from registry", tt.id)
			continue
		}
		if id, ok := m.CLIModelID(m.Preferred); !ok || id != tt.cli {
			t.Errorf("%s: %s CLI id = %q, want %q", tt.id, m.Preferred, id, tt.cli)
		}
		if m.InputUSD == nil || m.CachedInputUSD == nil || m.OutputUSD == nil {
			t.Errorf("%s: unpriced", tt.id)
			continue
		}
		if *m.InputUSD != tt.in || *m.CachedInputUSD != tt.cached || *m.OutputUSD != tt.out {
			t.Errorf("%s: rates = %v/%v/%v, want %v/%v/%v", tt.id,
				*m.InputUSD, *m.CachedInputUSD, *m.OutputUSD, tt.in, tt.cached, tt.out)
		}
	}
}

// TestCopilotSupportsEveryModel pins Copilot's role in the registry: it brokers
// both vendors, so every model must name it as a supported harness, and it must
// never be a model's Preferred — a vendor-native harness always wins when both
// are enabled. It also guards the id divergence Supported exists to record:
// Copilot spells Anthropic point releases with a dot.
func TestCopilotSupportsEveryModel(t *testing.T) {
	wantCLIID := map[string]string{
		"anthropic/claude-haiku-4-5":  "claude-haiku-4.5",
		"anthropic/claude-haiku-5-5":  "claude-haiku-5.5",
		"anthropic/claude-sonnet-5":   "claude-sonnet-5",
		"anthropic/claude-sonnet-5-5": "claude-sonnet-5.5",
		"anthropic/claude-opus-5":     "claude-opus-5",
		"anthropic/claude-opus-5-5":   "claude-opus-5.5",
		"anthropic/claude-fable-5":    "claude-fable-5",
		"anthropic/claude-fable-5-2":  "claude-fable-5.2",
		"openai/gpt-5.3-codex":        "gpt-5.3-codex",
		"openai/gpt-5.6-luna":         "gpt-5.6-luna",
		"openai/gpt-5.6-terra":        "gpt-5.6-terra",
		"openai/gpt-5.6-sol":          "gpt-5.6-sol",
		"openai/gpt-6-astra":          "gpt-6-astra",
	}
	models := builtins()
	if len(models) != len(wantCLIID) {
		t.Fatalf("registry has %d models, the copilot expectations cover %d — update both together",
			len(models), len(wantCLIID))
	}
	for _, m := range models {
		id, ok := m.CLIModelID(HarnessCopilot)
		if !ok {
			t.Errorf("model %q is not supported by copilot", m.ID)
			continue
		}
		if want := wantCLIID[m.ID]; id != want {
			t.Errorf("model %q copilot CLI id = %q, want %q", m.ID, id, want)
		}
		if m.Preferred == HarnessCopilot {
			t.Errorf("model %q prefers copilot, want its vendor-native harness", m.ID)
		}
	}
}

func TestUsageCostUSD(t *testing.T) {
	m, _ := ModelByID(builtins(), "anthropic/claude-sonnet-5") // 2 / 0.20 / 10 per MTok
	// Fresh input and cache writes at 2/MTok, cache reads at the 0.20/MTok
	// cache-read rate, output at 10/MTok:
	// (1_000_000 + 200_000)/1e6*2 + 500_000/1e6*0.20 + 100_000/1e6*10
	//   = 2.4 + 0.1 + 1.0 = 3.5
	got := UsageCostUSD(m, 1_000_000, 500_000, 200_000, 100_000)
	if got == nil || *got != 3.5 {
		t.Errorf("UsageCostUSD = %v, want 3.5", got)
	}

	// Without a published cache-read rate, cache reads fall back to the base
	// input rate: (100 + 100)/1e6*2 + 100/1e6*2 + 50/1e6*10 = 0.0011.
	noCache := m
	noCache.CachedInputUSD = nil
	if got := UsageCostUSD(noCache, 100, 100, 100, 50); got == nil || *got != 0.0011 {
		t.Errorf("UsageCostUSD(no cached rate) = %v, want 0.0011", got)
	}
	// A model without published pricing prices to nil. Every builtin is
	// priced, so this is the config-override case: a synthetic model whose
	// vendor publishes no per-token rates.
	unpriced := Model{
		ID: "openai/private-preview", ProviderID: ProviderOpenAI, Name: "Private Preview",
		Supported: map[string]string{HarnessCodex: "private-preview"}, Preferred: HarnessCodex,
	}
	if got := UsageCostUSD(unpriced, 100, 0, 0, 50); got != nil {
		t.Errorf("UsageCostUSD(unpriced) = %v, want nil", *got)
	}
}

// TestAllModelsOverride replaces one provider's matrix and leaves the others.
func TestAllModelsOverride(t *testing.T) {
	override := map[string][]Model{
		ProviderOpenAI: {{
			ID: "openai/gpt-6", ProviderID: ProviderOpenAI, Name: "GPT-6",
			Supported: map[string]string{HarnessCodex: "gpt-6"}, Preferred: HarnessCodex,
		}},
	}
	got := AllModels(override)
	if _, ok := ModelByID(got, "openai/gpt-5.6-sol"); ok {
		t.Error("builtin openai/gpt-5.6-sol should be replaced by the override")
	}
	if _, ok := ModelByID(got, "openai/gpt-6"); !ok {
		t.Error("override openai/gpt-6 missing")
	}
	if _, ok := ModelByID(got, "anthropic/claude-sonnet-5"); !ok {
		t.Error("non-overridden anthropic models should remain")
	}
}

// TestHarnessBrokered: claude and codex run proxy-only through the egress
// broker; copilot holds its own token and the fake harness reaches no model.
func TestHarnessBrokered(t *testing.T) {
	for h, want := range map[string]bool{
		HarnessClaude: true, HarnessCodex: true, HarnessCopilot: false, HarnessFake: false, "": false,
	} {
		if got := HarnessBrokered(h); got != want {
			t.Errorf("HarnessBrokered(%q) = %v, want %v", h, got, want)
		}
	}
}

// TestHarnessProvider: a brokered harness's traffic goes to its vendor,
// read from the registry; every builtin model a brokered harness is
// preferred for is that vendor's, so the answer is not an accident of
// order.
func TestHarnessProvider(t *testing.T) {
	for h, want := range map[string]string{
		HarnessClaude: ProviderAnthropic, HarnessCodex: ProviderOpenAI, HarnessCopilot: "", HarnessFake: "", "": "",
	} {
		if got := HarnessProvider(h); got != want {
			t.Errorf("HarnessProvider(%q) = %q, want %q", h, got, want)
		}
	}
	for _, m := range Builtins() {
		if HarnessBrokered(m.Preferred) && m.ProviderID != HarnessProvider(m.Preferred) {
			t.Errorf("%s is preferred by %s but is %s's, not %s's", m.ID, m.Preferred, m.ProviderID,
				HarnessProvider(m.Preferred))
		}
	}
}
