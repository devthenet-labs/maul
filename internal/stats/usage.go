// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package stats

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
)

// Usage run kinds: the keys of a UsageBucket's Kinds.
const (
	UsageKindInvestigation = "investigation"
	UsageKindRemediation   = "remediation"
	UsageKindPlan          = "plan"
	UsageKindBuild         = "build"
	UsageKindRevise        = "revise"
	UsageKindCheckFix      = "check-fix"
	UsageKindEvaluation    = "evaluation"
)

// Bucket key layouts (UTC).
const (
	usageDayLayout   = "2006-01-02"
	usageMonthLayout = "2006-01"
)

// UsageDelta is one settled run's contribution to its UsageRollups.
type UsageDelta struct {
	// Kind is one of the UsageKind constants.
	Kind string
	// CompletedAt buckets the run (its UTC day and month).
	CompletedAt time.Time
	// Intent and Project name the run's Intent and its Project; empty for
	// a run of no intent.
	Intent, Project string
	// Source is the figure the run counts as: measured or partial
	// (trusted), or reported.
	Source v1alpha1.UsageSource
	// Usage is the run's contribution: Runs is 1, and either the trusted
	// fields or the Reported ones are set, never both.
	Usage v1alpha1.UsageAggregate
}

// Trusted reports whether the delta's figures are the broker's.
func (d UsageDelta) Trusted() bool {
	return d.Source == v1alpha1.UsageSourceMeasured || d.Source == v1alpha1.UsageSourcePartial
}

// Tokens is the delta's metered tokens, every class (zero when reported).
func (d UsageDelta) Tokens() int64 {
	u := d.Usage
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// UsageDeltaFrom derives a settled run's delta from its usage. A run counts
// as metered (measured or partial) only when its usage is final with a
// broker figure; anything else (no usage at all, measurement off, a
// non-brokered harness, a run deleted before its usage settled) counts as
// reported, with the pod's own figures kept apart. Only observed tokens are
// priced, each model at its own pricing; a model with tokens and no price
// marks the run unpriced. A malformed cost is counted as zero and returned
// as the error, so the caller can log it and still aggregate.
func UsageDeltaFrom(kind string, u *v1alpha1.UsageSummary, completedAt time.Time) (UsageDelta, error) {
	d := UsageDelta{Kind: kind, CompletedAt: completedAt, Source: v1alpha1.UsageSourceReported}
	d.Usage.Runs = 1
	if u == nil {
		d.Usage.ReportedRuns = 1
		return d, nil
	}
	m := u.Measured
	if !u.Final || m == nil || (u.Source != v1alpha1.UsageSourceMeasured && u.Source != v1alpha1.UsageSourcePartial) {
		d.Usage.ReportedRuns = 1
		d.Usage.ReportedTokens = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
		cost, err := ParseCostMicroUSD(u.CostUSD)
		d.Usage.ReportedCostMicroUSD = cost
		return d, err
	}
	d.Source = u.Source
	if u.Source == v1alpha1.UsageSourcePartial {
		d.Usage.PartialRuns = 1
	}
	d.Usage.InputTokens = m.InputTokens
	d.Usage.OutputTokens = m.OutputTokens
	d.Usage.CacheReadTokens = m.CacheReadTokens
	d.Usage.CacheWriteTokens = m.CacheWriteTokens
	d.Usage.UnmeteredTokens = m.EstimatedShortfallTokens

	var firstErr error
	price := func(cost string, pricing v1alpha1.UsagePricing) {
		micro, err := ParseCostMicroUSD(cost)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if pricing == v1alpha1.UsagePricingSubscription {
			d.Usage.NotionalCostMicroUSD += micro
		} else {
			d.Usage.CostMicroUSD += micro
		}
	}
	unpriced := false
	if len(m.Models) == 0 {
		if m.CostUSD != "" {
			price(m.CostUSD, m.Pricing)
		} else {
			unpriced = d.Tokens() > 0
		}
	}
	for _, mu := range m.Models {
		if mu.CostUSD == "" {
			if mu.InputTokens+mu.OutputTokens+mu.CacheReadTokens+mu.CacheWrite5mTokens+mu.CacheWrite1hTokens > 0 {
				unpriced = true
			}
			continue
		}
		price(mu.CostUSD, mu.Pricing)
	}
	if unpriced {
		d.Usage.UnpricedRuns = 1
	}
	return d, firstErr
}

// UsageUser is the UsageRollup user key of an attribution: "org", or
// "github-id:<id>", exactly the attributed-to annotation's value.
func UsageUser(a *v1alpha1.Attribution) string {
	return attribution.Value(a)
}

// UsageScopes are the scopes a run attributed to user contributes to: the
// user's, and with a project the user's in that project and the project's.
func UsageScopes(user, project string) []v1alpha1.UsageScope {
	out := []v1alpha1.UsageScope{{Type: v1alpha1.UsageScopeUser, User: user}}
	if project != "" {
		out = append(out,
			v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUserProject, User: user, Project: project},
			v1alpha1.UsageScope{Type: v1alpha1.UsageScopeProject, Project: project})
	}
	return out
}

// UsageObjectName is the deterministic UsageRollup name of a scope:
// usage-<type>-<16 hex of sha256(user NUL project)>. A hash, because neither
// a project name nor a user key is bounded to a name fragment, and the NUL
// keeps (user, project) pairs from colliding by concatenation.
func UsageObjectName(scope v1alpha1.UsageScope) string {
	sum := sha256.Sum256([]byte(scope.User + "\x00" + scope.Project))
	return "usage-" + string(scope.Type) + "-" + hex.EncodeToString(sum[:8])
}

// ApplyUsage folds one run's delta into a UsageRollup status under the
// exactly-once ledger: it returns false, mutating nothing, when ledgerKey
// was already applied. The run lands in its UTC day (unless that day is
// already past retention) and its month; topIntents keeps the month's top
// intents (the user scope). login, when set, becomes the display login.
// Retention is enforced before returning, so the status never exceeds its
// schema's bounds.
func ApplyUsage(st *v1alpha1.UsageRollupStatus, ledgerKey string, d UsageDelta, login string,
	topIntents bool, now time.Time) bool {
	if slices.Contains(st.Recent, ledgerKey) {
		return false
	}
	if st.SchemaVersion < v1alpha1.UsageRollupSchemaVersion {
		st.SchemaVersion = v1alpha1.UsageRollupSchemaVersion
	}
	mt := metav1.NewTime(now.UTC())
	if st.FirstProcessed == nil {
		st.FirstProcessed = &mt
	}
	st.LastProcessed = &mt
	if login != "" {
		st.Login = login
	}

	at := d.CompletedAt.UTC()
	if day := at.Format(usageDayLayout); day >= usageDayCutoff(now) {
		if st.Days == nil {
			st.Days = map[string]v1alpha1.UsageBucket{}
		}
		b := st.Days[day]
		addBucket(&b, d)
		st.Days[day] = b
	}
	if st.Months == nil {
		st.Months = map[string]v1alpha1.UsageMonthBucket{}
	}
	month := at.Format(usageMonthLayout)
	mb := st.Months[month]
	addBucket(&mb.UsageBucket, d)
	if topIntents && d.Intent != "" {
		mb.TopIntents = addTopIntent(mb.TopIntents, d)
	}
	st.Months[month] = mb

	CompactUsage(st, now)
	st.Recent = append(st.Recent, ledgerKey)
	if len(st.Recent) > maxLedger {
		st.Recent = st.Recent[len(st.Recent)-maxLedger:]
	}
	return true
}

// CompactUsage enforces a UsageRollup's retention in place: day buckets
// older than v1alpha1.UsageRetentionDays days before now (UTC) are dropped,
// then the oldest beyond that many (a clock-skewed future day can only
// crowd out the oldest), and months beyond v1alpha1.UsageRetentionMonths,
// oldest first. Every run is in its month as well as its day, so a dropped
// day loses detail, never a total. It reports whether anything changed;
// a second call changes nothing.
func CompactUsage(st *v1alpha1.UsageRollupStatus, now time.Time) bool {
	changed := false
	cutoff := usageDayCutoff(now)
	for day := range st.Days {
		if day < cutoff {
			delete(st.Days, day)
			changed = true
		}
	}
	if dropOldest(st.Days, v1alpha1.UsageRetentionDays) {
		changed = true
	}
	if dropOldest(st.Months, v1alpha1.UsageRetentionMonths) {
		changed = true
	}
	return changed
}

// usageDayCutoff is the oldest day key kept at now: retention counts today
// as the first of its days.
func usageDayCutoff(now time.Time) string {
	return now.UTC().AddDate(0, 0, -(v1alpha1.UsageRetentionDays - 1)).Format(usageDayLayout)
}

// dropOldest deletes m's lexically smallest keys (the oldest, for date keys)
// until at most keep remain.
func dropOldest[V any](m map[string]V, keep int) bool {
	if len(m) <= keep {
		return false
	}
	keys := slices.Sorted(maps.Keys(m))
	for _, k := range keys[:len(keys)-keep] {
		delete(m, k)
	}
	return true
}

// addBucket folds d into a bucket and its kind's totals.
func addBucket(b *v1alpha1.UsageBucket, d UsageDelta) {
	addAggregate(&b.UsageAggregate, d.Usage)
	if b.Kinds == nil {
		b.Kinds = map[string]v1alpha1.UsageKindTotals{}
	}
	k := b.Kinds[d.Kind]
	k.Runs += d.Usage.Runs
	k.Tokens += d.Tokens()
	k.CostMicroUSD += d.Usage.CostMicroUSD
	k.NotionalCostMicroUSD += d.Usage.NotionalCostMicroUSD
	b.Kinds[d.Kind] = k
}

func addAggregate(a *v1alpha1.UsageAggregate, d v1alpha1.UsageAggregate) {
	a.Runs += d.Runs
	a.PartialRuns += d.PartialRuns
	a.ReportedRuns += d.ReportedRuns
	a.UnpricedRuns += d.UnpricedRuns
	a.InputTokens += d.InputTokens
	a.OutputTokens += d.OutputTokens
	a.CacheReadTokens += d.CacheReadTokens
	a.CacheWriteTokens += d.CacheWriteTokens
	a.UnmeteredTokens += d.UnmeteredTokens
	a.CostMicroUSD += d.CostMicroUSD
	a.NotionalCostMicroUSD += d.NotionalCostMicroUSD
	a.ReportedTokens += d.ReportedTokens
	a.ReportedCostMicroUSD += d.ReportedCostMicroUSD
}

// addTopIntent folds d into a month's top intents: an intent already listed
// grows; a new one is listed while there is room, or in place of the last
// entry when its run outranks it. The list stays sorted, highest first.
func addTopIntent(top []v1alpha1.UsageTopIntent, d UsageDelta) []v1alpha1.UsageTopIntent {
	i := slices.IndexFunc(top, func(t v1alpha1.UsageTopIntent) bool { return t.Intent == d.Intent })
	if i < 0 {
		entry := v1alpha1.UsageTopIntent{Intent: d.Intent, Project: d.Project}
		switch {
		case len(top) < v1alpha1.MaxTopIntents:
			top = append(top, entry)
			i = len(top) - 1
		default:
			slices.SortStableFunc(top, compareTopIntent)
			last := len(top) - 1
			cand := entry
			addTop(&cand, d)
			if compareTopIntent(cand, top[last]) >= 0 {
				return top // does not outrank the last entry
			}
			top[last] = entry
			i = last
		}
	}
	addTop(&top[i], d)
	slices.SortStableFunc(top, compareTopIntent)
	return top
}

func addTop(t *v1alpha1.UsageTopIntent, d UsageDelta) {
	t.Runs += d.Usage.Runs
	t.Tokens += d.Tokens()
	t.CostMicroUSD += d.Usage.CostMicroUSD
	t.NotionalCostMicroUSD += d.Usage.NotionalCostMicroUSD
}

// compareTopIntent orders top intents highest first: by cost (list and
// notional together), then tokens, then runs, then name.
func compareTopIntent(a, b v1alpha1.UsageTopIntent) int {
	return cmp.Or(
		cmp.Compare(b.CostMicroUSD+b.NotionalCostMicroUSD, a.CostMicroUSD+a.NotionalCostMicroUSD),
		cmp.Compare(b.Tokens, a.Tokens),
		cmp.Compare(b.Runs, a.Runs),
		cmp.Compare(a.Intent, b.Intent),
	)
}
