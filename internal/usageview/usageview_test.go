// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usageview

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/stats"
)

var testNow = time.Date(2026, 10, 16, 15, 4, 5, 0, time.UTC)

// run is one settled run as the generator makes it.
type run struct {
	user, login, project, intent, kind string
	at                                 time.Time
	delta                              stats.UsageDelta
}

// fold applies runs exactly as the aggregator does: each to the scopes
// stats.UsageScopes names, through stats.ApplyUsage, and returns the
// resulting rollups.
func fold(t testing.TB, runs []run) []v1alpha1.UsageRollup {
	t.Helper()
	byName := map[string]*v1alpha1.UsageRollup{}
	var order []string
	for i, r := range runs {
		d := r.delta
		d.Kind, d.CompletedAt, d.Intent, d.Project = r.kind, r.at, r.intent, r.project
		for _, scope := range stats.UsageScopes(r.user, r.project) {
			name := stats.UsageObjectName(scope)
			ur, ok := byName[name]
			if !ok {
				ur = &v1alpha1.UsageRollup{Spec: v1alpha1.UsageRollupSpec{Scope: scope}}
				ur.Name = name
				byName[name] = ur
				order = append(order, name)
			}
			login := r.login
			if scope.Type == v1alpha1.UsageScopeProject {
				login = ""
			}
			stats.ApplyUsage(&ur.Status, "u:"+strconv.Itoa(i), d, login, scope.Type == v1alpha1.UsageScopeUser, testNow)
		}
	}
	out := make([]v1alpha1.UsageRollup, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

func measured(in, out, cr, cw, unmetered, cost, notional int64) stats.UsageDelta {
	return stats.UsageDelta{Source: v1alpha1.UsageSourceMeasured, Usage: v1alpha1.UsageAggregate{
		Runs: 1, InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw,
		UnmeteredTokens: unmetered, CostMicroUSD: cost, NotionalCostMicroUSD: notional,
	}}
}

func reported(tokens, cost int64) stats.UsageDelta {
	return stats.UsageDelta{Source: v1alpha1.UsageSourceReported, Usage: v1alpha1.UsageAggregate{
		Runs: 1, ReportedRuns: 1, ReportedTokens: tokens, ReportedCostMicroUSD: cost,
	}}
}

func daysAgo(n int) time.Time { return testNow.AddDate(0, 0, -n) }

func TestWindow(t *testing.T) {
	w := Window(testNow, 3)
	if want := []string{"2026-10-14", "2026-10-15", "2026-10-16"}; !slices.Equal(w, want) {
		t.Errorf("Window = %v, want %v", w, want)
	}
	if got := len(Window(testNow, 0)); got != DefaultDays {
		t.Errorf("Window(0) has %d days, want the default %d", got, DefaultDays)
	}
	if got := len(Window(testNow, 9999)); got != MaxDays {
		t.Errorf("Window(9999) has %d days, want at most %d", got, MaxDays)
	}
}

// A person's own report: totals and days from their user scope, stacked by
// their Projects with the no-project remainder, by kind, with their top
// intents; reported and unmetered figures kept apart from the trusted ones.
func TestBuildSelf(t *testing.T) {
	const me, other = "github-id:42", "github-id:7"
	rollups := fold(t, []run{
		{user: me, login: "octocat", project: "shop", intent: "add-cart", kind: "build", at: daysAgo(0),
			delta: measured(100, 50, 10, 5, 3, 2_000_000, 0)},
		{user: me, login: "octocat", project: "shop", intent: "add-cart", kind: "plan", at: daysAgo(0),
			delta: measured(20, 10, 0, 0, 0, 0, 500_000)},
		{user: me, login: "octocat", kind: "remediation", at: daysAgo(0), delta: measured(40, 0, 0, 0, 0, 100, 0)},
		{user: me, login: "octocat", project: "blog", intent: "dark-mode", kind: "build", at: daysAgo(1),
			delta: reported(999, 7_000_000)},
		{user: me, login: "octocat", project: "blog", intent: "old", kind: "build", at: daysAgo(45),
			delta: measured(1, 1, 0, 0, 0, 1, 0)},
		{user: other, login: "hubot", project: "shop", intent: "theirs", kind: "build", at: daysAgo(0),
			delta: measured(1_000, 0, 0, 0, 0, 9_000_000, 0)},
	})
	rep := Build(rollups, Filter{User: me, Self: true}, testNow, 30)

	if rep.View != ViewSelf || rep.User != me || rep.Login != "octocat" || rep.Days != 30 ||
		rep.From != "2026-09-17" || rep.To != "2026-10-16" || len(rep.Daily) != 30 {
		t.Fatalf("header = %s %s %s %d %s..%s (%d days)", rep.View, rep.User, rep.Login, rep.Days, rep.From, rep.To,
			len(rep.Daily))
	}
	want := UsageFigures{
		Runs: 4, ReportedRuns: 1, Tokens: 235, InputTokens: 160, OutputTokens: 60, CacheReadTokens: 10,
		CacheWriteTokens: 5, UnmeteredTokens: 3, CostMicroUSD: 2_000_100, NotionalCostMicroUSD: 500_000,
		ReportedTokens: 999, ReportedCostMicroUSD: 7_000_000,
	}
	if rep.Totals != want {
		t.Errorf("Totals = %+v\nwant     %+v", rep.Totals, want)
	}
	today := rep.Daily[29]
	if today.Day != "2026-10-16" || today.Tokens != 235 {
		t.Errorf("today = %s %d tokens", today.Day, today.Tokens)
	}
	wantProjects := []UsageSegment{
		{Key: "shop", Runs: 2, Tokens: 195, CostMicroUSD: 2_000_000, NotionalCostMicroUSD: 500_000},
		{Key: NoProject, Runs: 1, Tokens: 40, CostMicroUSD: 100},
	}
	if !reflect.DeepEqual(today.Projects, wantProjects) {
		t.Errorf("today's projects = %+v, want %+v", today.Projects, wantProjects)
	}
	if !slices.Equal(rep.ProjectKeys, []string{"shop", "blog", NoProject}) {
		t.Errorf("ProjectKeys = %v", rep.ProjectKeys)
	}
	if !slices.Equal(rep.KindKeys, []string{"build", "plan", "remediation"}) {
		t.Errorf("KindKeys = %v", rep.KindKeys)
	}
	if got := rep.TopIntents; len(got) != 3 || got[0].Intent != "add-cart" || got[0].Runs != 2 ||
		got[0].CostMicroUSD != 2_000_000 || got[0].NotionalCostMicroUSD != 500_000 {
		t.Errorf("TopIntents = %+v", got)
	}
	for _, ti := range rep.TopIntents {
		if ti.Intent == "theirs" {
			t.Error("another person's intent is in my top intents")
		}
	}
	if !slices.Equal(rep.TopIntentMonths, []string{"2026-09", "2026-10"}) {
		t.Errorf("TopIntentMonths = %v", rep.TopIntentMonths)
	}
	if len(rep.People) != 0 {
		t.Errorf("a person's report lists people: %+v", rep.People)
	}
	if len(rep.Projects) != 2 || rep.Projects[0].Project != "shop" || rep.Projects[1].Project != "blog" {
		t.Errorf("Projects = %+v", rep.Projects)
	}
}

// Everyone's report sums every user scope, splits by the project scopes
// and lists every person (the org included) and every Project.
func TestBuildAll(t *testing.T) {
	rollups := fold(t, []run{
		{user: "github-id:42", login: "octocat", project: "shop", intent: "a", kind: "build", at: daysAgo(2),
			delta: measured(10, 0, 0, 0, 0, 10, 0)},
		{user: "github-id:7", login: "hubot", project: "shop", intent: "b", kind: "build", at: daysAgo(2),
			delta: measured(30, 0, 0, 0, 0, 30, 0)},
		{user: UserOrg, kind: "investigation", at: daysAgo(2), delta: measured(5, 0, 0, 0, 0, 5, 0)},
	})
	rep := Build(rollups, Filter{}, testNow, 7)
	if rep.View != ViewAll || rep.Totals.Tokens != 45 || rep.Totals.Runs != 3 {
		t.Fatalf("all = %s %+v", rep.View, rep.Totals)
	}
	var users []string
	for _, p := range rep.People {
		users = append(users, p.User+"="+p.Login)
	}
	if want := []string{"github-id:7=hubot", "github-id:42=octocat", "org="}; !slices.Equal(users, want) {
		t.Errorf("People = %v, want %v", users, want)
	}
	if len(rep.Projects) != 1 || rep.Projects[0].Tokens != 40 {
		t.Errorf("Projects = %+v", rep.Projects)
	}
	day := rep.Daily[4]
	if want := []UsageSegment{
		{Key: "shop", Runs: 2, Tokens: 40, CostMicroUSD: 40}, {Key: NoProject, Runs: 1, Tokens: 5, CostMicroUSD: 5},
	}; !reflect.DeepEqual(day.Projects, want) {
		t.Errorf("projects on %s = %+v", day.Day, day.Projects)
	}

	shop := Build(rollups, Filter{Project: "shop"}, testNow, 7)
	if shop.View != ViewProject || shop.Totals.Tokens != 40 || len(shop.People) != 2 || shop.Projects != nil {
		t.Errorf("project view = %s %+v people %+v projects %+v", shop.View, shop.Totals, shop.People, shop.Projects)
	}
	mine := Build(rollups, Filter{User: "github-id:42", Project: "shop"}, testNow, 7)
	if mine.View != ViewUserProject || mine.Totals.Tokens != 10 || len(mine.TopIntents) != 1 {
		t.Errorf("user-project view = %s %+v %+v", mine.View, mine.Totals, mine.TopIntents)
	}
}

func TestBuildEmpty(t *testing.T) {
	rep := Build(nil, Filter{User: "github-id:1", Self: true}, testNow, 90)
	if len(rep.Daily) != 90 || rep.Totals != (UsageFigures{}) {
		t.Errorf("empty report = %d days, %+v", len(rep.Daily), rep.Totals)
	}
	for _, d := range rep.Daily {
		if d.Projects == nil || d.Kinds == nil {
			t.Fatalf("day %s has nil segments; the JSON must say []", d.Day)
		}
	}
	if rep.TopIntents == nil || rep.ProjectKeys == nil || rep.KindKeys == nil || rep.Projects == nil {
		t.Error("an empty report has nil lists; the JSON must say []")
	}
}

func TestResolveUser(t *testing.T) {
	rollups := fold(t, []run{
		{user: "github-id:42", login: "octocat", kind: "build", at: daysAgo(0), delta: measured(1, 0, 0, 0, 0, 0, 0)},
		{user: "github-id:43", login: "renamed", kind: "build", at: daysAgo(0), delta: measured(1, 0, 0, 0, 0, 0, 0)},
		{user: "github-id:44", login: "Renamed", kind: "build", at: daysAgo(0), delta: measured(1, 0, 0, 0, 0, 0, 0)},
	})
	cases := []struct{ q, want, wantErr string }{
		{"octocat", "github-id:42", ""},
		{"@OctoCat", "github-id:42", ""},
		{"github-id:99", "github-id:99", ""},
		{"org", "org", ""},
		{"nobody", "", "no usage recorded"},
		{"renamed", "", "github-id:43 and github-id:44"},
		{"github-id:042", "", "no usage recorded"},
	}
	for _, tc := range cases {
		got, err := ResolveUser(rollups, tc.q)
		switch {
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("ResolveUser(%q) = %q, %v; want an error containing %q", tc.q, got, err, tc.wantErr)
		case tc.wantErr == "" && (err != nil || got != tc.want):
			t.Errorf("ResolveUser(%q) = %q, %v; want %q", tc.q, got, err, tc.want)
		}
	}
	if _, err := ResolveUser(rollups, "nobody"); !errors.Is(err, ErrUnknownUser) {
		t.Errorf("an unknown login is not ErrUnknownUser: %v", err)
	}
}

// genRuns makes a random estate: a few people and the org, a few Projects
// and none, every kind, measured, partial-free and reported runs over the
// last 120 days.
func genRuns(r *rand.Rand) []run {
	users := []string{UserOrg, "github-id:1", "github-id:2", "github-id:3"}
	projects := []string{"", "", "alpha", "beta"}
	kinds := []string{"investigation", "remediation", "plan", "build", "revise", "check-fix", "evaluation"}
	n := r.Intn(60)
	out := make([]run, n)
	for i := range out {
		u := users[r.Intn(len(users))]
		p := projects[r.Intn(len(projects))]
		intent := ""
		if p != "" {
			intent = fmt.Sprintf("%s-i%d", p, r.Intn(14))
		}
		d := measured(r.Int63n(1e6), r.Int63n(1e5), r.Int63n(1e6), r.Int63n(1e4), r.Int63n(100), r.Int63n(5e6),
			r.Int63n(5e6))
		if r.Intn(5) == 0 {
			d = reported(r.Int63n(1e6), r.Int63n(1e6))
		}
		login := ""
		if u != UserOrg {
			login = "login" + strings.TrimPrefix(u, "github-id:")
		}
		out[i] = run{user: u, login: login, project: p, intent: intent, kind: kinds[r.Intn(len(kinds))],
			at: daysAgo(r.Intn(120)).Add(-time.Duration(r.Intn(3600)) * time.Second), delta: d}
	}
	return out
}

func segSum(segs []UsageSegment) UsageSegment {
	var s UsageSegment
	for _, x := range segs {
		s = sumSegment(s, x, "")
	}
	return s
}

// The report conserves every run: the window's totals are exactly the
// generated runs completed inside it; every day's Project segments (with
// the remainder) and kind segments sum to the day; the people sum to the
// whole; each person's own report agrees with their row; and every report
// lists each day of the window once, in order, ending today.
func TestBuildConservationProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 300,
		Rand:     rand.New(rand.NewSource(20261016)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			args[0] = reflect.ValueOf(genRuns(r))
			args[1] = reflect.ValueOf(1 + r.Intn(130))
		},
	}
	prop := func(runs []run, days int) bool {
		rollups := fold(t, runs)
		keys := Window(testNow, days)
		var want UsageFigures
		for _, r := range runs {
			if day := r.at.UTC().Format(dayLayout); day >= keys[0] && day <= keys[len(keys)-1] {
				addFigures(&want, r.delta.Usage)
			}
		}
		all := Build(rollups, Filter{}, testNow, days)
		if all.Totals != want {
			t.Logf("totals %+v, want %+v", all.Totals, want)
			return false
		}
		if !checkDays(t, all, keys) {
			return false
		}
		var people UsageFigures
		for _, p := range all.People {
			addTotals(&people, p.UsageFigures)
			own := Build(rollups, Filter{User: p.User, Self: true}, testNow, days)
			if own.Totals != p.UsageFigures || !checkDays(t, own, keys) {
				t.Logf("%s: own totals %+v, row %+v", p.User, own.Totals, p.UsageFigures)
				return false
			}
		}
		if people != all.Totals {
			t.Logf("people sum %+v, totals %+v", people, all.Totals)
			return false
		}
		for _, project := range []string{"alpha", "beta"} {
			pr := Build(rollups, Filter{Project: project}, testNow, days)
			if !checkDays(t, pr, keys) {
				return false
			}
			for _, ti := range pr.TopIntents {
				if ti.Project != project {
					return false
				}
			}
		}
		return len(all.TopIntents) <= v1alpha1.MaxTopIntents
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// checkDays holds a report's days to the window and its segments to each
// day's figures.
func checkDays(t *testing.T, rep UsageReport, keys []string) bool {
	t.Helper()
	if len(rep.Daily) != len(keys) || rep.From != keys[0] || rep.To != keys[len(keys)-1] {
		t.Logf("%s: %d days %s..%s, want %d", rep.View, len(rep.Daily), rep.From, rep.To, len(keys))
		return false
	}
	for i, d := range rep.Daily {
		if d.Day != keys[i] {
			return false
		}
		total := dayTotalSegment(d.UsageFigures)
		if p := segSum(d.Projects); p != total {
			t.Logf("%s %s: projects %+v, day %+v", rep.View, d.Day, p, total)
			return false
		}
		if k := segSum(d.Kinds); k != total {
			t.Logf("%s %s: kinds %+v, day %+v", rep.View, d.Day, k, total)
			return false
		}
		for _, s := range d.Projects {
			if !slices.Contains(rep.ProjectKeys, s.Key) {
				return false
			}
		}
		for _, s := range d.Kinds {
			if !slices.Contains(rep.KindKeys, s.Key) {
				return false
			}
		}
	}
	return true
}

func TestImportsOnlyStdlibAndAPI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && path != "github.com/bitwise-media-group/patchy/api/v1alpha1" {
				t.Errorf("%s imports %s: package usageview imports only the stdlib and api/v1alpha1", name, path)
			}
		}
	}
}
