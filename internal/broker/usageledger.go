// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package broker

import (
	"regexp"
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/usagewire"
)

// modelUsage is what one pod consumed of one model: the tokens observed in
// the responses' usage objects, kept apart from the worst-case shortfall
// charged for responses whose usage never became final. The limits count
// both; only the observed tokens are ever priced (design revision 1, L2).
type modelUsage struct {
	route      string
	credential string
	observed   usage
	shortfall  int64
}

// modelKeyRE is the grammar a bare model id must match to key its own entry.
var modelKeyRE = regexp.MustCompile(usagewire.ModelPattern)

// modelKey is the ledger key for a request's model: its bare id when that is
// in the grammar, else usagewire.ModelInvalid. The id is caller-controlled
// (an empty allowlist admits any string), so the key is bounded here.
func modelKey(wire string) string {
	if id := bareModel(wire); modelKeyRE.MatchString(id) {
		return id
	}
	return usagewire.ModelInvalid
}

// modelLocked returns p's entry for the model, creating it; past
// usagewire.MaxModels distinct ids every new one shares
// usagewire.ModelOther. Called under the ledger lock.
func (p *podCounters) modelLocked(model, route, credential string) *modelUsage {
	if p.models == nil {
		p.models = map[string]*modelUsage{}
	}
	if m, ok := p.models[model]; ok {
		return m
	}
	if len(p.models) >= usagewire.MaxModels {
		model = usagewire.ModelOther
		if m, ok := p.models[model]; ok {
			return m
		}
	}
	m := &modelUsage{route: route, credential: credential}
	p.models[model] = m
	return m
}

// observe records an observed usage increment against pod's model and
// charges its tokens, and their price, to the limits. It returns the price
// in micro-USD.
func (l *ledger) observe(pod, model, route, credential string, d usage) int64 {
	if d.total() <= 0 {
		return 0
	}
	cost := l.prices.cost(model, d)
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.podLocked(pod, l.now())
	m := p.modelLocked(modelKey(model), route, credential)
	m.observed = m.observed.mergeAdd(d)
	l.chargeLocked(p, d.total(), cost)
	return cost
}

// shortfall records the worst-case charge for a response whose usage never
// became final against pod's model, apart from what was observed, and
// charges it to the limits, priced at the model's output rate (it is
// unmetered, so its class is unknown). It returns that price in micro-USD.
func (l *ledger) shortfall(pod, model, route, credential string, n int64) int64 {
	if n <= 0 {
		return 0
	}
	cost := l.prices.outputCost(model, n)
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.podLocked(pod, l.now())
	p.modelLocked(modelKey(model), route, credential).shortfall += n
	l.chargeLocked(p, n, cost)
	return cost
}

// jobUsage is the ledger entries of job's pods, sorted by name and UID,
// each with its models sorted by id. It never creates an entry.
func (l *ledger) jobUsage(job string) []usagewire.Pod {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []usagewire.Pod
	for key, p := range l.pods {
		name, uid, _ := strings.Cut(key, "/")
		if !usagewire.PodOfJob(name, job) {
			continue
		}
		pod := usagewire.Pod{Name: name, UID: uid, FirstSeen: p.firstSeen.UTC(), Inflight: p.inflight,
			Models: []usagewire.Model{}}
		for id, m := range p.models {
			pod.Models = append(pod.Models, usagewire.Model{
				Model: id, Route: m.route, Credential: m.credential,
				Input: m.observed.input, Output: m.observed.output, CacheRead: m.observed.cacheRead,
				CacheWrite5m: m.observed.cacheWrite5m(), CacheWrite1h: m.observed.cacheWrite1h,
				EstimatedShortfall: m.shortfall,
			})
		}
		slices.SortFunc(pod.Models, func(a, b usagewire.Model) int { return strings.Compare(a.Model, b.Model) })
		out = append(out, pod)
	}
	slices.SortFunc(out, func(a, b usagewire.Pod) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.UID, b.UID)
	})
	return out
}
