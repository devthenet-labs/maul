// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

// seed keeps every property deterministic in the gate.
const seed = 20261016

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

func TestMeasuredCostUSD(t *testing.T) {
	sonnet, _ := ModelByID(Builtins(), "anthropic/claude-sonnet-5") // $3 in, $0.30 cached, $15 out
	got := MeasuredCostUSD(sonnet, MeasuredTokens{
		Input: 1_000_000, Output: 100_000, CacheRead: 1_000_000, CacheWrite5m: 1_000_000, CacheWrite1h: 1_000_000,
	})
	// 3 + 1.5 + 0.30 + 3.75 + 6.
	if got == nil || *got != 14.55 {
		t.Fatalf("MeasuredCostUSD = %v, want 14.55", got)
	}
	if got := MeasuredCostUSD(Model{ID: "x/unpriced"}, MeasuredTokens{Input: 5}); got != nil {
		t.Errorf("unpriced model cost = %v, want nil", *got)
	}
	if got := MeasuredCostUSD(sonnet, MeasuredTokens{Input: -5, Output: 1_000_000}); got == nil || *got != 15 {
		t.Errorf("negative input priced: %v, want 15", got)
	}
}

// TestMeasuredCostMonotonicProperty (seeded): adding tokens of any class
// never lowers a priced cost, and a priced cost is never negative.
func TestMeasuredCostMonotonicProperty(t *testing.T) {
	models := Builtins()
	cfg := quickConfig(3000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		tok := func() MeasuredTokens {
			return MeasuredTokens{Input: rnd.Int63n(1e8), Output: rnd.Int63n(1e8), CacheRead: rnd.Int63n(1e8),
				CacheWrite5m: rnd.Int63n(1e8), CacheWrite1h: rnd.Int63n(1e8)}
		}
		args[0] = reflect.ValueOf(rnd.Intn(len(models)))
		args[1] = reflect.ValueOf(tok())
		args[2] = reflect.ValueOf(tok())
	}
	prop := func(i int, a, b MeasuredTokens) bool {
		m := models[i]
		sum := MeasuredTokens{Input: a.Input + b.Input, Output: a.Output + b.Output,
			CacheRead: a.CacheRead + b.CacheRead, CacheWrite5m: a.CacheWrite5m + b.CacheWrite5m,
			CacheWrite1h: a.CacheWrite1h + b.CacheWrite1h}
		ca, cs := MeasuredCostUSD(m, a), MeasuredCostUSD(m, sum)
		if ca == nil || cs == nil {
			return ca == nil && cs == nil
		}
		return *ca >= 0 && *cs >= *ca
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	models := Builtins()
	for id, want := range map[string]string{
		"anthropic/claude-sonnet-5":         "anthropic/claude-sonnet-5",
		"claude-sonnet-5":                   "anthropic/claude-sonnet-5",
		"Claude-Sonnet-5":                   "anthropic/claude-sonnet-5",
		"claude-sonnet-5-20260514":          "anthropic/claude-sonnet-5",
		"claude-sonnet-5@20260514":          "anthropic/claude-sonnet-5",
		"claude-haiku-4-5-20251001-v1:0":    "anthropic/claude-haiku-4-5",
		"claude-opus-5-v2:0":                "anthropic/claude-opus-5",
		"anthropic/claude-opus-5-20260101":  "anthropic/claude-opus-5",
		"gpt-5.6-sol":                       "openai/gpt-5.6-sol",
		"claude-sonnet-5-1":                 "",
		"claude-sonnet-5x":                  "",
		"claude-sonnet":                     "",
		"(other)":                           "",
		"":                                  "",
		"claude-sonnet-5-2026051":           "",
		"claude-sonnet-5-20260514-extended": "",
	} {
		m, ok := Resolve(models, id)
		if got := map[bool]string{true: m.ID, false: ""}[ok]; got != want {
			t.Errorf("Resolve(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestResolveProperty (seeded): every registry model resolves from its
// canonical id, its bare id and any dated or versioned form of it.
func TestResolveProperty(t *testing.T) {
	models := Builtins()
	suffixes := []string{"", "-20260514", "@20251231", "-20251001-v1:0", "-v3:12"}
	cfg := quickConfig(2000)
	cfg.Values = func(args []reflect.Value, rnd *rand.Rand) {
		args[0] = reflect.ValueOf(rnd.Intn(len(models)))
		args[1] = reflect.ValueOf(rnd.Intn(len(suffixes)))
		args[2] = reflect.ValueOf(rnd.Intn(2) == 0)
	}
	prop := func(i, s int, canonical bool) bool {
		id := models[i].BareID() + suffixes[s]
		if canonical {
			id = models[i].ProviderID + "/" + id
		}
		m, ok := Resolve(models, id)
		return ok && m.ID == models[i].ID
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
}

// TestPriceTableVersionPinned: the built-in rates and cache multipliers
// hash to the digest recorded beside PriceTableVersion. A rate change fails
// here until the version is bumped and the digest re-recorded, so no priced
// figure can name a version whose rates it was not priced at.
func TestPriceTableVersionPinned(t *testing.T) {
	const (
		version = "2026-10-16"
		digest  = "828065fa4bf8b383"
	)
	type rate struct {
		ID               string
		In, Cached, Out  *float64
		Write5m, Write1h float64
	}
	rates := make([]rate, 0, len(Builtins()))
	for _, m := range Builtins() {
		rates = append(rates, rate{m.ID, m.InputUSD, m.CachedInputUSD, m.OutputUSD,
			CacheWrite5mMultiplier, CacheWrite1hMultiplier})
	}
	blob, err := json.Marshal(rates)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	if got := hex.EncodeToString(sum[:8]); got != digest || PriceTableVersion != version {
		t.Fatalf("the price table is %s at digest %s; pinned %s at %s: bump PriceTableVersion and re-pin both",
			PriceTableVersion, got, version, digest)
	}
}
