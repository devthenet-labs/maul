// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"math"

	"github.com/bitwise-media-group/patchy/internal/model"
)

// maxChargeMicroUSD caps one priced charge or reservation at a billion
// dollars. A request's max_tokens is caller-controlled, so its priced
// reservation is bounded here, and so is every sum the ledger forms of
// charges: no limit check can overflow.
const maxChargeMicroUSD = 1_000_000_000_000_000

// pricer prices brokered tokens in micro-USD for the cost limits
// (--limit-unit=cost). It prices through model.MeasuredCostUSD, the function
// the run controllers price a run's measured usage with (L2), over the
// built-in registry, so a limit and the figure a run reports agree. Unlike
// that figure, a model the registry cannot price is never free here: it is
// priced at the ceiling, the highest rate of each token class across every
// priced model, so an unknown or misspelt id fails safe. A subscription's
// traffic is priced the same way, notionally.
type pricer struct {
	models  []model.Model
	ceiling model.Model
}

func newPricer(models []model.Model) *pricer {
	ceiling := model.Model{ID: "ceiling"}
	high := func(cur, v *float64) *float64 {
		if v != nil && (cur == nil || *v > *cur) {
			return v
		}
		return cur
	}
	for _, m := range models {
		if m.InputUSD == nil || m.OutputUSD == nil {
			continue
		}
		ceiling.InputUSD = high(ceiling.InputUSD, m.InputUSD)
		ceiling.OutputUSD = high(ceiling.OutputUSD, m.OutputUSD)
		cached := m.CachedInputUSD
		if cached == nil {
			cached = m.InputUSD
		}
		ceiling.CachedInputUSD = high(ceiling.CachedInputUSD, cached)
	}
	return &pricer{models: models, ceiling: ceiling}
}

// modelFor is the registry model a wire id names when it has pricing, else
// the ceiling.
func (p *pricer) modelFor(wire string) model.Model {
	if m, ok := model.Resolve(p.models, bareModel(wire)); ok && m.InputUSD != nil && m.OutputUSD != nil {
		return m
	}
	return p.ceiling
}

// cost prices one usage increment of the wire model, in micro-USD.
func (p *pricer) cost(wire string, u usage) int64 {
	if u.total() <= 0 {
		return 0
	}
	return microUSD(model.MeasuredCostUSD(p.modelFor(wire), model.MeasuredTokens{
		Input: u.input, Output: u.output, CacheRead: u.cacheRead,
		CacheWrite5m: u.cacheWrite5m(), CacheWrite1h: u.cacheWrite1h,
	}))
}

// outputCost prices n output tokens of the wire model, in micro-USD: the
// reservation for a request's output bound, and the charge for a shortfall,
// which is unmetered and so counts at the output price (design §1.1).
func (p *pricer) outputCost(wire string, n int64) int64 {
	if n <= 0 {
		return 0
	}
	return microUSD(model.MeasuredCostUSD(p.modelFor(wire), model.MeasuredTokens{Output: n}))
}

// microUSD converts a priced figure, which model.MeasuredCostUSD has already
// rounded to the micro-USD as a run's reported cost is, to an integer
// (absorbing float error), capped at maxChargeMicroUSD. A
// nil figure (no pricing, which the ceiling rules out unless the registry
// prices nothing at all) is charged the cap: failing safe.
func microUSD(usd *float64) int64 {
	if usd == nil {
		return maxChargeMicroUSD
	}
	v := math.Round(*usd * 1e6)
	switch {
	case v <= 0 || math.IsNaN(v):
		return 0
	case v >= maxChargeMicroUSD:
		return maxChargeMicroUSD
	}
	return int64(v)
}
