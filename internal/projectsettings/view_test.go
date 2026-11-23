// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

func fieldOf(t *testing.T, v ProjectSettingsView, id string) ProjectSettingsField {
	t.Helper()
	for _, f := range v.Fields {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("the view has no field %s", id)
	return ProjectSettingsField{}
}

func TestViewOfAnApprover(t *testing.T) {
	p := baseProject()
	v := View(p, DefaultPolicy(), 1, testNow)
	if v.Project != "demo" || v.Generation != 7 || v.ObservedGeneration != 7 || v.Schema != Schema {
		t.Errorf("header = %s gen %d observed %d schema %d", v.Project, v.Generation, v.ObservedGeneration, v.Schema)
	}
	if !v.CanEdit || v.Reason != "" {
		t.Errorf("canEdit %v reason %q, want an approver to edit", v.CanEdit, v.Reason)
	}
	if !reflect.DeepEqual(v.Settings, Read(&p.Spec)) {
		t.Errorf("settings = %+v, want what Read gives", v.Settings)
	}
	wantApprovers := []ProjectSettingsApprover{
		{Login: "alice", State: ApproverOK, You: true, Managers: []string{}},
		{Login: "bob", State: ApproverOK, Managers: []string{}},
	}
	if !reflect.DeepEqual(v.Approvers, wantApprovers) {
		t.Errorf("approvers = %+v, want %+v", v.Approvers, wantApprovers)
	}
	if v.ApproversResolvedAt != "2026-11-23T11:59:00Z" || v.ApproversStale {
		t.Errorf("resolved at %q, stale %v", v.ApproversResolvedAt, v.ApproversStale)
	}
	wantOptions := ProjectSettingsOptions{
		Published: true,
		Models:    []string{"anthropic/claude-a", "anthropic/claude-b", "anthropic/claude-dear"},
		DefaultModels: ProjectSettingsModels{Plan: "anthropic/claude-a", Build: "anthropic/claude-a",
			Evaluate: "anthropic/claude-b", CheckReview: "anthropic/claude-b"},
		ResourceClasses: []ProjectSettingsClass{
			{Name: "small", RequestCPU: "1", RequestMemory: "2Gi", LimitMemory: "4Gi"},
			{Name: "large", RequestCPU: "4", RequestMemory: "8Gi", LimitMemory: "16Gi"},
		},
		Evaluator: true,
	}
	if !reflect.DeepEqual(v.Options, wantOptions) {
		t.Errorf("options = %+v, want %+v", v.Options, wantOptions)
	}
	wantFacts := ProjectSettingsFacts{
		IntentRepository: "https://github.com/acme/intents",
		Repositories: []ProjectSettingsRepositoryFact{
			{Name: "app", URL: "https://github.com/acme/app", Previewed: true, PreviewPath: "/",
				PreviewImage: "registry.example/app", PreviewPort: 8080, PreviewReadinessPath: "/healthz",
				ClassManagers: []string{}},
			{Name: "lib", URL: "https://github.com/acme/lib", ClassManagers: []string{}},
		},
		TriggerLabel: "maul:demo", ApproveLabel: "maul:approved", Harness: "claude", Surfaces: []string{},
		RequireRepositoryImage: true, ActiveIntents: 1,
	}
	if !reflect.DeepEqual(v.Facts, wantFacts) {
		t.Errorf("facts = %+v, want %+v", v.Facts, wantFacts)
	}
	if v.LastEdit != nil || v.Allow != (ProjectSettingsAllow{}) || len(v.Conditions) != 0 {
		t.Errorf("last edit %+v, allow %+v, conditions %+v", v.LastEdit, v.Allow, v.Conditions)
	}
}

func TestViewBounds(t *testing.T) {
	p := baseProject(func(p *v1alpha1.Project) {
		p.Status.Admits.CostBudgets, p.Status.Admits.MaxRunCostMicroUSD = true, 12_000_000
	})
	v := View(p, DefaultPolicy(), 1, testNow)
	ids := make([]string, 0, len(v.Fields))
	for _, f := range v.Fields {
		ids = append(ids, f.ID)
	}
	want := make([]string, 0, len(Fields()))
	for _, f := range Fields() {
		want = append(want, f.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("the view's fields %v, want every editable field in order %v", ids, want)
	}
	none := []string{}
	for _, tc := range []ProjectSettingsField{
		{ID: FieldMaxRevisions, Group: "rounds", Kind: "count", Max: 20, Default: 3, Ceiling: 10, Effect: "live"},
		{ID: FieldMaxVerifications, Group: "rounds", Kind: "count", Min: 1, Max: 100, Default: 20, Ceiling: 50,
			Floor: 3, Effect: "live"},
		{ID: FieldMaxCost, Group: "cost", Kind: "microUSD", Min: 1, Max: 1_000_000_000, Default: 10_000_000,
			Ceiling: 25_000_000, Effect: "live"},
		{ID: FieldMaxEvaluateCost, Group: "cost", Kind: "microUSD", Min: 1, Max: 1_000_000_000, Default: 6_000_000,
			Ceiling: 8_000_000, Floor: 2_000_000, Effect: "live"},
		// Unset it is the code's 300, which the controller clamps to 250.
		{ID: FieldMaxToolCalls, Group: "evaluator", Kind: "count", Min: 50, Max: 300, Default: 250, Ceiling: 250,
			Floor: 100, Effect: "next-round"},
		{ID: FieldHandoffMax, Group: "handoff", Kind: "seconds", Min: 600, Max: 14400, Default: 3600, Floor: 1800,
			Effect: "live"},
		{ID: "limits.evaluate.maxTurns", Group: "stages", Kind: "count", Max: 1000, Default: 100, Ceiling: 100,
			Floor: 40, Effect: "new-runs"},
		{ID: "limits.evaluate.timeout", Group: "stages", Kind: "seconds", Min: 60, Max: 14400, Default: 1200,
			Ceiling: 1200, Floor: 600, Effect: "new-runs"},
		{ID: "limits.build.tokenBudget", Group: "stages", Kind: "count", Max: 100_000_000, Default: 800_000,
			Ceiling: 800_000, Effect: "new-runs"},
		{ID: "limits.plan.maxCostMicroUSD", Group: "stages", Kind: "microUSD", Max: 1_000_000_000,
			Ceiling: 12_000_000, Effect: "new-runs"},
		{ID: FieldChecksTimeout, Group: "checks", Kind: "seconds", Min: 60, Max: 21600, Default: 1800, Effect: "live"},
		{ID: FieldChecksFix, Group: "checks", Kind: "checks", Max: 32, Effect: "live"},
		{ID: FieldApprovers, Group: "approvers", Kind: "logins", Min: 1, Max: 20, Locked: LockedNotAllowed,
			Effect: "live"},
		{ID: "models.plan", Group: "models", Kind: "model", Effect: "new-runs"},
		{ID: FieldRepositoryClass, Group: "resources", Kind: "class", Effect: "new-runs"},
		{ID: FieldVerificationEnabled, Group: "evaluator", Kind: "bool", Effect: "live"},
		{ID: FieldHandoff, Group: "handoff", Kind: "handoff", Effect: "new-intents"},
	} {
		tc.Managers = none
		if got := fieldOf(t, v, tc.ID); !reflect.DeepEqual(got, tc) {
			t.Errorf("field %s =\n%+v\nwant\n%+v", tc.ID, got, tc)
		}
	}
}

func TestViewLocks(t *testing.T) {
	locked := func(v ProjectSettingsView) map[string]string {
		out := map[string]string{}
		for _, f := range v.Fields {
			if f.Locked != "" {
				out[f.ID] = f.Locked
			}
		}
		return out
	}
	open := openPolicy()
	if got := locked(View(baseProject(), open, 1, testNow)); len(got) != 0 {
		t.Errorf("with every switch on and nothing locked: %v", got)
	}
	pol := openPolicy()
	pol.Locked = []Group{GroupChecks, GroupApprovers}
	want := map[string]string{FieldChecksFix: LockedByOperator, FieldChecksTimeout: LockedByOperator,
		FieldChecksRerunFailed: LockedByOperator, FieldApprovers: LockedByOperator}
	if got := locked(View(baseProject(), pol, 1, testNow)); !reflect.DeepEqual(got, want) {
		t.Errorf("locked = %v, want %v", got, want)
	}
	unpublished := baseProject(func(p *v1alpha1.Project) { p.Status.Admits = nil })
	want = map[string]string{"models.plan": LockedUnpublished, "models.build": LockedUnpublished,
		"models.evaluate": LockedUnpublished, "models.checkReview": LockedUnpublished,
		FieldRepositoryClass: LockedUnpublished}
	v := View(unpublished, open, 1, testNow)
	if got := locked(v); !reflect.DeepEqual(got, want) {
		t.Errorf("with nothing published: locked = %v, want %v", got, want)
	}
	if v.Options.Published || v.Options.Models == nil || v.Options.ResourceClasses == nil {
		t.Errorf("with nothing published: options = %+v", v.Options)
	}
	if f := fieldOf(t, v, "limits.build.maxTurns"); f.Default != 0 || f.Ceiling != 0 {
		t.Errorf("with nothing published a stage limit claims a ceiling: %+v", f)
	}
	noClasses := baseProject(func(p *v1alpha1.Project) { p.Status.Admits.ResourceClasses = nil })
	if got, want := locked(View(noClasses, open, 1, testNow)), map[string]string{
		FieldRepositoryClass: LockedNoClasses}; !reflect.DeepEqual(got, want) {
		t.Errorf("with no classes defined: locked = %v, want %v", got, want)
	}
}

func TestViewApproverStates(t *testing.T) {
	p := baseProject(logins("alice", "Bob", "carol", "dave", "erin"), identities(
		v1alpha1.ProjectApproverIdentity{Login: "alice", ID: 1, Write: true},
		v1alpha1.ProjectApproverIdentity{Login: "bob", ID: 2},
		v1alpha1.ProjectApproverIdentity{Login: "carol"},
		v1alpha1.ProjectApproverIdentity{Login: "gone", ID: 9, Write: true},
		v1alpha1.ProjectApproverIdentity{Login: "erin", ID: 5, Write: true},
	), resolvedAgo(31*time.Minute))
	v := View(p, DefaultPolicy(), 5, testNow)
	none := []string{}
	want := []ProjectSettingsApprover{
		{Login: "alice", State: ApproverOK, Managers: none},
		{Login: "Bob", State: ApproverNoWrite, Managers: none},
		{Login: "carol", State: ApproverNoAccount, Managers: none},
		{Login: "dave", State: ApproverUnresolved, Managers: none},
		{Login: "erin", State: ApproverOK, You: true, Managers: none},
	}
	if !reflect.DeepEqual(v.Approvers, want) {
		t.Errorf("approvers =\n%+v\nwant\n%+v", v.Approvers, want)
	}
	if !v.ApproversStale || v.CanEdit || v.Reason != ReasonApproversStale {
		t.Errorf("stale %v, canEdit %v, reason %q", v.ApproversStale, v.CanEdit, v.Reason)
	}
	never := View(baseProject(func(p *v1alpha1.Project) { p.Status.Approvers = nil }), DefaultPolicy(), 1, testNow)
	if never.ApproversResolvedAt != "" || never.ApproversStale || never.CanEdit ||
		never.Reason != ReasonApproversUnresolved {
		t.Errorf("never resolved: %q, stale %v, canEdit %v, reason %q", never.ApproversResolvedAt,
			never.ApproversStale, never.CanEdit, never.Reason)
	}
	for _, a := range never.Approvers {
		if a.State != ApproverUnresolved || a.You {
			t.Errorf("never resolved: approver %+v", a)
		}
	}
	// Someone with no GitHub identity is nobody's "you", and an identity
	// GitHub does not know (id 0) is never them.
	for _, a := range View(p, DefaultPolicy(), 0, testNow).Approvers {
		if a.You {
			t.Errorf("a session with no id is %s", a.Login)
		}
	}
}

func TestViewFacts(t *testing.T) {
	p := baseProject(func(p *v1alpha1.Project) {
		p.Spec.Labels.Trigger = "ship-it"
		p.Spec.Harness = v1alpha1.ProjectHarnessCodex
		p.Spec.Suspend = true
		p.Spec.RequireRepositoryImage = new(false)
		p.Spec.Verification.ManualGatesStatus, p.Spec.Verification.PostLogs = true, true
		p.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Browser: &v1alpha1.BrowserSurface{}}
		p.Spec.Repositories = p.Spec.Repositories[1:]
		p.Spec.Preview = &v1alpha1.ProjectPreview{ImageRepository: "registry.example/lib", Port: 80}
	})
	got := View(p, DefaultPolicy(), 1, testNow).Facts
	want := ProjectSettingsFacts{
		IntentRepository: "https://github.com/acme/intents",
		Repositories: []ProjectSettingsRepositoryFact{{Name: "lib", URL: "https://github.com/acme/lib",
			Previewed: true, PreviewPath: "/", PreviewImage: "registry.example/lib", PreviewPort: 80,
			ClassManagers: []string{}}},
		TriggerLabel: "ship-it", ApproveLabel: "maul:approved", Harness: "codex", Surfaces: []string{"browser"},
		Suspend: true, ManualGatesStatus: true, PostLogs: true, ActiveIntents: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("facts =\n%+v\nwant\n%+v", got, want)
	}
}

func TestViewLastEdit(t *testing.T) {
	edit := NewEdit(2, "bob", testNow, []ProjectSettingsChange{{Field: FieldMaxRevisions}})
	p := baseProject(func(p *v1alpha1.Project) { p.Annotations[EditedAnnotation] = EncodeEdit(edit) })
	if got := View(p, DefaultPolicy(), 1, testNow).LastEdit; got == nil || !reflect.DeepEqual(*got, edit) {
		t.Errorf("last edit = %+v, want %+v", got, edit)
	}
	p.Annotations[EditedAnnotation] = "<img src=x>"
	if got := View(p, DefaultPolicy(), 1, testNow).LastEdit; got != nil {
		t.Errorf("an annotation that is no record is shown: %+v", got)
	}
}

// The page names the other field managers that apply a field, so a person
// knows a change made here has to be made there too (the values file, say).
// Its own manager is never one of them.
func TestViewNamesOtherFieldManagers(t *testing.T) {
	managed := func(manager, fields string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationApply,
			FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)}}
	}
	p := baseProject(func(p *v1alpha1.Project) {
		p.ManagedFields = []metav1.ManagedFieldsEntry{
			managed("helm", `{"f:spec":{"f:intentRepository":{},"f:approvers":{"f:logins":{"v:\"alice\"":{}}},
				"f:limits":{"f:maxRevisions":{},"f:plan":{"f:maxTurns":{}}},"f:checks":{"f:fix":{"v:\"test\"":{}}},
				"f:verification":{"f:enabled":{}},
				"f:repositories":{"k:{\"name\":\"app\"}":{".":{},"f:name":{},"f:url":{},"f:agentResourceClass":{}},
					"k:{\"name\":\"lib\"}":{".":{},"f:name":{},"f:url":{}}}}}`),
			managed(FieldManager, `{"f:spec":{"f:limits":{"f:maxRevisions":{},"f:maxActiveIntents":{}},
				"f:approvers":{"f:logins":{"v:\"bob\"":{}}}}}`),
			managed("kubectl-patch", `{"f:spec":{"f:limits":{"f:maxRevisions":{}},"f:models":{"f:plan":{}}}}`),
			managed("controller", `{"f:status":{"f:observedGeneration":{}}}`),
			{Manager: "broken", FieldsV1: &metav1.FieldsV1{Raw: []byte(`not json`)}},
			{Manager: "empty"},
		}
	})
	v := View(p, openPolicy(), 1, testNow)
	for id, want := range map[string][]string{
		FieldMaxRevisions:        {"helm", "kubectl-patch"},
		"limits.plan.maxTurns":   {"helm"},
		"limits.plan.timeout":    {},
		FieldMaxActiveIntents:    {},
		"models.plan":            {"kubectl-patch"},
		FieldChecksFix:           {"helm"},
		FieldVerificationEnabled: {"helm"},
		FieldApprovers:           {"helm"},
		FieldRepositoryClass:     {"helm"},
	} {
		if got := fieldOf(t, v, id).Managers; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: managers %v, want %v", id, got, want)
		}
	}
	if got := v.Approvers[0].Managers; !reflect.DeepEqual(got, []string{"helm"}) {
		t.Errorf("alice: managers %v, want helm", got)
	}
	if got := v.Approvers[1].Managers; len(got) != 0 {
		t.Errorf("bob: managers %v, want none (only the page applies bob)", got)
	}
	if got := v.Facts.Repositories[0].ClassManagers; !reflect.DeepEqual(got, []string{"helm"}) {
		t.Errorf("app: class managers %v, want helm", got)
	}
	if got := v.Facts.Repositories[1].ClassManagers; len(got) != 0 {
		t.Errorf("lib: class managers %v, want none", got)
	}
}

// The page maps over every list of the view, so none is ever null, and the
// view never changes the Project it is of.
func TestViewHasNoNullLists(t *testing.T) {
	for name, p := range map[string]*v1alpha1.Project{
		"resolved and published": baseProject(),
		"a bare Project": {Spec: v1alpha1.ProjectSpec{
			Approvers:    v1alpha1.ProjectApprovers{Logins: []string{"alice"}},
			Repositories: []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app"}}}},
		"an empty one": {},
	} {
		before := p.DeepCopy()
		raw, err := json.Marshal(View(p, DefaultPolicy(), 1, testNow))
		if err != nil {
			t.Fatal(err)
		}
		// Two values are null by design: no last edit, and an unset limit
		// whose zero is a value.
		for _, m := range regexp.MustCompile(`"(\w+)":null`).FindAllStringSubmatch(string(raw), -1) {
			if m[1] != "lastEdit" && m[1] != "maxVerifyFixes" {
				t.Errorf("%s: %s is null", name, m[1])
			}
		}
		if !strings.Contains(string(raw), `"fields":[{`) {
			t.Errorf("%s: the view lists no fields", name)
		}
		if !reflect.DeepEqual(p, before) {
			t.Errorf("%s: View changed the Project", name)
		}
	}
}
