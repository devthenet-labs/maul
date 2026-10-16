// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package stats

import (
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
)

var usageNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func measuredUsage(source v1alpha1.UsageSource, models ...v1alpha1.ModelUsage) *v1alpha1.UsageSummary {
	m := &v1alpha1.MeasuredUsage{}
	for _, mu := range models {
		m.InputTokens += mu.InputTokens
		m.OutputTokens += mu.OutputTokens
		m.CacheReadTokens += mu.CacheReadTokens
		m.CacheWriteTokens += mu.CacheWrite5mTokens + mu.CacheWrite1hTokens
		m.EstimatedShortfallTokens += mu.EstimatedShortfallTokens
	}
	m.Models = models
	return &v1alpha1.UsageSummary{
		InputTokens: 999, OutputTokens: 999, CostUSD: "9.99", // the pod's own, ignored when metered
		Source: source, Final: true, Measured: m,
	}
}

func TestUsageDeltaFrom(t *testing.T) {
	at := usageNow
	tests := []struct {
		name    string
		u       *v1alpha1.UsageSummary
		want    v1alpha1.UsageAggregate
		source  v1alpha1.UsageSource
		wantErr bool
	}{
		{
			name:   "no usage at all is a reported run with nothing",
			u:      nil,
			want:   v1alpha1.UsageAggregate{Runs: 1, ReportedRuns: 1},
			source: v1alpha1.UsageSourceReported,
		},
		{
			name: "not final counts the pod's figures as reported",
			u: &v1alpha1.UsageSummary{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheCreationTokens: 1,
				CostUSD: "0.25", Source: v1alpha1.UsageSourceMeasured, Measured: &v1alpha1.MeasuredUsage{InputTokens: 7}},
			want:   v1alpha1.UsageAggregate{Runs: 1, ReportedRuns: 1, ReportedTokens: 18, ReportedCostMicroUSD: 250_000},
			source: v1alpha1.UsageSourceReported,
		},
		{
			name: "final reported (codex) stays out of trusted totals",
			u: &v1alpha1.UsageSummary{InputTokens: 10, CostUSD: "1", Source: v1alpha1.UsageSourceReported,
				Final: true},
			want:   v1alpha1.UsageAggregate{Runs: 1, ReportedRuns: 1, ReportedTokens: 10, ReportedCostMicroUSD: 1_000_000},
			source: v1alpha1.UsageSourceReported,
		},
		{
			name: "measured prices each model at its own pricing",
			u: measuredUsage(v1alpha1.UsageSourceMeasured,
				v1alpha1.ModelUsage{Model: "a", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10,
					CacheWrite5mTokens: 3, CacheWrite1hTokens: 2, EstimatedShortfallTokens: 7,
					CostUSD: "0.5", Pricing: v1alpha1.UsagePricingList},
				v1alpha1.ModelUsage{Model: "b", InputTokens: 20, CostUSD: "0.125",
					Pricing: v1alpha1.UsagePricingSubscription}),
			want: v1alpha1.UsageAggregate{Runs: 1, InputTokens: 120, OutputTokens: 50, CacheReadTokens: 10,
				CacheWriteTokens: 5, UnmeteredTokens: 7, CostMicroUSD: 500_000, NotionalCostMicroUSD: 125_000},
			source: v1alpha1.UsageSourceMeasured,
		},
		{
			name: "partial is trusted and marked",
			u: measuredUsage(v1alpha1.UsageSourcePartial,
				v1alpha1.ModelUsage{Model: "a", OutputTokens: 4, CostUSD: "0.000001"}),
			want:   v1alpha1.UsageAggregate{Runs: 1, PartialRuns: 1, OutputTokens: 4, CostMicroUSD: 1},
			source: v1alpha1.UsageSourcePartial,
		},
		{
			name: "an unknown model keeps its tokens and marks the run unpriced",
			u: measuredUsage(v1alpha1.UsageSourceMeasured,
				v1alpha1.ModelUsage{Model: "mystery", InputTokens: 30},
				v1alpha1.ModelUsage{Model: "a", InputTokens: 1, CostUSD: "0.1"}),
			want:   v1alpha1.UsageAggregate{Runs: 1, UnpricedRuns: 1, InputTokens: 31, CostMicroUSD: 100_000},
			source: v1alpha1.UsageSourceMeasured,
		},
		{
			name: "a model with no tokens and no price is not unpriced",
			u: measuredUsage(v1alpha1.UsageSourceMeasured,
				v1alpha1.ModelUsage{Model: "idle", EstimatedShortfallTokens: 9}),
			want:   v1alpha1.UsageAggregate{Runs: 1, UnmeteredTokens: 9},
			source: v1alpha1.UsageSourceMeasured,
		},
		{
			name: "a malformed cost counts zero and is reported",
			u: measuredUsage(v1alpha1.UsageSourceMeasured,
				v1alpha1.ModelUsage{Model: "a", InputTokens: 1, CostUSD: "x"}),
			want:    v1alpha1.UsageAggregate{Runs: 1, InputTokens: 1},
			source:  v1alpha1.UsageSourceMeasured,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := UsageDeltaFrom(UsageKindBuild, tt.u, at)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if d.Usage != tt.want {
				t.Errorf("usage = %+v\nwant    %+v", d.Usage, tt.want)
			}
			if d.Source != tt.source || d.Kind != UsageKindBuild || !d.CompletedAt.Equal(at) {
				t.Errorf("delta = %+v", d)
			}
		})
	}
}

func TestUsageScopesAndNames(t *testing.T) {
	if got := UsageScopes("org", ""); len(got) != 1 || got[0].Type != v1alpha1.UsageScopeUser || got[0].User != "org" {
		t.Errorf("no project: %+v", got)
	}
	got := UsageScopes("github-id:7", "web")
	want := []v1alpha1.UsageScope{
		{Type: v1alpha1.UsageScopeUser, User: "github-id:7"},
		{Type: v1alpha1.UsageScopeUserProject, User: "github-id:7", Project: "web"},
		{Type: v1alpha1.UsageScopeProject, Project: "web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scopes = %+v", got)
	}
	seen := map[string]bool{}
	for _, s := range append(want,
		v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUser, User: "org"},
		// Concatenation alone would collide these two.
		v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUserProject, User: "github-id:1", Project: "2x"},
		v1alpha1.UsageScope{Type: v1alpha1.UsageScopeUserProject, User: "github-id:12", Project: "x"},
	) {
		n := UsageObjectName(s)
		if seen[n] || !strings.HasPrefix(n, "usage-"+string(s.Type)+"-") || len(n) > 63 {
			t.Errorf("name %q for %+v (duplicate %v)", n, s, seen[n])
		}
		seen[n] = true
		if UsageObjectName(s) != n {
			t.Errorf("name of %+v is not deterministic", s)
		}
	}
	if UsageUser(nil) != "org" || UsageUser(&v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}) != "org" {
		t.Error("an org attribution is not keyed org")
	}
	if got := UsageUser(&v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "a", ID: 42},
		Reason: v1alpha1.AttributionApprover}); got != "github-id:42" {
		t.Errorf("person key = %q", got)
	}
}

func delta(kind string, at time.Time, cost int64) UsageDelta {
	return UsageDelta{Kind: kind, CompletedAt: at, Source: v1alpha1.UsageSourceMeasured,
		Usage: v1alpha1.UsageAggregate{Runs: 1, InputTokens: 10, CostMicroUSD: cost}}
}

// TestApplyUsageBucketsByUTCCompletion: a run lands in the UTC day and month
// of its completion, not the local one and not the aggregation's.
func TestApplyUsageBucketsByUTCCompletion(t *testing.T) {
	denver := time.FixedZone("MDT", -6*3600)
	var st v1alpha1.UsageRollupStatus
	// 23:30 on the 31st in Denver is 05:30 on the 1st of the next month, UTC.
	at := time.Date(2026, 8, 31, 23, 30, 0, 0, denver)
	if !ApplyUsage(&st, "u:1", delta(UsageKindPlan, at, 5), "octo", true, usageNow) {
		t.Fatal("first application refused")
	}
	if _, ok := st.Days["2026-09-01"]; !ok || len(st.Days) != 1 {
		t.Errorf("days = %v, want only 2026-09-01", slices.Collect(maps.Keys(st.Days)))
	}
	if _, ok := st.Months["2026-09"]; !ok || len(st.Months) != 1 {
		t.Errorf("months = %v, want only 2026-09", slices.Collect(maps.Keys(st.Months)))
	}
	if st.Days["2026-09-01"].Kinds[UsageKindPlan].CostMicroUSD != 5 || st.Login != "octo" {
		t.Errorf("bucket = %+v login %q", st.Days["2026-09-01"], st.Login)
	}
	if st.SchemaVersion != v1alpha1.UsageRollupSchemaVersion || st.FirstProcessed == nil {
		t.Errorf("status header = %+v", st)
	}
	// The ledger refuses the same run again and changes nothing.
	before := st.DeepCopy()
	if ApplyUsage(&st, "u:1", delta(UsageKindPlan, at, 5), "", true, usageNow.Add(time.Hour)) {
		t.Error("second application of u:1 accepted")
	}
	if !reflect.DeepEqual(before, &st) {
		t.Error("a refused application mutated the status")
	}
}

// TestApplyUsageRetention: a run completed before the day window lands in
// its month only; compaction drops days past the window, keeps months, and
// a second compaction changes nothing.
func TestApplyUsageRetention(t *testing.T) {
	var st v1alpha1.UsageRollupStatus
	old := usageNow.AddDate(0, 0, -v1alpha1.UsageRetentionDays)
	ApplyUsage(&st, "u:old", delta(UsageKindBuild, old, 1), "", false, usageNow)
	if len(st.Days) != 0 || st.Months[old.Format("2006-01")].Runs != 1 {
		t.Fatalf("an out-of-window run: days %d, month %+v", len(st.Days), st.Months)
	}
	edge := usageNow.AddDate(0, 0, -(v1alpha1.UsageRetentionDays - 1))
	ApplyUsage(&st, "u:edge", delta(UsageKindBuild, edge, 1), "", false, usageNow)
	if len(st.Days) != 1 {
		t.Fatalf("the window's oldest day was not kept: %d days", len(st.Days))
	}
	// A day later the edge day is out of the window.
	if !CompactUsage(&st, usageNow.Add(24*time.Hour)) {
		t.Error("compaction past the edge changed nothing")
	}
	if len(st.Days) != 0 || st.Months[edge.Format("2006-01")].Runs != 2 {
		t.Errorf("after compaction: %d days, %d months", len(st.Days), len(st.Months))
	}
	if CompactUsage(&st, usageNow.Add(24*time.Hour)) {
		t.Error("a second compaction changed something")
	}
}

// TestCompactUsageBounds: months beyond the cap and clock-skewed future days
// beyond the day cap are dropped oldest first.
func TestCompactUsageBounds(t *testing.T) {
	st := v1alpha1.UsageRollupStatus{
		Days:   map[string]v1alpha1.UsageBucket{},
		Months: map[string]v1alpha1.UsageMonthBucket{},
	}
	for i := range v1alpha1.UsageRetentionDays + 5 {
		// From 4 days in the future backwards: all within the age cutoff
		// except none, so only the count bound applies.
		day := usageNow.AddDate(0, 0, 4-i).Format("2006-01-02")
		st.Days[day] = v1alpha1.UsageBucket{}
	}
	for i := range v1alpha1.UsageRetentionMonths + 3 {
		st.Months[usageNow.AddDate(0, -i, 0).Format("2006-01")] = v1alpha1.UsageMonthBucket{}
	}
	if !CompactUsage(&st, usageNow) {
		t.Fatal("compaction changed nothing")
	}
	if len(st.Days) > v1alpha1.UsageRetentionDays || len(st.Months) != v1alpha1.UsageRetentionMonths {
		t.Errorf("%d days, %d months", len(st.Days), len(st.Months))
	}
	if _, ok := st.Months[usageNow.Format("2006-01")]; !ok {
		t.Error("the newest month was dropped")
	}
	if _, ok := st.Days[usageNow.AddDate(0, 0, 4).Format("2006-01-02")]; !ok {
		t.Error("the newest day was dropped")
	}
}

func TestTopIntents(t *testing.T) {
	var st v1alpha1.UsageRollupStatus
	at := usageNow
	n := 0
	apply := func(intent string, cost int64) {
		n++
		d := delta(UsageKindBuild, at, cost)
		d.Intent, d.Project = intent, "p"
		ApplyUsage(&st, fmt.Sprintf("u:%d", n), d, "", true, usageNow)
	}
	for i := range v1alpha1.MaxTopIntents {
		apply(fmt.Sprintf("i%02d", i), int64(10+i))
	}
	apply("cheap", 1) // does not outrank the last (10)
	apply("big", 100) // replaces the last
	apply("i05", 200) // a listed intent grows
	top := st.Months[at.Format("2006-01")].TopIntents
	if len(top) != v1alpha1.MaxTopIntents {
		t.Fatalf("len = %d", len(top))
	}
	if top[0].Intent != "i05" || top[0].CostMicroUSD != 215 || top[1].Intent != "big" {
		t.Errorf("top = %+v", top[:2])
	}
	for _, e := range top {
		if e.Intent == "cheap" || e.Intent == "i00" {
			t.Errorf("%s listed", e.Intent)
		}
	}
	// A non-user scope keeps no list, and a run of no intent never enters one.
	var other v1alpha1.UsageRollupStatus
	d := delta(UsageKindBuild, at, 3)
	d.Intent = "x"
	ApplyUsage(&other, "u:x", d, "", false, usageNow)
	ApplyUsage(&st, "u:none", delta(UsageKindInvestigation, at, 1000), "", true, usageNow)
	if len(other.Months[at.Format("2006-01")].TopIntents) != 0 {
		t.Error("a scope without top intents listed one")
	}
	if st.Months[at.Format("2006-01")].TopIntents[0].Intent != "i05" {
		t.Error("a run of no intent entered the top intents")
	}
}

// usageRun is one generated run for the properties.
type usageRun struct {
	uid    string
	delta  UsageDelta
	copies int
}

func randUsageRuns(r *rand.Rand) []usageRun {
	n := 1 + r.Intn(60)
	out := make([]usageRun, n)
	kinds := []string{UsageKindPlan, UsageKindBuild, UsageKindRevise, UsageKindInvestigation}
	for i := range out {
		// Completions spread over ~two years before now, so some fall out
		// of the day window and some months repeat.
		at := usageNow.Add(-time.Duration(r.Int63n(int64(800 * 24 * time.Hour))))
		u := v1alpha1.UsageAggregate{Runs: 1}
		source := v1alpha1.UsageSourceMeasured
		switch r.Intn(3) {
		case 0:
			source = v1alpha1.UsageSourceReported
			u.ReportedRuns, u.ReportedTokens, u.ReportedCostMicroUSD = 1, r.Int63n(1e6), r.Int63n(1e6)
		case 1:
			source = v1alpha1.UsageSourcePartial
			u.PartialRuns = 1
			fallthrough
		default:
			u.InputTokens, u.OutputTokens, u.CacheReadTokens = r.Int63n(1e6), r.Int63n(1e5), r.Int63n(1e6)
			u.CacheWriteTokens, u.UnmeteredTokens = r.Int63n(1e4), r.Int63n(1e3)
			u.CostMicroUSD, u.NotionalCostMicroUSD = r.Int63n(5e6), r.Int63n(2)*r.Int63n(5e6)
		}
		d := UsageDelta{Kind: kinds[r.Intn(len(kinds))], CompletedAt: at, Source: source, Usage: u}
		if r.Intn(2) == 0 {
			d.Intent, d.Project = fmt.Sprintf("intent-%d", r.Intn(15)), "p"
		}
		out[i] = usageRun{uid: fmt.Sprintf("u:%d", i), delta: d, copies: 1 + r.Intn(3)}
	}
	return out
}

// Generate makes usageRuns usable as a quick.Check argument.
type usageRuns []usageRun

func (usageRuns) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(usageRuns(randUsageRuns(r)))
}

// replay applies every run copies times, the retries interleaved: each
// copy goes at a random later point, as a crash between the rollup write and
// the run's condition write re-delivers it.
func replay(runs usageRuns, r *rand.Rand) v1alpha1.UsageRollupStatus {
	var seq []usageRun
	for _, run := range runs {
		seq = append(seq, run)
		for range run.copies - 1 {
			at := len(seq) - 1 + r.Intn(3)
			if at > len(seq) {
				at = len(seq)
			}
			seq = slices.Insert(seq, at, run)
		}
	}
	var st v1alpha1.UsageRollupStatus
	for _, run := range seq {
		ApplyUsage(&st, run.uid, run.delta, "", true, usageNow)
	}
	return st
}

// TestApplyUsageExactlyOnceProperty: re-delivering runs (a retry after a
// crash between the rollup write and the run's marker) leaves exactly the
// status applying each run once gives.
func TestApplyUsageExactlyOnceProperty(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	prop := func(runs usageRuns) bool {
		var once v1alpha1.UsageRollupStatus
		for _, run := range runs {
			ApplyUsage(&once, run.uid, run.delta, "", true, usageNow)
		}
		got := replay(runs, r)
		return reflect.DeepEqual(once, got)
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

// TestApplyUsageConservationProperty: the months hold every run exactly
// (none is past the month cap here); the days hold exactly the runs within
// the day window; kinds sum to their bucket; nothing exceeds a schema bound.
func TestApplyUsageConservationProperty(t *testing.T) {
	prop := func(runs usageRuns) bool {
		var st v1alpha1.UsageRollupStatus
		var wantAll, wantDays v1alpha1.UsageAggregate
		cutoff := usageDayCutoff(usageNow)
		for _, run := range runs {
			ApplyUsage(&st, run.uid, run.delta, "", true, usageNow)
			addAggregate(&wantAll, run.delta.Usage)
			if run.delta.CompletedAt.UTC().Format("2006-01-02") >= cutoff {
				addAggregate(&wantDays, run.delta.Usage)
			}
		}
		var gotMonths, gotDays v1alpha1.UsageAggregate
		for _, m := range st.Months {
			addAggregate(&gotMonths, m.UsageAggregate)
			if !kindsSum(m.UsageBucket) || len(m.TopIntents) > v1alpha1.MaxTopIntents {
				return false
			}
		}
		for _, d := range st.Days {
			addAggregate(&gotDays, d.UsageAggregate)
			if !kindsSum(d) {
				return false
			}
		}
		return gotMonths == wantAll && gotDays == wantDays &&
			len(st.Days) <= v1alpha1.UsageRetentionDays && len(st.Months) <= v1alpha1.UsageRetentionMonths &&
			len(st.Recent) == len(runs)
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

func kindsSum(b v1alpha1.UsageBucket) bool {
	var runs, cost int64
	for _, k := range b.Kinds {
		runs += k.Runs
		cost += k.CostMicroUSD + k.NotionalCostMicroUSD
	}
	return runs == b.Runs && cost == b.CostMicroUSD+b.NotionalCostMicroUSD
}

// TestCompactUsageProperty: compaction never touches a month, leaves only
// days within the window, and is idempotent; later applications still
// conserve the months.
func TestCompactUsageProperty(t *testing.T) {
	prop := func(runs usageRuns, laterDays uint16) bool {
		var st v1alpha1.UsageRollupStatus
		for _, run := range runs {
			ApplyUsage(&st, run.uid, run.delta, "", true, usageNow)
		}
		later := usageNow.AddDate(0, 0, int(laterDays%900))
		months := st.DeepCopy().Months
		CompactUsage(&st, later)
		cutoff := usageDayCutoff(later)
		for day := range st.Days {
			if day < cutoff {
				return false
			}
		}
		return reflect.DeepEqual(months, st.Months) && !CompactUsage(&st, later)
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

// TestTopIntentsProperty: the list is bounded, sorted and unique; it never
// overstates an intent's month; and with no more distinct intents than its
// bound it states every intent's month exactly.
func TestTopIntentsProperty(t *testing.T) {
	prop := func(runs usageRuns) bool {
		var st v1alpha1.UsageRollupStatus
		for _, run := range runs {
			ApplyUsage(&st, run.uid, run.delta, "", true, usageNow)
		}
		truth := intentTruth(runs)
		for month, m := range st.Months {
			if !topIntentsHold(m.TopIntents, truth[month]) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

// intentTruth is every intent's exact usage per month.
func intentTruth(runs usageRuns) map[string]map[string]v1alpha1.UsageTopIntent {
	truth := map[string]map[string]v1alpha1.UsageTopIntent{}
	for _, run := range runs {
		if run.delta.Intent == "" {
			continue
		}
		month := run.delta.CompletedAt.UTC().Format("2006-01")
		if truth[month] == nil {
			truth[month] = map[string]v1alpha1.UsageTopIntent{}
		}
		e := truth[month][run.delta.Intent]
		e.Intent = run.delta.Intent
		addTop(&e, run.delta)
		truth[month][run.delta.Intent] = e
	}
	return truth
}

// topIntentsHold checks one month's list against the month's truth.
func topIntentsHold(top []v1alpha1.UsageTopIntent, truth map[string]v1alpha1.UsageTopIntent) bool {
	if len(top) > v1alpha1.MaxTopIntents || !slices.IsSortedFunc(top, compareTopIntent) {
		return false
	}
	exact := len(truth) <= v1alpha1.MaxTopIntents
	if exact && len(top) != len(truth) {
		return false
	}
	seen := map[string]bool{}
	for _, e := range top {
		tr, ok := truth[e.Intent]
		if seen[e.Intent] || !ok || e.Runs > tr.Runs || e.Tokens > tr.Tokens ||
			e.CostMicroUSD > tr.CostMicroUSD || e.NotionalCostMicroUSD > tr.NotionalCostMicroUSD {
			return false
		}
		seen[e.Intent] = true
		if exact && (e.Runs != tr.Runs || e.Tokens != tr.Tokens || e.CostMicroUSD != tr.CostMicroUSD) {
			return false
		}
	}
	return true
}

// TestApplyUsageLedgerCap: the usage ledger is capped at maxLedger entries,
// the CRD's maxItems on status.recent, keeping the newest, so a scope that
// takes every run (the org) never has its status refused by the API server;
// the totals still count every run, and the newest runs are still refused a
// second time.
func TestApplyUsageLedgerCap(t *testing.T) {
	var st v1alpha1.UsageRollupStatus
	d := UsageDelta{Kind: UsageKindInvestigation, CompletedAt: usageNow, Source: v1alpha1.UsageSourceReported,
		Usage: v1alpha1.UsageAggregate{Runs: 1, ReportedRuns: 1}}
	const runs = maxLedger + 10
	for i := range runs {
		if !ApplyUsage(&st, fmt.Sprintf("u:run-%d", i), d, "", true, usageNow) {
			t.Fatalf("run %d refused", i)
		}
	}
	if len(st.Recent) != maxLedger {
		t.Errorf("ledger = %d entries, want capped at %d", len(st.Recent), maxLedger)
	}
	if st.Recent[0] != "u:run-10" || st.Recent[len(st.Recent)-1] != fmt.Sprintf("u:run-%d", runs-1) {
		t.Errorf("ledger keeps %s .. %s, want the newest %d", st.Recent[0], st.Recent[len(st.Recent)-1], maxLedger)
	}
	month := st.Months[usageNow.Format("2006-01")]
	if month.Runs != runs || st.Days[usageNow.Format(time.DateOnly)].Runs != runs {
		t.Errorf("runs = %d month / %d day, want %d", month.Runs, st.Days[usageNow.Format(time.DateOnly)].Runs, runs)
	}
	if ApplyUsage(&st, fmt.Sprintf("u:run-%d", runs-1), d, "", true, usageNow) {
		t.Error("the newest run was applied twice")
	}
}

// TestUsageLedgerCapMatchesCRD: maxLedger is the CRD's maxItems on a
// UsageRollup's status.recent; were it larger, every status write past
// that many runs would be refused.
func TestUsageLedgerCapMatchesCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kustomize", "base", "crds",
		"patchy.bitwisemedia.uk_usagerollups.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	for _, v := range crd.Spec.Versions {
		recent := v.Schema.OpenAPIV3Schema.Properties["status"].Properties["recent"]
		if recent.MaxItems == nil || *recent.MaxItems != maxLedger {
			t.Errorf("%s: status.recent maxItems = %v, want %d", v.Name, recent.MaxItems, maxLedger)
		}
	}
}
