// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagemeter

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/model"
	"github.com/devthenet-labs/maul/internal/usagewire"
)

// maxModels is the schema's bound on MeasuredUsage.Models.
const maxModels = 16

// maxModelID is the schema's bound on ModelUsage.Model.
const maxModelID = 160

// Price builds the measured figure from the run's pods' entries, as Pods
// selects them from the broker's answer: every pod's usage summed per model
// and credential kind, each model named by its registry id when its
// reported id resolves to one, its observed tokens priced at the list rates
// of model.PriceTableVersion, an unknown model's tokens kept unpriced, and
// the estimated shortfall kept apart and never priced. A model paid for
// with two kinds (a person who swapped an API key for a setup-token mid-run)
// stays two entries, so a subscription's notional cost is never counted as
// money spent, nor the reverse. The total cost is the sum of the models'
// costs, and absent when any model with observed tokens has none.
func Price(models []model.Model, pods []usagewire.Pod, boot *metav1.Time) *v1alpha1.MeasuredUsage {
	type acc struct {
		resolved   *model.Model
		credential string
		tokens     model.MeasuredTokens
		shortfall  int64
	}
	// key is one entry: a model and the kind that paid for it. The overflow
	// entry, ModelOther, is one key whatever paid, so the bound holds; it is
	// never priced, so its kind decides nothing.
	type key struct{ id, credential string }
	keyOf := func(id, credential string) key {
		if id == usagewire.ModelOther {
			return key{id: id}
		}
		return key{id, credential}
	}
	byKey := map[key]*acc{}
	var keys []key
	for _, p := range pods {
		for _, w := range p.Models {
			id, resolved := modelID(models, w.Model)
			credential := ""
			if w.Credential != "" {
				credential = credentialKind(w.Credential)
			}
			k := keyOf(id, credential)
			a, ok := byKey[k]
			if !ok {
				if len(keys) >= maxModels-1 && id != usagewire.ModelOther {
					k, resolved = keyOf(usagewire.ModelOther, credential), nil
				}
				if a, ok = byKey[k]; !ok {
					a = &acc{resolved: resolved}
					byKey[k] = a
					keys = append(keys, k)
				}
			}
			if credential != "" {
				a.credential = credential
			}
			a.tokens.Input = sat(a.tokens.Input, w.Input)
			a.tokens.Output = sat(a.tokens.Output, w.Output)
			a.tokens.CacheRead = sat(a.tokens.CacheRead, w.CacheRead)
			a.tokens.CacheWrite5m = sat(a.tokens.CacheWrite5m, w.CacheWrite5m)
			a.tokens.CacheWrite1h = sat(a.tokens.CacheWrite1h, w.CacheWrite1h)
			a.shortfall = sat(a.shortfall, w.EstimatedShortfall)
		}
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := strings.Compare(a.id, b.id); c != 0 {
			return c
		}
		return strings.Compare(a.credential, b.credential)
	})
	out := &v1alpha1.MeasuredUsage{BrokerBootTime: boot}
	var total float64
	priced, unpriced, subscription := false, false, false
	for _, k := range keys {
		a := byKey[k]
		t := a.tokens
		mu := v1alpha1.ModelUsage{
			Model: k.id, Credential: a.credential,
			InputTokens: t.Input, OutputTokens: t.Output, CacheReadTokens: t.CacheRead,
			CacheWrite5mTokens: t.CacheWrite5m, CacheWrite1hTokens: t.CacheWrite1h,
			EstimatedShortfallTokens: a.shortfall,
		}
		observed := t.Input+t.Output+t.CacheRead+t.CacheWrite5m+t.CacheWrite1h > 0
		var cost *float64
		if a.resolved != nil {
			cost = model.MeasuredCostUSD(*a.resolved, t)
		}
		switch {
		case cost != nil:
			mu.CostUSD = formatUSD(*cost)
			mu.PriceTable = model.PriceTableVersion
			mu.Pricing = pricingOf(a.credential)
			total += *cost
			priced = true
			subscription = subscription || mu.Pricing == v1alpha1.UsagePricingSubscription
		case observed:
			unpriced = true
		}
		out.InputTokens = sat(out.InputTokens, t.Input)
		out.OutputTokens = sat(out.OutputTokens, t.Output)
		out.CacheReadTokens = sat(out.CacheReadTokens, t.CacheRead)
		out.CacheWriteTokens = sat(out.CacheWriteTokens, sat(t.CacheWrite5m, t.CacheWrite1h))
		out.EstimatedShortfallTokens = sat(out.EstimatedShortfallTokens, a.shortfall)
		out.Models = append(out.Models, mu)
	}
	if priced && !unpriced {
		out.CostUSD = formatUSD(total)
		out.PriceTable = model.PriceTableVersion
		out.Pricing = v1alpha1.UsagePricingList
		if subscription {
			out.Pricing = v1alpha1.UsagePricingSubscription
		}
	}
	return out
}

// modelID names a reported model: the registry's canonical id when it
// resolves, else the reported id, bounded.
func modelID(models []model.Model, reported string) (string, *model.Model) {
	if m, ok := model.Resolve(models, reported); ok {
		return m.ID, &m
	}
	id := strings.TrimSpace(reported)
	if id == "" {
		id = usagewire.ModelInvalid
	}
	if len(id) > maxModelID {
		id = id[:maxModelID]
	}
	return id, nil
}

// credentialKind bounds a reported credential kind to the schema's: one of
// the known kinds as is, anything else "unknown", so a value the API server
// would refuse can never stall a run's status write.
func credentialKind(c string) string {
	switch c {
	case usagewire.CredentialAPIKey, usagewire.CredentialOAuthToken, usagewire.CredentialCloudIdentity:
		return c
	}
	return "unknown"
}

// pricingOf is what a credential kind's priced figure means.
func pricingOf(credential string) v1alpha1.UsagePricing {
	if credential == usagewire.CredentialOAuthToken {
		return v1alpha1.UsagePricingSubscription
	}
	return v1alpha1.UsagePricingList
}

// formatUSD renders a cost in the schema's decimal form (micro-USD), "0"
// included: a priced model with no observed tokens cost nothing, which is
// not the same as an unknown cost.
func formatUSD(f float64) string {
	if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		f = 0
	}
	return strconv.FormatFloat(f, 'f', 6, 64)
}

// sat adds two non-negative counts, saturating instead of wrapping and
// treating a negative count as zero.
func sat(a, b int64) int64 {
	a, b = max(a, 0), max(b, 0)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func metaTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}
