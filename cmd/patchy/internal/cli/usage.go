// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
	"github.com/bitwise-media-group/patchy/internal/usageview"
)

// usageNoun is the `get` noun of the usage report. It is not a kind: it is
// the projection of the UsageRollups that the status page's Usage view
// renders too (internal/usageview), so the two never disagree.
const usageNoun = "usage"

// usageGrant is the access review `get usage` asks first: get on the
// virtual usage/all, the status page's admin usage grant. The CLI reads the
// UsageRollups with the caller's own kubeconfig, so RBAC on usagerollups
// enforces it again; the review is what makes the refusal say why.
const usageGrant = "usage/all"

// usageOnly lists the usage filters set, so they can be refused elsewhere.
func (f *getFlags) usageOnly() []string {
	var used []string
	if f.user != "" {
		used = append(used, "--user")
	}
	if f.project != "" {
		used = append(used, "--project")
	}
	if f.since != "" {
		used = append(used, "--since")
	}
	return used
}

// parseSince reads --since as a window of whole UTC days ending today: a
// count of days ("30d"), a Go duration ("720h", rounded up to whole days)
// or a first day ("2026-09-01"). Empty is the default window; anything past
// the retained day buckets is refused rather than silently cut.
func parseSince(s string, today time.Time) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return usageview.DefaultDays, nil
	}
	var days int
	switch {
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("--since %q: want a number of days (30d), a duration (720h) or a date (2026-09-01)", s)
		}
		days = n
	default:
		if d, err := time.ParseDuration(s); err == nil {
			days = int((d + 24*time.Hour - 1) / (24 * time.Hour))
			break
		}
		first, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return 0, fmt.Errorf("--since %q: want a number of days (30d), a duration (720h) or a date (2026-09-01)", s)
		}
		t := today.UTC()
		todayDate := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		days = int(todayDate.Sub(first).Hours()/24) + 1
	}
	if days < 1 || days > usageview.MaxDays {
		return 0, fmt.Errorf("--since %q: the window must be 1 to %d days (day buckets are kept no longer)", s,
			usageview.MaxDays)
	}
	return days, nil
}

// runGetUsage prints the usage report: everyone's, one person's
// (--user: a login, github-id:<id> or org), one Project's, or one person's
// in one Project, over --since.
func runGetUsage(ctx context.Context, opts *Options, f *getFlags, names []string) error {
	switch {
	case len(names) > 0:
		return errUsage(errors.New("usage takes no names; filter with --user and --project"))
	case len(f.findingOnly()) > 0 || f.finding != "" || f.selector != "":
		return errUsage(errors.New("usage takes only --user, --project and --since"))
	}
	days, err := parseSince(f.since, now())
	if err != nil {
		return errUsage(err)
	}
	p, err := opts.Printer()
	if err != nil {
		return err
	}
	if p.Format() == printer.FormatName {
		return errUsage(errors.New("usage is a report, not objects: -o name does not apply"))
	}
	env, err := opts.Connect()
	if err != nil {
		return err
	}
	if env.Namespace == "" {
		return errUsage(errors.New("usage lives in patchy's release namespace: name it rather than --all-namespaces"))
	}
	allowed, err := opts.access(ctx, env, usageGrant, "get")
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: get usage needs get on %s in %s (the status page's admin usage grant); "+
			"your own usage is on the status page's Usage view", errDenied, usageGrant, env.Scope())
	}

	callCtx, cancel := opts.Timeout(ctx)
	defer cancel()
	var list v1alpha1.UsageRollupList
	if err := env.Client.List(callCtx, &list, listOptions(env, "")...); err != nil {
		return err
	}
	filter := usageview.Filter{Project: strings.TrimSpace(f.project)}
	if f.user != "" {
		key, err := usageview.ResolveUser(list.Items, f.user)
		if err != nil {
			return errUsage(err)
		}
		filter.User = key
	}
	rep := usageview.Build(list.Items, filter, now(), days)
	if p.Format().Structured() {
		return p.Objects([]any{rep}, nil)
	}
	if rep.Totals.Runs == 0 {
		notef(opts.ErrOut, "No usage recorded in %s from %s to %s.\n", env.Scope(), rep.From, rep.To)
		return nil
	}
	return p.Tables(usageTables(rep))
}

// figureColumns are the columns every usage table shares after its first.
// Cost leads: a token total is mostly cache reads, which cost a tenth of
// input, so it says little about spend; the tokens follow split by type.
var figureColumns = []string{"RUNS", "COST", "EQUIVALENT", "INPUT", "OUTPUT", "CACHE READ", "CACHE WRITE",
	"TOKENS", "UNMETERED", "REPORTED"}

// figureCells renders a figures row: list-rate cost, a subscription's
// equivalent API cost, the metered tokens by type and in total, the
// broker's worst-case unmetered tokens and the reported runs, which no
// total includes.
func figureCells(f usageview.UsageFigures) []any {
	return []any{
		strconv.FormatInt(f.Runs, 10), microUSD(f.CostMicroUSD), microUSD(f.NotionalCostMicroUSD),
		strconv.FormatInt(f.InputTokens, 10), strconv.FormatInt(f.OutputTokens, 10),
		strconv.FormatInt(f.CacheReadTokens, 10), strconv.FormatInt(f.CacheWriteTokens, 10),
		strconv.FormatInt(f.Tokens, 10), strconv.FormatInt(f.UnmeteredTokens, 10),
		strconv.FormatInt(f.ReportedRuns, 10),
	}
}

func microUSD(m int64) string {
	return fmt.Sprintf("$%d.%02d", m/1_000_000, (m%1_000_000)/10_000)
}

func usageTable(first string, cols []string, rows [][]any) *metav1.Table {
	t := &metav1.Table{}
	for _, c := range append([]string{first}, cols...) {
		t.ColumnDefinitions = append(t.ColumnDefinitions, metav1.TableColumnDefinition{Name: c, Type: "string"})
	}
	for _, r := range rows {
		t.Rows = append(t.Rows, metav1.TableRow{Cells: r, Object: runtime.RawExtension{}})
	}
	return t
}

// usageTables lays the report out: its totals, then per person and per
// Project where the view has them, the days with runs where it is one
// person's or one Project's, and the top intents.
func usageTables(rep usageview.UsageReport) []printer.Group {
	title := "Usage"
	switch {
	case rep.User != "" && rep.Project != "":
		title += " of " + person(rep.User, rep.Login) + " in " + rep.Project
	case rep.User != "":
		title += " of " + person(rep.User, rep.Login)
	case rep.Project != "":
		title += " in " + rep.Project
	}
	title += fmt.Sprintf(", %s to %s (UTC)", rep.From, rep.To)
	groups := []printer.Group{{Title: title, Table: usageTable("WINDOW", figureColumns,
		[][]any{append([]any{fmt.Sprintf("%dd", rep.Days)}, figureCells(rep.Totals)...)})}}

	if len(rep.People) > 0 {
		rows := make([][]any, 0, len(rep.People))
		for _, p := range rep.People {
			rows = append(rows, append([]any{person(p.User, p.Login)}, figureCells(p.UsageFigures)...))
		}
		groups = append(groups, printer.Group{Title: "People", Table: usageTable("USER", figureColumns, rows)})
	}
	if len(rep.Projects) > 0 {
		rows := make([][]any, 0, len(rep.Projects))
		for _, p := range rep.Projects {
			rows = append(rows, append([]any{p.Project}, figureCells(p.UsageFigures)...))
		}
		groups = append(groups, printer.Group{Title: "Projects", Table: usageTable("PROJECT", figureColumns, rows)})
	}
	if rep.User != "" || rep.Project != "" {
		var rows [][]any
		for i := len(rep.Daily) - 1; i >= 0; i-- {
			if d := rep.Daily[i]; d.Runs > 0 {
				rows = append(rows, append([]any{d.Day}, figureCells(d.UsageFigures)...))
			}
		}
		groups = append(groups, printer.Group{Title: "Days", Table: usageTable("DAY", figureColumns, rows)})
	}
	if len(rep.TopIntents) > 0 {
		// A month bucket's top intents record one token total, not its
		// split by type, so TOKENS is all the table can say.
		rows := make([][]any, 0, len(rep.TopIntents))
		for _, ti := range rep.TopIntents {
			rows = append(rows, []any{ti.Intent, ti.Project, strconv.FormatInt(ti.Runs, 10),
				microUSD(ti.CostMicroUSD), microUSD(ti.NotionalCostMicroUSD), strconv.FormatInt(ti.Tokens, 10)})
		}
		groups = append(groups, printer.Group{
			Title: "Top intents (" + strings.Join(rep.TopIntentMonths, ", ") + ")",
			Table: usageTable("INTENT", []string{"PROJECT", "RUNS", "COST", "EQUIVALENT", "TOKENS"}, rows),
		})
	}
	return groups
}

// person names a user key for a table: the login when known.
func person(user, login string) string {
	if login != "" {
		return "@" + login + " (" + user + ")"
	}
	return user
}
