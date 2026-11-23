// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// The states of an approver login, as intent-controller resolved it.
const (
	// ApproverOK: a GitHub account with write access to the intent
	// repository.
	ApproverOK = "ok"
	// ApproverNoWrite: an account GitHub gives no write access.
	ApproverNoWrite = "no-write-access"
	// ApproverNoAccount: GitHub knows no account of that login.
	ApproverNoAccount = "no-account"
	// ApproverUnresolved: not asked about yet.
	ApproverUnresolved = "unresolved"
)

// The operations another field manager applies a field by
// (ProjectSettingsManager.Operation), as managedFields records them.
const (
	// ManagerApply: server-side apply, Helm 4's default. Its next apply
	// that still states the field fails with a conflict on it, unless
	// forced, which puts its own value back; a set element it states and
	// the page removed comes back without one.
	ManagerApply = "Apply"
	// ManagerUpdate: a client-side apply, kubectl apply, a PUT or a patch.
	// Its next apply of a changed value replaces it, and of a list the
	// whole list.
	ManagerUpdate = "Update"
)

// Why a field cannot be changed at all.
const (
	// LockedByOperator: the operator locked its group.
	LockedByOperator = "locked"
	// LockedNotAllowed: it is behind a switch the operator left off.
	LockedNotAllowed = "not-allowed"
	// LockedUnpublished: its choices come from status.admits, which
	// intent-controller has not published.
	LockedUnpublished = "admits-unpublished"
	// LockedNoClasses: the operator defines no resource class to choose.
	LockedNoClasses = "no-classes"
)

// View is what the settings page shows of p to the GitHub account githubID
// (zero for none) at now: the settings, each field's bounds under the
// operator's policy and what intent-controller admits, the approvers as
// GitHub resolved them, the read-only facts, and whether this viewer may
// save (Gate). It reads p and changes nothing.
//
// The other field managers it names come from p's managedFields, which a
// cached read strips: give it the Project read uncached. Conditions are
// left empty; their wording is the status server's.
func View(p *v1alpha1.Project, pol Policy, githubID int64, now time.Time) ProjectSettingsView {
	gate := Gate(p, githubID, now)
	own := otherManagers(p)
	v := ProjectSettingsView{
		Project:            p.Name,
		Generation:         p.Generation,
		ObservedGeneration: p.Status.ObservedGeneration,
		Schema:             Schema,
		CanEdit:            gate.OK,
		Reason:             gate.Reason,
		Settings:           Read(&p.Spec),
		Fields:             fieldViews(p, pol, own),
		Approvers:          approverViews(p, githubID, own),
		Options:            options(p.Status.Admits),
		Allow:              pol.Allow,
		Facts:              facts(p, own),
		Conditions:         []ProjectSettingsCondition{},
	}
	if st := p.Status.Approvers; st != nil && st.ResolvedAt != nil {
		v.ApproversResolvedAt = st.ResolvedAt.UTC().Format(time.RFC3339)
		v.ApproversStale = now.Sub(st.ResolvedAt.Time) > ApproversMaxAge
	}
	if e, ok := ParseEdit(p.Annotations[EditedAnnotation]); ok {
		v.LastEdit = &e
	}
	return v
}

func fieldViews(p *v1alpha1.Project, pol Policy, own managers) []ProjectSettingsField {
	admits := p.Status.Admits
	out := make([]ProjectSettingsField, 0, len(table)+1)
	for _, f := range Fields() {
		v := ProjectSettingsField{ID: f.ID, Group: string(f.Group), Kind: string(f.Kind), Min: f.Min, Max: f.Max,
			Default: f.Default, Effect: string(f.Effect), Locked: locked(f, pol, admits),
			Managers: own.of(own.fields, f.ID)}
		// Unset, a limit the controller clamps takes effect at its
		// ceiling, or at the field's own default where that is lower.
		controller := controllerCeiling(admits, f.ID)
		if !f.HasDefault || controller > 0 && controller < f.Default {
			v.Default = controller
		}
		v.Ceiling = controller
		if c, ok := pol.Ceiling(f.ID); ok {
			v.Ceiling = c
		}
		if admits != nil && admits.CostBudgets && strings.HasSuffix(f.ID, "."+leafCost) && f.Group == GroupStages {
			v.Ceiling = admits.MaxRunCostMicroUSD
		}
		v.Floor, _ = pol.Floor(f.ID)
		out = append(out, v)
	}
	return out
}

// locked is why a field cannot be changed at all, "" when it can.
func locked(f Field, pol Policy, admits *v1alpha1.ProjectAdmits) string {
	choice := f.Kind == KindModel || f.Kind == KindClass
	switch {
	case pol.Locks(f.Group):
		return LockedByOperator
	case f.ID == FieldApprovers && !pol.Allow.Approvers:
		return LockedNotAllowed
	case choice && admits == nil:
		return LockedUnpublished
	case f.Kind == KindClass && len(admits.ResourceClasses) == 0:
		return LockedNoClasses
	}
	return ""
}

func approverViews(p *v1alpha1.Project, githubID int64, own managers) []ProjectSettingsApprover {
	out := make([]ProjectSettingsApprover, 0, len(p.Spec.Approvers.Logins))
	for _, login := range p.Spec.Approvers.Logins {
		a := ProjectSettingsApprover{Login: login, State: ApproverUnresolved, Managers: own.of(own.logins, login)}
		if who, ok := identityOf(p.Status.Approvers, login); ok {
			switch {
			case who.ID == 0:
				a.State = ApproverNoAccount
			case !who.Write:
				a.State = ApproverNoWrite
			default:
				a.State = ApproverOK
			}
			a.You = githubID > 0 && who.ID == githubID
		}
		out = append(out, a)
	}
	return out
}

func options(a *v1alpha1.ProjectAdmits) ProjectSettingsOptions {
	o := ProjectSettingsOptions{Models: []string{}, ResourceClasses: []ProjectSettingsClass{}}
	if a == nil {
		return o
	}
	o.Published, o.Evaluator, o.CostBudgets = true, a.Evaluator, a.CostBudgets
	o.Models = append(o.Models, a.Models...)
	o.DefaultModels = ProjectSettingsModels{Plan: a.DefaultModels.Plan, Build: a.DefaultModels.Build,
		Evaluate: a.DefaultModels.Evaluate}
	for _, c := range a.ResourceClasses {
		o.ResourceClasses = append(o.ResourceClasses, ProjectSettingsClass{Name: c.Name,
			RequestCPU: c.Requests.CPU, RequestMemory: c.Requests.Memory,
			LimitCPU: c.Limits.CPU, LimitMemory: c.Limits.Memory})
	}
	return o
}

// facts are the operator's settings the page shows and never changes. They
// are the operator's own words, unbounded but for the schema: the status
// server makes them fit to show.
func facts(p *v1alpha1.Project, own managers) ProjectSettingsFacts {
	s := &p.Spec
	f := ProjectSettingsFacts{
		IntentRepository:       s.IntentRepository,
		Repositories:           make([]ProjectSettingsRepositoryFact, 0, len(s.Repositories)),
		TriggerLabel:           v1alpha1.ProjectTriggerLabel(p),
		ApproveLabel:           s.Labels.Approve,
		Harness:                string(v1alpha1.ProjectHarnessClaude),
		Surfaces:               []string{},
		Suspend:                s.Suspend,
		RequireRepositoryImage: s.RequireRepositoryImage == nil || *s.RequireRepositoryImage,
		ActiveIntents:          p.Status.ActiveIntents,
	}
	if f.ApproveLabel == "" {
		f.ApproveLabel = v1alpha1.DefaultApproveLabel
	}
	if s.Harness.IsCodex() {
		f.Harness = string(v1alpha1.ProjectHarnessCodex)
	}
	if s.Verification != nil {
		f.ManualGatesStatus, f.PostLogs = s.Verification.ManualGatesStatus, s.Verification.PostLogs
	}
	if sf := s.Surfaces; sf != nil {
		for name, set := range map[string]bool{"browser": sf.Browser != nil, "cli": sf.CLI != nil,
			"library": sf.Library != nil} {
			if set {
				f.Surfaces = append(f.Surfaces, name)
			}
		}
		slices.Sort(f.Surfaces)
	}
	for i, repo := range s.Repositories {
		fact := ProjectSettingsRepositoryFact{Name: repo.Name, URL: repo.URL,
			ClassManagers: own.of(own.classes, repo.Name)}
		preview := s.Preview // the one-repository shorthand, served at "/"
		if i != 0 || repo.Preview != nil {
			preview = nil
		}
		if repo.Preview != nil {
			preview, fact.PreviewPath = &repo.Preview.ProjectPreview, repo.Preview.Path
		}
		if preview != nil {
			fact.Previewed, fact.PreviewImage = true, preview.ImageRepository
			fact.PreviewPort, fact.PreviewReadinessPath = preview.Port, preview.ReadinessPath
			if fact.PreviewPath == "" {
				fact.PreviewPath = "/"
			}
		}
		f.Repositories = append(f.Repositories, fact)
	}
	return f
}

// managers are the field managers other than the status server's own that
// apply each editable field: per field id, per approver login and per
// repository's resource class.
type managers struct {
	fields, logins, classes map[string][]ProjectSettingsManager
}

// of is the managers recorded under key, by name, never nil. A manager
// recorded under both operations is named once, as Apply: that is the one
// whose next apply stops on the edit.
func (managers) of(in map[string][]ProjectSettingsManager, key string) []ProjectSettingsManager {
	out := []ProjectSettingsManager{}
	for _, m := range in[key] {
		i := slices.IndexFunc(out, func(o ProjectSettingsManager) bool { return o.Name == m.Name })
		switch {
		case i < 0:
			out = append(out, m)
		case m.Operation == ManagerApply:
			out[i].Operation = ManagerApply
		}
	}
	slices.SortFunc(out, func(a, b ProjectSettingsManager) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// managerOf is a managedFields entry as the view names it: any operation
// but Apply is an Update, as the API server records no third.
func managerOf(name, operation string) ProjectSettingsManager {
	op := ManagerUpdate
	if operation == ManagerApply {
		op = ManagerApply
	}
	return ProjectSettingsManager{Name: name, Operation: op}
}

// otherManagers reads p's managedFields: which managers own each editable
// leaf of the spec (FieldsV1: "f:" a field, "v:" a set's value, "k:" a
// keyed list's entry). An entry that does not parse owns nothing here.
func otherManagers(p *v1alpha1.Project) managers {
	own := managers{fields: map[string][]ProjectSettingsManager{}, logins: map[string][]ProjectSettingsManager{},
		classes: map[string][]ProjectSettingsManager{}}
	for _, entry := range p.ManagedFields {
		if entry.Manager == "" || entry.Manager == FieldManager || entry.FieldsV1 == nil {
			continue
		}
		who := managerOf(entry.Manager, string(entry.Operation))
		var tree map[string]any
		if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &tree); err != nil {
			continue
		}
		spec := under(tree, "f:spec")
		for _, r := range table {
			node := spec
			for _, part := range strings.Split(r.ID, ".") {
				node = under(node, "f:"+part)
			}
			if node == nil || r.Kind.list() && len(setValues(node)) == 0 {
				continue
			}
			own.fields[r.ID] = append(own.fields[r.ID], who)
			if r.ID == FieldApprovers {
				for _, login := range setValues(node) {
					own.logins[login] = append(own.logins[login], who)
				}
			}
		}
		for key, repo := range under(spec, "f:repositories") {
			var k struct {
				Name string `json:"name"`
			}
			if !strings.HasPrefix(key, "k:") || json.Unmarshal([]byte(key[2:]), &k) != nil {
				continue
			}
			if node, _ := repo.(map[string]any); under(node, "f:agentResourceClass") != nil {
				own.classes[k.Name] = append(own.classes[k.Name], who)
				own.fields[FieldRepositoryClass] = append(own.fields[FieldRepositoryClass], who)
			}
		}
	}
	return own
}

// under is a FieldsV1 node's child, nil when it has none of that name.
func under(node map[string]any, name string) map[string]any {
	child, _ := node[name].(map[string]any)
	return child
}

// setValues are the string values a FieldsV1 set node holds ("v:" keys).
func setValues(node map[string]any) []string {
	var out []string
	for key := range node {
		var s string
		if strings.HasPrefix(key, "v:") && json.Unmarshal([]byte(key[2:]), &s) == nil {
			out = append(out, s)
		}
	}
	return out
}
