// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
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

// TestMeasuredCostHalfMicroTie pins a sum that lands exactly on half a
// micro-USD: 5000 in (15000) + 10 written (37.5) + 20 read (6) + 1 out (15) =
// 15058.5 micro-USD, which rounds up to 15059. Summed in USD with the products
// fused into the additions, arm64 rounded it up and amd64 down, so the broker's
// cost limit (CI on linux/amd64) disagreed with the same figure on arm64.
func TestMeasuredCostHalfMicroTie(t *testing.T) {
	sonnet, _ := ModelByID(Builtins(), "anthropic/claude-sonnet-5")
	got := MeasuredCostUSD(sonnet, MeasuredTokens{Input: 5000, CacheWrite5m: 10, CacheRead: 20, Output: 1})
	if got == nil || *got != 0.015059 {
		t.Fatalf("MeasuredCostUSD = %v, want 0.015059", got)
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
		"gpt-5.3-codex-2026-02-05":          "openai/gpt-5.3-codex",
		"openai/gpt-5.6-luna-2026-10-01":    "openai/gpt-5.6-luna",
		"gpt-5.6-sol-2026-13-01":            "",
		"gpt-5.6-sol-2026-10-32":            "",
		"gpt-5.6-sol-2026-1001":             "",
		"gpt-5.6-sol-2026-10-01-v1:0":       "",
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
	suffixes := []string{"", "-20260514", "@20251231", "-20251001-v1:0", "-v3:12", "-2026-05-14"}
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

// priceDigest hashes what a priced figure depends on: every model's rates
// and the cache-write multipliers.
func priceDigest(t *testing.T, models []Model, write5m, write1h float64) string {
	t.Helper()
	type rate struct {
		ID               string
		In, Cached, Out  *float64
		Write5m, Write1h float64
	}
	rates := make([]rate, 0, len(models))
	for _, m := range models {
		rates = append(rates, rate{m.ID, m.InputUSD, m.CachedInputUSD, m.OutputUSD, write5m, write1h})
	}
	blob, err := json.Marshal(rates)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:8])
}

// TestPriceTableVersionPinned: PriceTableVersion is "<date>.<digest>", the
// digest that of the built-in rates and cache multipliers. The digest is part
// of the recorded version itself, so a rate change cannot pass here while the
// version every priced figure records stays the same: no priced figure can
// name a version whose rates it was not priced at.
func TestPriceTableVersionPinned(t *testing.T) {
	digest := priceDigest(t, Builtins(), CacheWrite5mMultiplier, CacheWrite1hMultiplier)
	date, got, ok := strings.Cut(PriceTableVersion, ".")
	if !ok || got != digest {
		t.Fatalf("PriceTableVersion = %q, but the rates hash to %s: set it to \"<today>.%s\"",
			PriceTableVersion, digest, digest)
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		t.Errorf("PriceTableVersion %q does not start with a YYYY-MM-DD date: %v", PriceTableVersion, err)
	}
}

// TestPriceDigestCoversEveryRate: changing any one rate or multiplier moves
// the digest, so TestPriceTableVersionPinned fails for each kind of change.
func TestPriceDigestCoversEveryRate(t *testing.T) {
	base := priceDigest(t, Builtins(), CacheWrite5mMultiplier, CacheWrite1hMultiplier)
	bump := func(p *float64) *float64 {
		v := 1.0
		if p != nil {
			v = *p + 0.01
		}
		return &v
	}
	cases := map[string]func([]Model) ([]Model, float64, float64){
		"input": func(ms []Model) ([]Model, float64, float64) {
			ms[0].InputUSD = bump(ms[0].InputUSD)
			return ms, CacheWrite5mMultiplier, CacheWrite1hMultiplier
		},
		"cached input": func(ms []Model) ([]Model, float64, float64) {
			ms[0].CachedInputUSD = bump(ms[0].CachedInputUSD)
			return ms, CacheWrite5mMultiplier, CacheWrite1hMultiplier
		},
		"output": func(ms []Model) ([]Model, float64, float64) {
			ms[len(ms)-1].OutputUSD = bump(ms[len(ms)-1].OutputUSD)
			return ms, CacheWrite5mMultiplier, CacheWrite1hMultiplier
		},
		"5m multiplier": func(ms []Model) ([]Model, float64, float64) {
			return ms, CacheWrite5mMultiplier + 0.25, CacheWrite1hMultiplier
		},
		"1h multiplier": func(ms []Model) ([]Model, float64, float64) {
			return ms, CacheWrite5mMultiplier, CacheWrite1hMultiplier + 0.5
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			ms, w5, w1 := change(append([]Model(nil), Builtins()...))
			if got := priceDigest(t, ms, w5, w1); got == base {
				t.Errorf("digest unchanged at %s after changing the %s", got, name)
			}
		})
	}
}
