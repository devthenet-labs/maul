// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package model

import (
	"regexp"
	"strings"
)

// PriceTableVersion names the rates of the built-in registry. Every figure
// priced from measured tokens records it, so a cost can be told apart from
// one priced at other rates. It is "<date>.<digest>", the digest a hash of
// every built-in rate and cache multiplier: TestPriceTableVersionPinned fails
// whenever the rates change until the version is set to their new digest, so
// the recorded version always changes with them.
const PriceTableVersion = "2026-10-16.828065fa4bf8b383"

// Cache-write multipliers over the base input rate: a five-minute prompt
// cache write bills at 1.25x, a one-hour one at 2x (Anthropic's published
// rates; only brokered Anthropic traffic is measured by token class).
const (
	CacheWrite5mMultiplier = 1.25
	CacheWrite1hMultiplier = 2.0
)

// MeasuredTokens is one model's measured usage by token class.
type MeasuredTokens struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// MeasuredCostUSD prices measured tokens at m's list rates, rounded to
// micro-USD, or nil when m has no published pricing. Unlike UsageCostUSD (a
// fallback for a harness that reports no cost) it prices cache writes at
// their multipliers. A negative count prices as zero.
func MeasuredCostUSD(m Model, t MeasuredTokens) *float64 {
	if m.InputUSD == nil || m.OutputUSD == nil {
		return nil
	}
	n := func(v int64) float64 { return float64(max(v, 0)) / 1e6 }
	in := *m.InputUSD
	cost := n(t.Input)*in +
		n(t.CacheWrite5m)*in*CacheWrite5mMultiplier +
		n(t.CacheWrite1h)*in*CacheWrite1hMultiplier +
		n(t.CacheRead)*cachedInputRate(m) +
		n(t.Output)**m.OutputUSD
	cost = round6(cost)
	return &cost
}

// datedSuffix is what a provider may append to a registry model's bare id
// on the wire: a dated snapshot ("-20260514", vertex's "@20260514"), with or
// without a bedrock version ("-v1:0"), or the version alone.
var datedSuffix = regexp.MustCompile(`^([-@][0-9]{8}(-v[0-9]+:[0-9]+)?|-v[0-9]+:[0-9]+)?$`)

// Resolve normalises a model id as a provider or the broker reports it to
// the registry model it names: a canonical id ("anthropic/claude-sonnet-5"),
// a bare one ("claude-sonnet-5"), or either with a dated or versioned
// suffix. A Bedrock geo or "anthropic." prefix must already be stripped (the
// broker reports bare ids). An id no registry model accounts for is not
// resolved: its tokens are kept and its cost is unknown, never guessed.
func Resolve(models []Model, id string) (Model, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if m, ok := ModelByID(models, id); ok {
		return m, true
	}
	if _, rest, ok := strings.Cut(id, "/"); ok {
		id = rest
	}
	var best Model
	found := false
	for _, m := range models {
		bare := strings.ToLower(m.BareID())
		rest, ok := strings.CutPrefix(id, bare)
		if !ok || !datedSuffix.MatchString(rest) {
			continue
		}
		if !found || len(bare) > len(best.BareID()) {
			best, found = m, true
		}
	}
	return best, found
}
