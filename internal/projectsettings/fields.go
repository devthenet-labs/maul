// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// Group is a lock group: the unit an operator makes read-only
// (statusServer.projectSettings.locked). Every editable field is in one.
type Group string

// The lock groups.
const (
	GroupApprovers     Group = "approvers"
	GroupEvaluator     Group = "evaluator"
	GroupHandoff       Group = "handoff"
	GroupModels        Group = "models"
	GroupCost          Group = "cost"
	GroupRounds        Group = "rounds"
	GroupActiveIntents Group = "activeIntents"
	GroupStages        Group = "stages"
	GroupChecks        Group = "checks"
	GroupResources     Group = "resources"
)

// Groups are the lock groups, in the page's order.
func Groups() []Group {
	return []Group{GroupApprovers, GroupEvaluator, GroupHandoff, GroupModels, GroupCost, GroupRounds,
		GroupActiveIntents, GroupStages, GroupChecks, GroupResources}
}

// Kind is how a field's value is written.
type Kind string

// The kinds. Count, MicroUSD and Seconds are numbers; the rest are not.
const (
	KindCount    Kind = "count"
	KindMicroUSD Kind = "microUSD"
	KindSeconds  Kind = "seconds"
	KindBool     Kind = "bool"
	KindHandoff  Kind = "handoff"
	KindModel    Kind = "model"
	KindLogins   Kind = "logins"
	KindChecks   Kind = "checks"
	KindClass    Kind = "class"
)

// numeric reports whether a kind's value is a number with a range.
func (k Kind) numeric() bool { return k == KindCount || k == KindMicroUSD || k == KindSeconds }

// list reports whether a kind's value is a set of strings.
func (k Kind) list() bool { return k == KindLogins || k == KindChecks }

// Effect is when a change to a field takes effect.
type Effect string

// The effects. A run's grant, model and harness are recorded when the run
// is created, and an intent's hand-off mode when it is first decided, so
// those follow the Project only from the next one on.
const (
	// EffectLive: read on every pass, open intents included.
	EffectLive Effect = "live"
	// EffectNewRuns: runs created after the save.
	EffectNewRuns Effect = "new-runs"
	// EffectNewIntents: intents whose hand-off is not yet recorded.
	EffectNewIntents Effect = "new-intents"
	// EffectNextRound: the next evaluation round.
	EffectNextRound Effect = "next-round"
)

// The ids of the fields other code names. An id is the field's path below
// spec; the per-repository field's stands for every repository's.
const (
	FieldApprovers           = "approvers.logins"
	FieldVerificationEnabled = "verification.enabled"
	FieldHandoff             = "verification.handoff"
	FieldHandoffMax          = "verification.handoffMax"
	FieldMaxToolCalls        = "verification.maxToolCalls"
	FieldMaxCost             = "limits.maxCostMicroUSD"
	FieldMaxEvaluateCost     = "limits.maxEvaluateCostMicroUSD"
	FieldMaxRevisions        = "limits.maxRevisions"
	FieldMaxCheckFixes       = "limits.maxCheckFixes"
	FieldMaxVerifyFixes      = "limits.maxVerifyFixes"
	FieldMaxVerifications    = "limits.maxVerifications"
	FieldMaxActiveIntents    = "limits.maxActiveIntents"
	FieldChecksFix           = "checks.fix"
	FieldChecksTimeout       = "checks.timeout"
	FieldChecksRerunFailed   = "checks.rerunFailed"
	FieldRepositoryClass     = "repositories.agentResourceClass"
)

// Field is one editable setting of a Project: a row of the table everything
// else in the package is driven by. A field not in the table cannot be
// changed by Apply, whatever a save holds.
type Field struct {
	// ID is the field's path below spec.
	ID string
	// Group is its lock group.
	Group Group
	// Kind is how its value is written.
	Kind Kind
	// Min and Max are the Project schema's own range: a number's, a
	// duration's in seconds, a list's item count. Both zero: no range.
	Min, Max int64
	// Default is what the field lands on when unset, where the schema or
	// the code fixes that (HasDefault). A stage limit's is the controller's
	// ceiling, which the table cannot know.
	Default    int64
	HasDefault bool
	// Nullable: zero is a value of its own, and unset is null.
	Nullable bool
	// Effect is when a change takes effect.
	Effect Effect
}

// Path is the field's path in the Project, as an admission policy or a
// reader of the schema spells it.
func (f Field) Path() string {
	if f.ID == FieldRepositoryClass {
		return "spec.repositories[].agentResourceClass"
	}
	return "spec." + f.ID
}

// Fields are the editable fields, in the page's order.
func Fields() []Field {
	out := make([]Field, 0, len(table)+1)
	for _, r := range table {
		out = append(out, r.Field)
	}
	return append(out, classField)
}

// StageNames are the stages that have limits, in spec.limits' spelling:
// every stage the controller runs, the check reviewer's included, though the
// page offers no limit of that one (Fields has none).
func StageNames() []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.name)
	}
	return out
}

// StageField is the id of one limit of one stage: leaf is maxTurns,
// tokenBudget, maxCostMicroUSD or timeout.
func StageField(stage, leaf string) string { return "limits." + stage + "." + leaf }

// The leaves of a stage's limits.
const (
	leafTurns   = "maxTurns"
	leafTokens  = "tokenBudget"
	leafCost    = "maxCostMicroUSD"
	leafTimeout = "timeout"
)

// classField is the one per-repository field. It is not a row of table,
// because it has a value per repository; Apply handles it by name.
var classField = Field{ID: FieldRepositoryClass, Group: GroupResources, Kind: KindClass, Effect: EffectNewRuns}

// value is one field's value in the document's own terms: a number (whole
// seconds for a duration, 1 for true), a string or a set of strings.
type value struct {
	n    int64
	null bool
	s    string
	list []string
}

// unset reports a number that states nothing.
func (v value) unset(f Field) bool {
	if f.Nullable {
		return v.null
	}
	return v.n == 0
}

// equal compares two values of one field; lists compare as sets.
func (v value) equal(w value) bool {
	return v.n == w.n && v.null == w.null && v.s == w.s && sameSet(v.list, w.list)
}

// text is the value as a change reports it.
func (v value) text(f Field) string {
	switch {
	case f.Kind.list():
		return strings.Join(v.list, ", ")
	case f.Kind == KindBool:
		return strconv.FormatBool(v.n != 0)
	case !f.Kind.numeric():
		return v.s
	case v.unset(f):
		return ""
	}
	return strconv.FormatInt(v.n, 10)
}

// sameSet reports whether two lists hold the same strings, compared exactly
// as the API server compares a set's items.
func sameSet(a, b []string) bool {
	return len(missing(a, b)) == 0 && len(missing(b, a)) == 0
}

// missing are the entries of from that in does not hold, in from's order,
// each once.
func missing(from, in []string) []string {
	var out []string
	for _, s := range from {
		if !slices.Contains(in, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// row is a field with its accessors.
type row struct {
	Field
	// defaulted: the schema defaults the field, so the stored value is
	// never unset and an unset one in a save means the default.
	defaulted bool
	// slot points at the field's place in a document.
	slot func(*ProjectSettings) any
	// read is the field's value in a live spec, in the document's terms.
	read func(*v1alpha1.ProjectSpec) value
	// write sets the field in a spec.
	write func(*v1alpha1.ProjectSpec, value)
}

// load reads a document slot.
func load(slot any) value {
	switch p := slot.(type) {
	case *int32:
		return value{n: int64(*p)}
	case *int64:
		return value{n: *p}
	case **int32:
		if *p == nil {
			return value{null: true}
		}
		return value{n: int64(**p)}
	case *bool:
		if *p {
			return value{n: 1}
		}
	case *string:
		return value{s: *p}
	case *[]string:
		return value{list: *p}
	}
	return value{}
}

// store writes a document slot.
func store(slot any, v value) {
	switch p := slot.(type) {
	case *int32:
		*p = int32(v.n)
	case *int64:
		*p = v.n
	case **int32:
		*p = nil
		if !v.null {
			n := int32(v.n)
			*p = &n
		}
	case *bool:
		*p = v.n != 0
	case *string:
		*p = v.s
	case *[]string:
		*p = append([]string{}, v.list...)
	}
}

// normal is a submitted value as Apply compares it: an unset value of a
// field the schema defaults is that default, and a list holds each entry
// once.
func (r row) normal(v value) value {
	if r.Kind.list() {
		return value{list: missing(v.list, nil)}
	}
	if r.defaulted && r.Kind.numeric() && v.unset(r.Field) {
		return value{n: r.Default}
	}
	if v.null {
		return value{null: true}
	}
	return v
}

// landing is the number a value of the field takes effect as: the value,
// or the field's default when unset. open reports an unset value with no
// fixed default, which lands on the controller's own ceiling: the most the
// field can be.
func (f Field) landing(v value) (n int64, open bool) {
	switch {
	case !v.unset(f):
		return v.n, false
	case f.HasDefault:
		return f.Default, false
	}
	return 0, true
}

// zeroOf is the zero value of the type a pointer points at. It lets this
// package build a value of a type it never names: the API's duration is
// apimachinery's, which the package does not import.
func zeroOf[T any](*T) (zero T) { return zero }

// wholeSeconds is a duration as the document carries it.
func wholeSeconds(d time.Duration) int64 { return int64(d / time.Second) }

// MaxSeconds bounds a duration in a document, so that it is a duration at
// all; the schema's own ranges are far inside it.
const MaxSeconds = 366 * 24 * 60 * 60

// ptrNum is a pointer to n.
func ptrNum[T int32 | int64](n int64) *T {
	v := T(n)
	return &v
}

// verification and models return the spec's optional blocks for a write,
// made when absent. Only a write of a changed leaf calls them, so a block
// is never created by a read or by a save that leaves it alone.
func verification(s *v1alpha1.ProjectSpec) *v1alpha1.ProjectVerification {
	if s.Verification == nil {
		s.Verification = &v1alpha1.ProjectVerification{}
	}
	return s.Verification
}

func models(s *v1alpha1.ProjectSpec) *v1alpha1.ProjectModels {
	if s.Models == nil {
		s.Models = &v1alpha1.ProjectModels{}
	}
	return s.Models
}

// stage is one stage that has limits, a model of its own or not.
type stage struct {
	name string
	// limits and doc are the stage's limits in a spec and in a document. A
	// stage with no doc has limits the page does not offer (the check
	// reviewer, being retired: the owner's decision 8): they are no rows of
	// the table, but still count toward its run budget.
	limits func(*v1alpha1.ProjectLimits) *v1alpha1.StageLimits
	doc    func(*ProjectSettingsStages) *ProjectSettingsStage
	// ceiling is the controller's ceiling for the stage.
	ceiling func(*v1alpha1.ProjectStageCeilings) *v1alpha1.ProjectStageCeiling
	// role: an evaluator stage, spent against the evaluation ceiling.
	role bool
}

var stages = []stage{
	{name: "plan",
		limits:  func(l *v1alpha1.ProjectLimits) *v1alpha1.StageLimits { return &l.Plan },
		doc:     func(d *ProjectSettingsStages) *ProjectSettingsStage { return &d.Plan },
		ceiling: func(c *v1alpha1.ProjectStageCeilings) *v1alpha1.ProjectStageCeiling { return &c.Plan }},
	{name: "build",
		limits:  func(l *v1alpha1.ProjectLimits) *v1alpha1.StageLimits { return &l.Build },
		doc:     func(d *ProjectSettingsStages) *ProjectSettingsStage { return &d.Build },
		ceiling: func(c *v1alpha1.ProjectStageCeilings) *v1alpha1.ProjectStageCeiling { return &c.Build }},
	{name: "revise",
		limits:  func(l *v1alpha1.ProjectLimits) *v1alpha1.StageLimits { return &l.Revise },
		doc:     func(d *ProjectSettingsStages) *ProjectSettingsStage { return &d.Revise },
		ceiling: func(c *v1alpha1.ProjectStageCeilings) *v1alpha1.ProjectStageCeiling { return &c.Revise }},
	{name: "evaluate", role: true,
		limits:  func(l *v1alpha1.ProjectLimits) *v1alpha1.StageLimits { return &l.Evaluate },
		doc:     func(d *ProjectSettingsStages) *ProjectSettingsStage { return &d.Evaluate },
		ceiling: func(c *v1alpha1.ProjectStageCeilings) *v1alpha1.ProjectStageCeiling { return &c.Evaluate }},
	{name: "checkReview", role: true,
		limits:  func(l *v1alpha1.ProjectLimits) *v1alpha1.StageLimits { return &l.CheckReview },
		ceiling: func(c *v1alpha1.ProjectStageCeilings) *v1alpha1.ProjectStageCeiling { return &c.CheckReview }},
}

// table is every editable field but the per-repository one.
var table = buildTable()

func buildTable() []row {
	rows := []row{approverRow()}
	rows = append(rows, verificationRows()...)
	rows = append(rows, modelRows()...)
	rows = append(rows, limitRows()...)
	for _, s := range stages {
		if s.doc != nil {
			rows = append(rows, stageRows(s)...)
		}
	}
	return append(rows, checkRows()...)
}

// rowByID finds a row of the table.
func rowByID(id string) (row, bool) {
	for _, r := range table {
		if r.ID == id {
			return r, true
		}
	}
	return row{}, false
}

func approverRow() row {
	return row{
		Field: Field{ID: FieldApprovers, Group: GroupApprovers, Kind: KindLogins, Min: 1,
			Max: v1alpha1.MaxProjectApprovers, Effect: EffectLive},
		slot:  func(d *ProjectSettings) any { return &d.Approvers },
		read:  func(s *v1alpha1.ProjectSpec) value { return value{list: s.Approvers.Logins} },
		write: func(s *v1alpha1.ProjectSpec, v value) { s.Approvers.Logins = merged(s.Approvers.Logins, v.list) },
	}
}

// merged is a set changed to hold exactly want: the entries of cur it
// keeps stay in their order, and the new ones follow in want's. A set the
// API server stores keeps its order, so a kept entry never moves.
func merged(cur, want []string) []string {
	out := make([]string, 0, len(want))
	for _, s := range cur {
		if slices.Contains(want, s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return append(out, missing(want, cur)...)
}

func verificationRows() []row {
	return []row{
		{
			Field: Field{ID: FieldVerificationEnabled, Group: GroupEvaluator, Kind: KindBool, Effect: EffectLive},
			slot:  func(d *ProjectSettings) any { return &d.Verification.Enabled },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.VerificationEnabled() {
					return value{n: 1}
				}
				return value{}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) { verification(s).Enabled = v.n != 0 },
		},
		{
			Field: Field{ID: FieldHandoff, Group: GroupHandoff, Kind: KindHandoff, Effect: EffectNewIntents},
			slot:  func(d *ProjectSettings) any { return &d.Verification.Handoff },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Verification == nil {
					return value{}
				}
				return value{s: string(s.Verification.Handoff)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				verification(s).Handoff = v1alpha1.HandoffMode(v.s)
			},
		},
		{
			Field: Field{ID: FieldHandoffMax, Group: GroupHandoff, Kind: KindSeconds, Min: 10 * 60, Max: 4 * 60 * 60,
				Default: wholeSeconds(v1alpha1.DefaultHandoffMax), HasDefault: true, Effect: EffectLive},
			slot: func(d *ProjectSettings) any { return &d.Verification.HandoffMaxSeconds },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Verification == nil || s.Verification.HandoffMax == nil {
					return value{}
				}
				return value{n: wholeSeconds(s.Verification.HandoffMax.Duration)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				ver := verification(s)
				if v.n == 0 {
					ver.HandoffMax = nil
					return
				}
				d := zeroOf(ver.HandoffMax)
				d.Duration = time.Duration(v.n) * time.Second
				ver.HandoffMax = &d
			},
		},
		{
			Field: Field{ID: FieldMaxToolCalls, Group: GroupEvaluator, Kind: KindCount, Min: v1alpha1.MinToolCalls,
				Max: v1alpha1.MaxToolCallsLimit, Default: int64(v1alpha1.DefaultMaxToolCalls), HasDefault: true,
				Effect: EffectNextRound},
			slot: func(d *ProjectSettings) any { return &d.Verification.MaxToolCalls },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Verification == nil || s.Verification.MaxToolCalls == nil {
					return value{}
				}
				return value{n: int64(*s.Verification.MaxToolCalls)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				ver := verification(s)
				ver.MaxToolCalls = nil
				if v.n != 0 {
					ver.MaxToolCalls = ptrNum[int32](v.n)
				}
			},
		},
	}
}

func modelRows() []row {
	type model struct {
		name string
		doc  func(*ProjectSettingsModels) *string
		spec func(*v1alpha1.ProjectModels) *string
	}
	rows := make([]row, 0, 3)
	for _, m := range []model{
		{"plan", func(d *ProjectSettingsModels) *string { return &d.Plan },
			func(s *v1alpha1.ProjectModels) *string { return &s.Plan }},
		{"build", func(d *ProjectSettingsModels) *string { return &d.Build },
			func(s *v1alpha1.ProjectModels) *string { return &s.Build }},
		{"evaluate", func(d *ProjectSettingsModels) *string { return &d.Evaluate },
			func(s *v1alpha1.ProjectModels) *string { return &s.Evaluate }},
	} {
		rows = append(rows, row{
			Field: Field{ID: "models." + m.name, Group: GroupModels, Kind: KindModel, Effect: EffectNewRuns},
			slot:  func(d *ProjectSettings) any { return m.doc(&d.Models) },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Models == nil {
					return value{}
				}
				return value{s: *m.spec(s.Models)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) { *m.spec(models(s)) = v.s },
		})
	}
	return rows
}

// limitRows are the per-intent limits.
func limitRows() []row {
	return []row{
		{
			Field: Field{ID: FieldMaxCost, Group: GroupCost, Kind: KindMicroUSD, Min: 1, Max: 1000000000,
				Default: v1alpha1.DefaultMaxCostMicroUSD, HasDefault: true, Effect: EffectLive},
			defaulted: true,
			slot:      func(d *ProjectSettings) any { return &d.Limits.MaxCostMicroUSD },
			read: func(s *v1alpha1.ProjectSpec) value {
				return value{n: orDefault(s.Limits.MaxCostMicroUSD, v1alpha1.DefaultMaxCostMicroUSD)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) { s.Limits.MaxCostMicroUSD = v.n },
		},
		{
			Field: Field{ID: FieldMaxEvaluateCost, Group: GroupCost, Kind: KindMicroUSD, Min: 1, Max: 1000000000,
				Default: v1alpha1.DefaultMaxEvaluateCostMicroUSD, HasDefault: true, Effect: EffectLive},
			slot: func(d *ProjectSettings) any { return &d.Limits.MaxEvaluateCostMicroUSD },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Limits.MaxEvaluateCostMicroUSD == nil {
					return value{}
				}
				return value{n: *s.Limits.MaxEvaluateCostMicroUSD}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				s.Limits.MaxEvaluateCostMicroUSD = nil
				if v.n != 0 {
					s.Limits.MaxEvaluateCostMicroUSD = ptrNum[int64](v.n)
				}
			},
		},
		roundsRow(FieldMaxRevisions, 20, v1alpha1.DefaultMaxRevisions, true,
			func(d *ProjectSettingsLimits) **int32 { return &d.MaxRevisions },
			func(l *v1alpha1.ProjectLimits) **int32 { return &l.MaxRevisions }),
		roundsRow(FieldMaxCheckFixes, 20, v1alpha1.DefaultMaxCheckFixes, true,
			func(d *ProjectSettingsLimits) **int32 { return &d.MaxCheckFixes },
			func(l *v1alpha1.ProjectLimits) **int32 { return &l.MaxCheckFixes }),
		roundsRow(FieldMaxVerifyFixes, 10, v1alpha1.DefaultMaxVerifyFixes, false,
			func(d *ProjectSettingsLimits) **int32 { return &d.MaxVerifyFixes },
			func(l *v1alpha1.ProjectLimits) **int32 { return &l.MaxVerifyFixes }),
		{
			Field: Field{ID: FieldMaxVerifications, Group: GroupRounds, Kind: KindCount, Min: 1, Max: 100,
				Default: int64(v1alpha1.DefaultMaxVerifications), HasDefault: true, Effect: EffectLive},
			slot: func(d *ProjectSettings) any { return &d.Limits.MaxVerifications },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Limits.MaxVerifications == nil {
					return value{}
				}
				return value{n: int64(*s.Limits.MaxVerifications)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				s.Limits.MaxVerifications = nil
				if v.n != 0 {
					s.Limits.MaxVerifications = ptrNum[int32](v.n)
				}
			},
		},
		{
			Field: Field{ID: FieldMaxActiveIntents, Group: GroupActiveIntents, Kind: KindCount, Min: 1, Max: 20,
				Default: int64(v1alpha1.DefaultMaxActiveIntents), HasDefault: true, Effect: EffectLive},
			defaulted: true,
			slot:      func(d *ProjectSettings) any { return &d.Limits.MaxActiveIntents },
			read: func(s *v1alpha1.ProjectSpec) value {
				return value{n: orDefault(int64(s.Limits.MaxActiveIntents), int64(v1alpha1.DefaultMaxActiveIntents))}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) { s.Limits.MaxActiveIntents = int32(v.n) },
		},
	}
}

// orDefault is n, or def for the zero an undefaulted Project holds.
func orDefault(n, def int64) int64 {
	if n == 0 {
		return def
	}
	return n
}

// roundsRow is a round limit where zero is a value (no such rounds), so
// unset is null. The schema defaults two of the three (defaulted): those
// read as their default when a Project holds none.
func roundsRow(id string, limit int64, def int32, defaulted bool,
	doc func(*ProjectSettingsLimits) **int32, spec func(*v1alpha1.ProjectLimits) **int32) row {
	return row{
		Field: Field{ID: id, Group: GroupRounds, Kind: KindCount, Max: limit, Default: int64(def),
			HasDefault: true, Nullable: true, Effect: EffectLive},
		defaulted: defaulted,
		slot:      func(d *ProjectSettings) any { return doc(&d.Limits) },
		read: func(s *v1alpha1.ProjectSpec) value {
			switch p := *spec(&s.Limits); {
			case p != nil:
				return value{n: int64(*p)}
			case defaulted:
				return value{n: int64(def)}
			}
			return value{null: true}
		},
		write: func(s *v1alpha1.ProjectSpec, v value) {
			*spec(&s.Limits) = nil
			if !v.null {
				*spec(&s.Limits) = ptrNum[int32](v.n)
			}
		},
	}
}

// stageRows are one stage's four limits.
func stageRows(st stage) []row {
	field := func(leaf string, kind Kind, lo, hi int64) Field {
		return Field{ID: StageField(st.name, leaf), Group: GroupStages, Kind: kind, Min: lo, Max: hi,
			Effect: EffectNewRuns}
	}
	return []row{
		{
			Field: field(leafTurns, KindCount, 0, 1000),
			slot:  func(d *ProjectSettings) any { return &st.doc(&d.Stages).MaxTurns },
			read:  func(s *v1alpha1.ProjectSpec) value { return value{n: int64(st.limits(&s.Limits).MaxTurns)} },
			write: func(s *v1alpha1.ProjectSpec, v value) { st.limits(&s.Limits).MaxTurns = int32(v.n) },
		},
		{
			Field: field(leafTokens, KindCount, 0, 100000000),
			slot:  func(d *ProjectSettings) any { return &st.doc(&d.Stages).TokenBudget },
			read:  func(s *v1alpha1.ProjectSpec) value { return value{n: st.limits(&s.Limits).TokenBudget} },
			write: func(s *v1alpha1.ProjectSpec, v value) { st.limits(&s.Limits).TokenBudget = v.n },
		},
		{
			Field: field(leafCost, KindMicroUSD, 0, 1000000000),
			slot:  func(d *ProjectSettings) any { return &st.doc(&d.Stages).MaxCostMicroUSD },
			read:  func(s *v1alpha1.ProjectSpec) value { return value{n: st.limits(&s.Limits).MaxCostMicroUSD} },
			write: func(s *v1alpha1.ProjectSpec, v value) { st.limits(&s.Limits).MaxCostMicroUSD = v.n },
		},
		{
			Field: field(leafTimeout, KindSeconds, 60, 4*60*60),
			slot:  func(d *ProjectSettings) any { return &st.doc(&d.Stages).TimeoutSeconds },
			read: func(s *v1alpha1.ProjectSpec) value {
				if t := st.limits(&s.Limits).Timeout; t != nil {
					return value{n: wholeSeconds(t.Duration)}
				}
				return value{}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				l := st.limits(&s.Limits)
				if v.n == 0 {
					l.Timeout = nil
					return
				}
				d := zeroOf(l.Timeout)
				d.Duration = time.Duration(v.n) * time.Second
				l.Timeout = &d
			},
		},
	}
}

func checkRows() []row {
	const defaultTimeout = 30 * 60
	return []row{
		{
			Field: Field{ID: FieldChecksFix, Group: GroupChecks, Kind: KindChecks, Max: 32, Effect: EffectLive},
			slot:  func(d *ProjectSettings) any { return &d.Checks.Fix },
			read:  func(s *v1alpha1.ProjectSpec) value { return value{list: s.Checks.Fix} },
			write: func(s *v1alpha1.ProjectSpec, v value) {
				s.Checks.Fix = merged(s.Checks.Fix, v.list)
				if len(s.Checks.Fix) == 0 {
					s.Checks.Fix = nil
				}
			},
		},
		{
			Field: Field{ID: FieldChecksTimeout, Group: GroupChecks, Kind: KindSeconds, Min: 60, Max: 6 * 60 * 60,
				Default: defaultTimeout, HasDefault: true, Effect: EffectLive},
			defaulted: true,
			slot:      func(d *ProjectSettings) any { return &d.Checks.TimeoutSeconds },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Checks.Timeout == nil {
					return value{n: defaultTimeout}
				}
				return value{n: wholeSeconds(s.Checks.Timeout.Duration)}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) {
				d := zeroOf(s.Checks.Timeout)
				d.Duration = time.Duration(v.n) * time.Second
				s.Checks.Timeout = &d
			},
		},
		{
			Field: Field{ID: FieldChecksRerunFailed, Group: GroupChecks, Kind: KindBool, Effect: EffectLive},
			slot:  func(d *ProjectSettings) any { return &d.Checks.RerunFailed },
			read: func(s *v1alpha1.ProjectSpec) value {
				if s.Checks.RerunFailed {
					return value{n: 1}
				}
				return value{}
			},
			write: func(s *v1alpha1.ProjectSpec, v value) { s.Checks.RerunFailed = v.n != 0 },
		},
	}
}
