// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/internal/stats"
	"github.com/bitwise-media-group/patchy/internal/usageview"
)

// usageRollups folds runs into rollups as the aggregator would, at the
// harness clock.
func usageRollups(t *testing.T) []client.Object {
	t.Helper()
	type run struct {
		user, login, project, intent string
		tokens, cost                 int64
		ago                          int
	}
	byName := map[string]*v1alpha1.UsageRollup{}
	var order []string
	for i, r := range []run{
		{"github-id:42", "octocat", "storefront", "storefront-41", 1000, 3_500_000, 0},
		{"github-id:42", "octocat", "", "", 200, 10_000, 3},
		{"github-id:7", "hubot", "storefront", "storefront-9", 500, 1_000_000, 60},
		{"org", "", "", "", 50, 500, 1},
	} {
		d := stats.UsageDelta{Kind: "build", CompletedAt: testClock.AddDate(0, 0, -r.ago), Intent: r.intent,
			Project: r.project, Source: v1alpha1.UsageSourceMeasured,
			Usage: v1alpha1.UsageAggregate{Runs: 1, InputTokens: r.tokens, CostMicroUSD: r.cost}}
		for _, scope := range stats.UsageScopes(r.user, r.project) {
			name := stats.UsageObjectName(scope)
			ur, ok := byName[name]
			if !ok {
				ur = &v1alpha1.UsageRollup{Spec: v1alpha1.UsageRollupSpec{Scope: scope}}
				ur.Name, ur.Namespace = name, testNamespace
				byName[name] = ur
				order = append(order, name)
			}
			login := r.login
			if scope.Type == v1alpha1.UsageScopeProject {
				login = ""
			}
			stats.ApplyUsage(&ur.Status, "u:"+strconv.Itoa(i), d, login, scope.Type == v1alpha1.UsageScopeUser,
				testClock)
		}
	}
	out := make([]client.Object, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

func TestGetUsageTable(t *testing.T) {
	h := newHarness(t, usageRollups(t)...)
	var asked []string
	h.opts.WithAccess(func(_ context.Context, _ *kubecfg.Env, res, verb string) (bool, error) {
		asked = append(asked, res+" "+verb)
		return true, nil
	})
	if err := h.execRoot(t, "get", "usage"); err != nil {
		t.Fatalf("get usage: %v\n%s", err, h.errOut)
	}
	out := h.out.String()
	for _, want := range []string{
		"Usage, 2026-06-25 to 2026-07-24 (UTC)", "People", "@octocat (github-id:42)", "org", "Projects",
		"storefront", "Top intents (2026-07)", "storefront-41", "$3.51",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// hubot's run is 60 days old: outside the default 30-day window.
	if strings.Contains(out, "hubot") {
		t.Errorf("a run outside the window is listed:\n%s", out)
	}
	if len(asked) != 1 || asked[0] != "usage/all get" {
		t.Errorf("access reviews = %v, want one for get usage/all", asked)
	}
}

func TestGetUsageJSONIsTheReport(t *testing.T) {
	h := newHarness(t, usageRollups(t)...)
	if err := h.execRoot(t, "get", "usage", "--user", "@octocat", "--since", "90d", "-o", "json"); err != nil {
		t.Fatalf("get usage: %v\n%s", err, h.errOut)
	}
	var rep usageview.UsageReport
	if err := json.Unmarshal(h.out.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v\n%s", err, h.out)
	}
	if rep.View != usageview.ViewUser || rep.User != "github-id:42" || rep.Days != 90 ||
		rep.Totals.Tokens != 1200 || rep.Totals.CostMicroUSD != 3_510_000 {
		t.Errorf("report = %s %s %d days %+v", rep.View, rep.User, rep.Days, rep.Totals)
	}

	h = newHarness(t, usageRollups(t)...)
	if err := h.execRoot(t, "get", "usage", "--project", "storefront", "--since", "2026-05-01", "-o", "yaml"); err != nil {
		t.Fatalf("get usage: %v", err)
	}
	if out := h.out.String(); !strings.Contains(out, "view: project") || !strings.Contains(out, "github-id:7") {
		t.Errorf("project report misses hubot's run inside the window:\n%s", out)
	}
}

// Without get on usage/all the CLI says why and exits denied, before
// reading any rollup.
func TestGetUsageDenied(t *testing.T) {
	h := newHarness(t, usageRollups(t)...)
	h.deny()
	err := h.execRoot(t, "get", "usage")
	if err == nil || exitCode(err) != ExitDenied || !strings.Contains(err.Error(), "usage/all") {
		t.Fatalf("err = %v (exit %d), want a denial naming usage/all", err, exitCode(err))
	}
	if h.out.Len() != 0 {
		t.Errorf("a denied caller got output:\n%s", h.out)
	}
}

// The real review asks the API server about the usage/all subresource.
func TestGetUsageAsksForTheSubresource(t *testing.T) {
	g := &grants{allowed: map[string]bool{}}
	h := reviewHarness(t, g, usageRollups(t)...)
	err := h.execRoot(t, "get", "usage")
	if exitCode(err) != ExitDenied {
		t.Fatalf("err = %v, want denied", err)
	}
	if len(g.asked) != 1 || g.asked[0].Resource != "usage" || g.asked[0].Subresource != "all" ||
		g.asked[0].Verb != "get" || g.asked[0].Group != v1alpha1.GroupVersion.Group {
		t.Errorf("reviews = %+v, want get usage/all", g.asked)
	}
}

func TestGetUsageRefusesMisuse(t *testing.T) {
	for _, args := range [][]string{
		{"get", "usage", "some-name"},
		{"get", "usage", "--phase", "Opened"},
		{"get", "usage", "-l", "a=b"},
		{"get", "usage", "--since", "500d"},
		{"get", "usage", "--since", "yesterday"},
		{"get", "usage", "--user", "nobody"},
		{"get", "usage", "-o", "name"},
		{"get", "findings", "--user", "octocat"},
		{"get", "all", "--since", "30d"},
		{"get", "usage", "--all-namespaces"},
	} {
		h := newHarness(t, usageRollups(t)...)
		if strings.Contains(strings.Join(args, " "), "--all-namespaces") {
			h.opts.WithEnv(&kubecfg.Env{Client: h.client})
		}
		if err := h.execRoot(t, args...); exitCode(err) != ExitUsage {
			t.Errorf("%v: err = %v (exit %d), want a usage error", args, err, exitCode(err))
		}
	}
}

func TestParseSince(t *testing.T) {
	today := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want int
		err  bool
	}{
		{"", usageview.DefaultDays, false},
		{"30d", 30, false},
		{"1d", 1, false},
		{"400d", 400, false},
		{"24h", 1, false},
		{"25h", 2, false},
		{"2026-10-16", 1, false},
		{"2026-09-17", 30, false},
		{"0d", 0, true},
		{"401d", 0, true},
		{"2026-10-17", 0, true},
		{"2025-01-01", 0, true},
		{"-5h", 0, true},
		{"soon", 0, true},
	}
	for _, tc := range cases {
		got, err := parseSince(tc.in, today)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("parseSince(%q) = %d, %v; want %d (error %v)", tc.in, got, err, tc.want, tc.err)
		}
	}
}

// Cost leads every usage table and the tokens are split by type: a total
// that is mostly cache reads says little about spend.
func TestUsageTablesLeadWithCostAndSplitTokens(t *testing.T) {
	rep := usageview.UsageReport{From: "2026-07-01", To: "2026-07-30", Days: 30, Totals: usageview.UsageFigures{
		Runs: 1, Tokens: 1_260_000, InputTokens: 60_000, OutputTokens: 30_000, CacheReadTokens: 1_150_000,
		CacheWriteTokens: 20_000, CostMicroUSD: 940_000,
	}}
	groups := usageTables(rep)
	if len(groups) == 0 {
		t.Fatal("no tables")
	}
	tbl := groups[0].Table
	cols := make([]string, 0, len(tbl.ColumnDefinitions))
	for _, c := range tbl.ColumnDefinitions {
		cols = append(cols, c.Name)
	}
	wantCols := []string{"WINDOW", "RUNS", "COST", "EQUIVALENT", "INPUT", "OUTPUT", "CACHE READ", "CACHE WRITE",
		"TOKENS", "UNMETERED", "REPORTED"}
	if strings.Join(cols, ",") != strings.Join(wantCols, ",") {
		t.Fatalf("columns = %v, want %v", cols, wantCols)
	}
	wantCells := []any{"30d", "1", "$0.94", "$0.00", "60000", "30000", "1150000", "20000", "1260000", "0", "0"}
	if got := tbl.Rows[0].Cells; len(got) != len(wantCells) {
		t.Fatalf("cells = %v, want %v", got, wantCells)
	} else {
		for i := range got {
			if got[i] != wantCells[i] {
				t.Errorf("cell %s = %v, want %v", wantCols[i], got[i], wantCells[i])
			}
		}
	}
}
