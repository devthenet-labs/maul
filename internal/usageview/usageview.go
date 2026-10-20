// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usageview

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// Window bounds, in days.
const (
	// DefaultDays is the window when none is asked for.
	DefaultDays = 30
	// MaxDays is the longest window: day buckets are kept no longer.
	MaxDays = v1alpha1.UsageRetentionDays
)

// NoProject is the segment key of usage that belongs to no Project (Finding
// investigations and remediations, evaluation units). A Project name is a
// Kubernetes object name, so it can never collide.
const NoProject = "(no project)"

// UserOrg is the user key of runs no person is behind.
const UserOrg = "org"

// Views a report can be.
const (
	ViewSelf        = "self"
	ViewUser        = "user"
	ViewProject     = "project"
	ViewUserProject = "user-project"
	ViewAll         = "all"
)

// dayLayout is a day bucket's key layout (UTC), stats' own.
const dayLayout = "2006-01-02"

// UsageFigures are a window's or a day's totals.
type UsageFigures struct {
	// Runs counted, whatever their source.
	Runs int64 `json:"runs"`
	// PartialRuns are metered runs the broker saw only part of.
	PartialRuns int64 `json:"partialRuns"`
	// ReportedRuns have only their pod's own figure (untrusted).
	ReportedRuns int64 `json:"reportedRuns"`
	// UnpricedRuns are metered runs of a model with no known price.
	UnpricedRuns int64 `json:"unpricedRuns"`
	// Tokens metered, every class.
	Tokens int64 `json:"tokens"`
	// InputTokens metered.
	InputTokens int64 `json:"inputTokens"`
	// OutputTokens metered.
	OutputTokens int64 `json:"outputTokens"`
	// CacheReadTokens metered.
	CacheReadTokens int64 `json:"cacheReadTokens"`
	// CacheWriteTokens metered.
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	// UnmeteredTokens is the broker's worst-case shortfall: never priced.
	UnmeteredTokens int64 `json:"unmeteredTokens"`
	// CostMicroUSD is the metered tokens' list-rate cost.
	CostMicroUSD int64 `json:"costMicroUSD"`
	// NotionalCostMicroUSD is the list-rate equivalent of what a Claude
	// subscription paid for: never money spent.
	NotionalCostMicroUSD int64 `json:"notionalCostMicroUSD"`
	// ReportedTokens are the reported runs' pod-reported tokens (untrusted).
	ReportedTokens int64 `json:"reportedTokens"`
	// ReportedCostMicroUSD is their pod-reported cost (untrusted).
	ReportedCostMicroUSD int64 `json:"reportedCostMicroUSD"`
}

// UsageSegment is one stack segment of a day: one Project's or one run
// kind's share.
type UsageSegment struct {
	// Key is the Project (or NoProject) or the run kind.
	Key string `json:"key"`
	// Runs counted.
	Runs int64 `json:"runs"`
	// Tokens metered.
	Tokens int64 `json:"tokens"`
	// CostMicroUSD at list rates.
	CostMicroUSD int64 `json:"costMicroUSD"`
	// NotionalCostMicroUSD for a subscription.
	NotionalCostMicroUSD int64 `json:"notionalCostMicroUSD"`
}

// UsageDay is one day of the window.
type UsageDay struct {
	// Day is the UTC day, 2026-10-16.
	Day string `json:"day"`
	UsageFigures
	// Projects split the day by Project, in the report's ProjectKeys order;
	// zero segments are left out.
	Projects []UsageSegment `json:"projects"`
	// Kinds split the day by run kind, in the report's KindKeys order; zero
	// segments are left out.
	Kinds []UsageSegment `json:"kinds"`
}

// UsageIntent is one of the top intents.
type UsageIntent struct {
	// Intent is the Intent's name.
	Intent string `json:"intent"`
	// Project it belonged to.
	Project string `json:"project"`
	// Runs counted.
	Runs int64 `json:"runs"`
	// Tokens metered.
	Tokens int64 `json:"tokens"`
	// CostMicroUSD at list rates.
	CostMicroUSD int64 `json:"costMicroUSD"`
	// NotionalCostMicroUSD for a subscription.
	NotionalCostMicroUSD int64 `json:"notionalCostMicroUSD"`
}

// UsagePerson is one person's (or the org's) window totals.
type UsagePerson struct {
	// User is "org" or "github-id:<numeric id>".
	User string `json:"user"`
	// Login is the GitHub login last seen, for display only.
	Login string `json:"login"`
	UsageFigures
}

// UsageProject is one Project's window totals.
type UsageProject struct {
	// Project is the Project's name.
	Project string `json:"project"`
	UsageFigures
}

// UsageReport is the whole projection.
type UsageReport struct {
	// View is self, user, project, user-project or all.
	View string `json:"view"`
	// User is the person the report is for ("" for project and all).
	User string `json:"user"`
	// Login is that person's last-seen login, display only.
	Login string `json:"login"`
	// Project is the Project the report is for ("" unless filtered).
	Project string `json:"project"`
	// Days is the window's length; From and To its first and last day.
	Days int    `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
	// Totals over the window.
	Totals UsageFigures `json:"totals"`
	// Daily is every day of the window, oldest first.
	Daily []UsageDay `json:"daily"`
	// ProjectKeys and KindKeys order the stack segments (largest first,
	// NoProject last), so a chart colours each key the same on every day.
	ProjectKeys []string `json:"projectKeys"`
	KindKeys    []string `json:"kindKeys"`
	// TopIntents are the most expensive intents of the months the window
	// touches (month buckets keep them; a day does not).
	TopIntents []UsageIntent `json:"topIntents"`
	// TopIntentMonths are those months, oldest first.
	TopIntentMonths []string `json:"topIntentMonths"`
	// People are every person's totals (views all and project; omitted
	// otherwise).
	People []UsagePerson `json:"people,omitempty"`
	// Projects are every Project's totals (views all, self and user).
	Projects []UsageProject `json:"projects,omitempty"`
}

// Filter selects a report's view.
type Filter struct {
	// User is the user key ("org" or "github-id:<id>"); "" for everyone.
	User string
	// Project is a Project name; "" for every Project.
	Project string
	// Self marks a person's own report (view self rather than user).
	Self bool
}

// ClampDays bounds a requested window: below 1 is the default, above
// MaxDays is MaxDays.
func ClampDays(days int) int {
	switch {
	case days < 1:
		return DefaultDays
	case days > MaxDays:
		return MaxDays
	}
	return days
}

// Window is the day keys of a days-long window ending on now's UTC day,
// oldest first.
func Window(now time.Time, days int) []string {
	days = ClampDays(days)
	today := now.UTC()
	out := make([]string, days)
	for i := range days {
		out[i] = today.AddDate(0, 0, i-days+1).Format(dayLayout)
	}
	return out
}

// userKeyRE is the user key grammar, the attributed-to annotation's.
var userKeyRE = regexp.MustCompile(`^(org|github-id:[1-9][0-9]{0,18})$`)

// UserKey is a person's user key from their GitHub id.
func UserKey(githubID int64) string {
	return fmt.Sprintf("github-id:%d", githubID)
}

// ErrUnknownUser is ResolveUser's refusal.
var ErrUnknownUser = errors.New("no usage recorded for that user")

// ResolveUser reads a --user argument: a user key ("org",
// "github-id:<id>") as is, else a GitHub login matched, case-insensitively,
// against the logins the user scopes last saw. A login two ids have held
// is refused as ambiguous, since a login alone never says who.
func ResolveUser(rollups []v1alpha1.UsageRollup, q string) (string, error) {
	q = strings.TrimSpace(q)
	if userKeyRE.MatchString(q) {
		return q, nil
	}
	q = strings.TrimPrefix(q, "@")
	var found []string
	for i := range rollups {
		r := &rollups[i]
		if r.Spec.Scope.Type == v1alpha1.UsageScopeUser && strings.EqualFold(r.Status.Login, q) {
			found = append(found, r.Spec.Scope.User)
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: %q (want a login, github-id:<id> or org)", ErrUnknownUser, q)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("login %q has been held by %s; name one as github-id:<id>", q,
		strings.Join(found, " and "))
}

// Build projects rollups into the report f selects, over the days-long
// window ending on now's UTC day.
func Build(rollups []v1alpha1.UsageRollup, f Filter, now time.Time, days int) UsageReport {
	days = ClampDays(days)
	keys := Window(now, days)
	rep := UsageReport{
		View: view(f), User: f.User, Project: f.Project, Days: days, From: keys[0], To: keys[len(keys)-1],
		Daily: make([]UsageDay, len(keys)), ProjectKeys: []string{}, KindKeys: []string{},
		TopIntents: []UsageIntent{}, TopIntentMonths: []string{},
	}
	sel := selectScopes(rollups, f)
	rep.Login = sel.login

	dayKinds, kindTotals := fillDays(&rep, keys, sel.primary)
	dayProjects, projectTotals := splitProjects(&rep, keys, sel.stack)
	rep.ProjectKeys = orderKeys(projectTotals, NoProject)
	rep.KindKeys = orderKeys(kindTotals, "")
	for i := range rep.Daily {
		rep.Daily[i].Projects = segments(dayProjects[i], rep.ProjectKeys)
		rep.Daily[i].Kinds = segments(dayKinds[i], rep.KindKeys)
	}

	rep.TopIntents, rep.TopIntentMonths = topIntents(sel.tops, f.Project, rep.From, rep.To)
	inWindow := func(day string) bool { return day >= rep.From && day <= rep.To }
	if f.User == "" {
		rep.People = peopleRows(rollups, sel.people, inWindow)
	}
	if f.Project == "" {
		rep.Projects = projectRows(sel.projects, inWindow)
	}
	return rep
}

// selection is which rollups play which part in a report.
type selection struct {
	// primary sum to the report's totals and days.
	primary []*v1alpha1.UsageRollup
	// stack split the days by Project.
	stack []*v1alpha1.UsageRollup
	// people and projects are the per-person and per-Project rows.
	people, projects []*v1alpha1.UsageRollup
	// tops hold the top intents (user scopes).
	tops []*v1alpha1.UsageRollup
	// login is the filtered person's last-seen login.
	login string
}

func selectScopes(rollups []v1alpha1.UsageRollup, f Filter) selection {
	var sel selection
	for i := range rollups {
		r := &rollups[i]
		switch r.Spec.Scope.Type {
		case v1alpha1.UsageScopeUser:
			sel.addUser(r, f)
		case v1alpha1.UsageScopeUserProject:
			sel.addUserProject(r, f)
		case v1alpha1.UsageScopeProject:
			sel.addProject(r, f)
		}
	}
	return sel
}

func (sel *selection) addUser(r *v1alpha1.UsageRollup, f Filter) {
	if f.User != "" && r.Spec.Scope.User != f.User {
		return
	}
	sel.tops = append(sel.tops, r)
	if f.Project == "" {
		if f.User == "" {
			sel.people = append(sel.people, r)
		}
		sel.primary = append(sel.primary, r)
	}
	if f.User != "" && r.Status.Login != "" {
		sel.login = r.Status.Login
	}
}

func (sel *selection) addUserProject(r *v1alpha1.UsageRollup, f Filter) {
	s := r.Spec.Scope
	userOK, projOK := f.User == "" || s.User == f.User, f.Project == "" || s.Project == f.Project
	switch {
	case f.User != "" && f.Project != "" && userOK && projOK:
		sel.primary = append(sel.primary, r)
		sel.stack = append(sel.stack, r)
	case f.User != "" && f.Project == "" && userOK:
		sel.stack = append(sel.stack, r)
		sel.projects = append(sel.projects, r)
	case f.User == "" && f.Project != "" && projOK:
		sel.people = append(sel.people, r)
	}
}

func (sel *selection) addProject(r *v1alpha1.UsageRollup, f Filter) {
	if f.User != "" || (f.Project != "" && r.Spec.Scope.Project != f.Project) {
		return
	}
	sel.stack = append(sel.stack, r)
	sel.projects = append(sel.projects, r)
	if f.Project != "" {
		sel.primary = append(sel.primary, r)
	}
}

// fillDays sums the primary scopes into each day and the totals, and
// returns each day's kind segments and the window's kind totals.
func fillDays(rep *UsageReport, keys []string, primary []*v1alpha1.UsageRollup) ([]map[string]UsageSegment,
	map[string]UsageSegment) {
	kindTotals := map[string]UsageSegment{}
	dayKinds := make([]map[string]UsageSegment, len(keys))
	for i, day := range keys {
		rep.Daily[i].Day = day
		dayKinds[i] = map[string]UsageSegment{}
		for _, r := range primary {
			b, ok := r.Status.Days[day]
			if !ok {
				continue
			}
			addFigures(&rep.Daily[i].UsageFigures, b.UsageAggregate)
			for k, kt := range b.Kinds {
				seg := UsageSegment{Key: k, Runs: kt.Runs, Tokens: kt.Tokens, CostMicroUSD: kt.CostMicroUSD,
					NotionalCostMicroUSD: kt.NotionalCostMicroUSD}
				dayKinds[i][k] = sumSegment(dayKinds[i][k], seg, k)
				kindTotals[k] = sumSegment(kindTotals[k], seg, k)
			}
		}
		addTotals(&rep.Totals, rep.Daily[i].UsageFigures)
	}
	return dayKinds, kindTotals
}

// splitProjects splits each day by the stack scopes' Projects, plus the
// remainder no Project accounts for.
func splitProjects(rep *UsageReport, keys []string, stack []*v1alpha1.UsageRollup) ([]map[string]UsageSegment,
	map[string]UsageSegment) {
	projectTotals := map[string]UsageSegment{}
	dayProjects := make([]map[string]UsageSegment, len(keys))
	for i, day := range keys {
		dayProjects[i] = map[string]UsageSegment{}
		var sum UsageSegment
		for _, r := range stack {
			b, ok := r.Status.Days[day]
			if !ok {
				continue
			}
			p := r.Spec.Scope.Project
			seg := bucketSegment(p, b.UsageAggregate)
			dayProjects[i][p] = sumSegment(dayProjects[i][p], seg, p)
			sum = sumSegment(sum, seg, "")
		}
		if rest := remainder(dayTotalSegment(rep.Daily[i].UsageFigures), sum); !zero(rest) {
			dayProjects[i][NoProject] = rest
		}
		for k, seg := range dayProjects[i] {
			projectTotals[k] = sumSegment(projectTotals[k], seg, k)
		}
	}
	return dayProjects, projectTotals
}

func peopleRows(rollups []v1alpha1.UsageRollup, people []*v1alpha1.UsageRollup, in func(string) bool) []UsagePerson {
	out := make([]UsagePerson, 0, len(people))
	for _, r := range people {
		p := UsagePerson{User: r.Spec.Scope.User, Login: loginOf(rollups, r.Spec.Scope.User)}
		windowFigures(&p.UsageFigures, r, in)
		if p.Runs > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b UsagePerson) int {
		return cmp.Or(compareFigures(a.UsageFigures, b.UsageFigures), cmp.Compare(a.User, b.User))
	})
	return out
}

func projectRows(projects []*v1alpha1.UsageRollup, in func(string) bool) []UsageProject {
	out := make([]UsageProject, 0, len(projects))
	for _, r := range projects {
		p := UsageProject{Project: r.Spec.Scope.Project}
		windowFigures(&p.UsageFigures, r, in)
		if p.Runs > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b UsageProject) int {
		return cmp.Or(compareFigures(a.UsageFigures, b.UsageFigures), cmp.Compare(a.Project, b.Project))
	})
	return out
}

func view(f Filter) string {
	switch {
	case f.User != "" && f.Project != "":
		return ViewUserProject
	case f.User != "" && f.Self:
		return ViewSelf
	case f.User != "":
		return ViewUser
	case f.Project != "":
		return ViewProject
	}
	return ViewAll
}

// loginOf is the login the user's own scope last saw.
func loginOf(rollups []v1alpha1.UsageRollup, user string) string {
	for i := range rollups {
		s := rollups[i].Spec.Scope
		if s.Type == v1alpha1.UsageScopeUser && s.User == user {
			return rollups[i].Status.Login
		}
	}
	return ""
}

// windowFigures sums r's day buckets inside the window into f.
func windowFigures(f *UsageFigures, r *v1alpha1.UsageRollup, in func(string) bool) {
	for day, b := range r.Status.Days {
		if in(day) {
			addFigures(f, b.UsageAggregate)
		}
	}
}

func addFigures(f *UsageFigures, a v1alpha1.UsageAggregate) {
	f.Runs += a.Runs
	f.PartialRuns += a.PartialRuns
	f.ReportedRuns += a.ReportedRuns
	f.UnpricedRuns += a.UnpricedRuns
	f.InputTokens += a.InputTokens
	f.OutputTokens += a.OutputTokens
	f.CacheReadTokens += a.CacheReadTokens
	f.CacheWriteTokens += a.CacheWriteTokens
	f.Tokens += a.InputTokens + a.OutputTokens + a.CacheReadTokens + a.CacheWriteTokens
	f.UnmeteredTokens += a.UnmeteredTokens
	f.CostMicroUSD += a.CostMicroUSD
	f.NotionalCostMicroUSD += a.NotionalCostMicroUSD
	f.ReportedTokens += a.ReportedTokens
	f.ReportedCostMicroUSD += a.ReportedCostMicroUSD
}

func addTotals(t *UsageFigures, d UsageFigures) {
	addFigures(t, v1alpha1.UsageAggregate{
		Runs: d.Runs, PartialRuns: d.PartialRuns, ReportedRuns: d.ReportedRuns, UnpricedRuns: d.UnpricedRuns,
		InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CacheReadTokens: d.CacheReadTokens,
		CacheWriteTokens: d.CacheWriteTokens, UnmeteredTokens: d.UnmeteredTokens, CostMicroUSD: d.CostMicroUSD,
		NotionalCostMicroUSD: d.NotionalCostMicroUSD, ReportedTokens: d.ReportedTokens,
		ReportedCostMicroUSD: d.ReportedCostMicroUSD,
	})
}

func bucketSegment(key string, a v1alpha1.UsageAggregate) UsageSegment {
	return UsageSegment{
		Key: key, Runs: a.Runs, Tokens: a.InputTokens + a.OutputTokens + a.CacheReadTokens + a.CacheWriteTokens,
		CostMicroUSD: a.CostMicroUSD, NotionalCostMicroUSD: a.NotionalCostMicroUSD,
	}
}

func dayTotalSegment(f UsageFigures) UsageSegment {
	return UsageSegment{Runs: f.Runs, Tokens: f.Tokens, CostMicroUSD: f.CostMicroUSD,
		NotionalCostMicroUSD: f.NotionalCostMicroUSD}
}

func sumSegment(a, b UsageSegment, key string) UsageSegment {
	return UsageSegment{
		Key: key, Runs: a.Runs + b.Runs, Tokens: a.Tokens + b.Tokens, CostMicroUSD: a.CostMicroUSD + b.CostMicroUSD,
		NotionalCostMicroUSD: a.NotionalCostMicroUSD + b.NotionalCostMicroUSD,
	}
}

// remainder is what of total no Project accounts for, each figure clamped at
// zero: the scopes are separate objects written one after another, so a
// Project's may briefly be ahead of the user's own.
func remainder(total, projects UsageSegment) UsageSegment {
	return UsageSegment{
		Key: NoProject, Runs: max(0, total.Runs-projects.Runs), Tokens: max(0, total.Tokens-projects.Tokens),
		CostMicroUSD:         max(0, total.CostMicroUSD-projects.CostMicroUSD),
		NotionalCostMicroUSD: max(0, total.NotionalCostMicroUSD-projects.NotionalCostMicroUSD),
	}
}

func zero(s UsageSegment) bool {
	return s.Runs == 0 && s.Tokens == 0 && s.CostMicroUSD == 0 && s.NotionalCostMicroUSD == 0
}

// orderKeys orders segment keys largest first (cost, then tokens, then
// runs, then name), with last (if present) at the end whatever its size.
func orderKeys(totals map[string]UsageSegment, last string) []string {
	keys := slices.Collect(maps.Keys(totals))
	slices.SortFunc(keys, func(a, b string) int {
		if (a == last) != (b == last) {
			if a == last {
				return 1
			}
			return -1
		}
		x, y := totals[a], totals[b]
		return cmp.Or(
			cmp.Compare(y.CostMicroUSD+y.NotionalCostMicroUSD, x.CostMicroUSD+x.NotionalCostMicroUSD),
			cmp.Compare(y.Tokens, x.Tokens), cmp.Compare(y.Runs, x.Runs), cmp.Compare(a, b))
	})
	if keys == nil {
		return []string{}
	}
	return keys
}

func segments(m map[string]UsageSegment, order []string) []UsageSegment {
	out := []UsageSegment{}
	for _, k := range order {
		if seg, ok := m[k]; ok && !zero(seg) {
			seg.Key = k
			out = append(out, seg)
		}
	}
	return out
}

func compareFigures(a, b UsageFigures) int {
	return cmp.Or(
		cmp.Compare(b.CostMicroUSD+b.NotionalCostMicroUSD, a.CostMicroUSD+a.NotionalCostMicroUSD),
		cmp.Compare(b.Tokens, a.Tokens), cmp.Compare(b.Runs, a.Runs))
}

// topIntents merges the month buckets' top intents of the months [from, to]
// touches, across the given user scopes, keeping project's alone when set;
// at most v1alpha1.MaxTopIntents, highest first.
func topIntents(scopes []*v1alpha1.UsageRollup, project, from, to string) ([]UsageIntent, []string) {
	first, last := from[:7], to[:7]
	merged := map[string]UsageIntent{}
	months := map[string]bool{}
	for _, r := range scopes {
		for m, b := range r.Status.Months {
			if m < first || m > last {
				continue
			}
			months[m] = true
			for _, t := range b.TopIntents {
				if project != "" && t.Project != project {
					continue
				}
				cur := merged[t.Intent]
				cur.Intent = t.Intent
				if cur.Project == "" {
					cur.Project = t.Project
				}
				cur.Runs += t.Runs
				cur.Tokens += t.Tokens
				cur.CostMicroUSD += t.CostMicroUSD
				cur.NotionalCostMicroUSD += t.NotionalCostMicroUSD
				merged[t.Intent] = cur
			}
		}
	}
	out := slices.Collect(maps.Values(merged))
	slices.SortFunc(out, func(a, b UsageIntent) int {
		return cmp.Or(
			cmp.Compare(b.CostMicroUSD+b.NotionalCostMicroUSD, a.CostMicroUSD+a.NotionalCostMicroUSD),
			cmp.Compare(b.Tokens, a.Tokens), cmp.Compare(b.Runs, a.Runs), cmp.Compare(a.Intent, b.Intent))
	})
	if len(out) > v1alpha1.MaxTopIntents {
		out = out[:v1alpha1.MaxTopIntents]
	}
	if out == nil {
		out = []UsageIntent{}
	}
	ms := slices.Sorted(maps.Keys(months))
	if ms == nil {
		ms = []string{}
	}
	return out, ms
}
