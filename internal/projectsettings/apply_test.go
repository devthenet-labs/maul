// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

func TestReadShowsTheLiveSettings(t *testing.T) {
	p := baseProject(func(p *v1alpha1.Project) {
		p.Spec.Models = &v1alpha1.ProjectModels{Build: "anthropic/claude-b"}
		p.Spec.Limits.MaxVerifyFixes = i32(0)
		p.Spec.Limits.Evaluate = v1alpha1.StageLimits{MaxTurns: 60, TokenBudget: 300_000, MaxCostMicroUSD: 2_500_000,
			Timeout: dur(15 * time.Minute)}
		p.Spec.Verification.HandoffMax = dur(45 * time.Minute)
		p.Spec.Verification.Handoff = v1alpha1.HandoffGated
	})
	want := ProjectSettings{
		Approvers: []string{"alice", "bob"},
		Verification: ProjectSettingsVerification{Enabled: true, Handoff: "gated",
			HandoffMaxSeconds: 2700},
		Models: ProjectSettingsModels{Build: "anthropic/claude-b"},
		Limits: ProjectSettingsLimits{MaxCostMicroUSD: 10_000_000, MaxRevisions: i32(3), MaxCheckFixes: i32(2),
			MaxVerifyFixes: i32(0), MaxActiveIntents: 2},
		Stages: ProjectSettingsStages{Evaluate: ProjectSettingsStage{MaxTurns: 60, TokenBudget: 300_000,
			MaxCostMicroUSD: 2_500_000, TimeoutSeconds: 900}},
		Checks: ProjectSettingsChecks{Fix: []string{"test", "lint"}, TimeoutSeconds: 1800},
		Repositories: []ProjectSettingsRepository{{Name: "app", AgentResourceClass: "small"},
			{Name: "lib"}},
	}
	if got := Read(&p.Spec); !reflect.DeepEqual(got, want) {
		t.Errorf("Read =\n%+v\nwant\n%+v", got, want)
	}
}

// A Project the schema never defaulted (a fake client's) reads as the
// schema would have stored it, and the lists a document carries are never
// null: the page maps over them.
func TestReadAppliesTheSchemaDefaults(t *testing.T) {
	spec := v1alpha1.ProjectSpec{Approvers: v1alpha1.ProjectApprovers{Logins: []string{"alice"}},
		Repositories: []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app"}}}
	got := Read(&spec)
	want := ProjectSettingsLimits{MaxCostMicroUSD: 10_000_000, MaxRevisions: i32(3), MaxCheckFixes: i32(2),
		MaxActiveIntents: 2}
	if !reflect.DeepEqual(got.Limits, want) {
		t.Errorf("limits = %+v, want %+v", got.Limits, want)
	}
	if got.Checks.TimeoutSeconds != 1800 {
		t.Errorf("checks.timeoutSeconds = %d, want the default 1800", got.Checks.TimeoutSeconds)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"fix":null`) || strings.Contains(string(raw), `"approvers":null`) ||
		strings.Contains(string(raw), `"repositories":null`) {
		t.Errorf("a list is null in %s", raw)
	}
	if !reflect.DeepEqual(spec.Limits, v1alpha1.ProjectLimits{}) {
		t.Error("Read wrote the defaults into the spec")
	}
}

// changeOf is a change of a field that is no list: nothing added or removed.
func changeOf(field, item, group, from, to, effect string) ProjectSettingsChange {
	return ProjectSettingsChange{Field: field, Item: item, Group: group, From: from, To: to, Added: []string{},
		Removed: []string{}, Effect: effect}
}

func TestApplyWritesOnlyWhatDiffers(t *testing.T) {
	p := baseProject()
	out, changes := save(t, p, func(d *ProjectSettings) {
		d.Limits.MaxRevisions = i32(7)
		d.Stages.Build.TimeoutSeconds = 2400
		d.Models.Plan = "anthropic/claude-b"
	})
	want := []ProjectSettingsChange{
		changeOf("models.plan", "", "models", "", "anthropic/claude-b", "new-runs"),
		changeOf("limits.maxRevisions", "", "rounds", "3", "7", "live"),
		changeOf("limits.build.timeout", "", "stages", "", "2400", "new-runs"),
	}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes =\n%+v\nwant\n%+v", changes, want)
	}
	const patch = `{"spec":{"limits":{"build":{"timeout":"40m0s"},"maxRevisions":7},` +
		`"models":{"plan":"anthropic/claude-b"}}}`
	if got := mergePatch(t, p, out); got != patch {
		t.Errorf("merge patch = %s\nwant         %s", got, patch)
	}
}

// The delegation switches are written leaf by leaf: turning one on makes
// the block, turning it off sends a null for that leaf alone (the bools are
// omitempty, so false and absent are one value), and a Project that never
// opted in keeps no block.
func TestApplyDelegation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		from    *v1alpha1.ProjectDelegation
		edit    func(*ProjectSettings)
		changes []ProjectSettingsChange
		patch   string
	}{
		{"subagents on", nil, func(d *ProjectSettings) { d.Delegation.Subagents = true },
			[]ProjectSettingsChange{changeOf(FieldSubagents, "", "delegation", "false", "true", "new-runs")},
			`{"spec":{"delegation":{"subagents":true}}}`},
		{"both on", nil, func(d *ProjectSettings) {
			d.Delegation = ProjectSettingsDelegation{Subagents: true, Workflows: true}
		},
			[]ProjectSettingsChange{changeOf(FieldSubagents, "", "delegation", "false", "true", "new-runs"),
				changeOf(FieldWorkflows, "", "delegation", "false", "true", "new-runs")},
			`{"spec":{"delegation":{"subagents":true,"workflows":true}}}`},
		{"workflows off", &v1alpha1.ProjectDelegation{Subagents: true, Workflows: true},
			func(d *ProjectSettings) { d.Delegation.Workflows = false },
			[]ProjectSettingsChange{changeOf(FieldWorkflows, "", "delegation", "true", "false", "new-runs")},
			`{"spec":{"delegation":{"workflows":null}}}`},
		{"both off", &v1alpha1.ProjectDelegation{Subagents: true, Workflows: true},
			func(d *ProjectSettings) { d.Delegation = ProjectSettingsDelegation{} },
			[]ProjectSettingsChange{changeOf(FieldSubagents, "", "delegation", "true", "false", "new-runs"),
				changeOf(FieldWorkflows, "", "delegation", "true", "false", "new-runs")},
			`{"spec":{"delegation":{"subagents":null,"workflows":null}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := baseProject(func(p *v1alpha1.Project) { p.Spec.Delegation = tt.from })
			out, changes := save(t, p, tt.edit)
			if !reflect.DeepEqual(changes, tt.changes) {
				t.Errorf("changes =\n%+v\nwant\n%+v", changes, tt.changes)
			}
			if got := mergePatch(t, p, out); got != tt.patch {
				t.Errorf("merge patch = %s\nwant         %s", got, tt.patch)
			}
		})
	}
	p := baseProject()
	out, changes := save(t, p, func(d *ProjectSettings) { d.Limits.MaxRevisions = i32(7) })
	if len(changes) != 1 || out.Spec.Delegation != nil {
		t.Errorf("a save leaving the switches alone: changes %+v, delegation %+v; want no block made",
			changes, out.Spec.Delegation)
	}
}

// Changing one repository's class sends the whole list, every entry
// complete and in order: a JSON merge patch can only replace a list.
func TestApplyClassSendsEveryRepositoryWhole(t *testing.T) {
	p := baseProject()
	out, changes := save(t, p, func(d *ProjectSettings) { d.Repositories[1].AgentResourceClass = "large" })
	want := []ProjectSettingsChange{changeOf(FieldRepositoryClass, "lib", "resources", "", "large", "new-runs")}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
	const patch = `{"spec":{"repositories":[{"agentResourceClass":"small","name":"app","preview":` +
		`{"imageRepository":"registry.example/app","path":"/","port":8080,"readinessPath":"/healthz"},` +
		`"url":"https://github.com/acme/app"},{"agentResourceClass":"large","name":"lib",` +
		`"url":"https://github.com/acme/lib"}]}}`
	if got := mergePatch(t, p, out); got != patch {
		t.Errorf("merge patch = %s\nwant         %s", got, patch)
	}
}

func TestApplyRepositories(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repos  []ProjectSettingsRepository
		reason string
		want   []string // the classes of app and lib afterwards
	}{
		{name: "a repository left out is left alone", repos: []ProjectSettingsRepository{{Name: "lib",
			AgentResourceClass: "large"}}, want: []string{"small", "large"}},
		{name: "none named changes none", repos: nil, want: []string{"small", ""}},
		{name: "named out of order", repos: []ProjectSettingsRepository{{Name: "lib", AgentResourceClass: "large"},
			{Name: "app"}}, want: []string{"", "large"}},
		{name: "a repository the Project does not have", repos: []ProjectSettingsRepository{{Name: "evil",
			AgentResourceClass: "large"}}, reason: DocumentUnknownRepository},
		{name: "a repository named twice", repos: []ProjectSettingsRepository{{Name: "app"},
			{Name: "app", AgentResourceClass: "large"}}, reason: DocumentDuplicateRepository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProject()
			doc := Read(&p.Spec)
			doc.Repositories = tc.repos
			out := p.DeepCopy()
			_, err := Apply(out, doc)
			if tc.reason != "" {
				var de *DocumentError
				if !errors.As(err, &de) || de.Reason != tc.reason || de.Field != FieldRepositoryClass {
					t.Fatalf("Apply error = %v, want a DocumentError %s", err, tc.reason)
				}
				if !reflect.DeepEqual(out, p) {
					t.Error("a refused document changed the Project")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := []string{out.Spec.Repositories[0].AgentResourceClass, out.Spec.Repositories[1].AgentResourceClass}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("classes = %q, want %q", got, tc.want)
			}
		})
	}
}

// A duration no schema could hold is refused as a document, before it can
// overflow into some other duration.
func TestApplyRefusesAnImpossibleDuration(t *testing.T) {
	for _, secs := range []int64{-1, MaxSeconds + 1, 1 << 62} {
		p := baseProject()
		doc := Read(&p.Spec)
		doc.Stages.Plan.TimeoutSeconds = secs
		out := p.DeepCopy()
		_, err := Apply(out, doc)
		var de *DocumentError
		if !errors.As(err, &de) || de.Reason != DocumentNotADuration || de.Field != "limits.plan.timeout" {
			t.Errorf("%d seconds: Apply error = %v, want a DocumentError %s", secs, err, DocumentNotADuration)
		}
		if !reflect.DeepEqual(out, p) {
			t.Errorf("%d seconds: a refused document changed the Project", secs)
		}
	}
}

// A set keeps the order the API server stores: an entry that stays never
// moves, a new one goes last, and the same set in another order is no
// change at all.
func TestApplyKeepsASetsOrder(t *testing.T) {
	p := baseProject(logins("alice", "bob", "carol"))
	out, changes := save(t, p, func(d *ProjectSettings) { d.Approvers = []string{"dave", "carol", "alice", "dave"} })
	if got, want := out.Spec.Approvers.Logins, []string{"alice", "carol", "dave"}; !reflect.DeepEqual(got, want) {
		t.Errorf("logins = %v, want %v", got, want)
	}
	want := []ProjectSettingsChange{{Field: FieldApprovers, Group: "approvers", From: "alice, bob, carol",
		To: "alice, carol, dave", Added: []string{"dave"}, Removed: []string{"bob"}, Effect: "live"}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
	_, changes = save(t, p, func(d *ProjectSettings) { d.Approvers = []string{"carol", "alice", "bob"} })
	if len(changes) != 0 {
		t.Errorf("a reordered set is a change: %+v", changes)
	}
}

// What a cleared field does: one the schema defaults is set to that default
// (so the result is what will be stored), any other is unset.
func TestApplyClears(t *testing.T) {
	p := baseProject(func(p *v1alpha1.Project) {
		p.Spec.Limits.MaxRevisions = i32(7)
		p.Spec.Limits.MaxActiveIntents = 5
		p.Spec.Limits.MaxCostMicroUSD = 20_000_000
		p.Spec.Limits.MaxVerifyFixes = i32(4)
		p.Spec.Limits.MaxVerifications = i32(30)
		p.Spec.Limits.MaxEvaluateCostMicroUSD = i64(3_000_000)
		p.Spec.Checks.Timeout = dur(time.Hour)
		p.Spec.Verification.HandoffMax = dur(time.Hour)
		p.Spec.Verification.MaxToolCalls = i32(120)
		p.Spec.Limits.Plan = v1alpha1.StageLimits{MaxTurns: 20, Timeout: dur(10 * time.Minute)}
	})
	out, changes := save(t, p, func(d *ProjectSettings) {
		d.Limits = ProjectSettingsLimits{}
		d.Checks.TimeoutSeconds = 0
		d.Verification.HandoffMaxSeconds = 0
		d.Verification.MaxToolCalls = 0
		d.Stages.Plan = ProjectSettingsStage{}
	})
	l := out.Spec.Limits
	if l.MaxRevisions == nil || *l.MaxRevisions != 3 || l.MaxCheckFixes == nil || *l.MaxCheckFixes != 2 ||
		l.MaxActiveIntents != 2 || l.MaxCostMicroUSD != 10_000_000 {
		t.Errorf("schema-defaulted limits = %+v, want the defaults written", l)
	}
	if l.MaxVerifyFixes != nil || l.MaxVerifications != nil || l.MaxEvaluateCostMicroUSD != nil {
		t.Errorf("unset limits = %+v, want nil", l)
	}
	if got := out.Spec.Checks.Timeout; got == nil || got.Duration != 30*time.Minute {
		t.Errorf("checks.timeout = %v, want 30m", got)
	}
	if v := out.Spec.Verification; v.HandoffMax != nil || v.MaxToolCalls != nil || !v.Enabled {
		t.Errorf("verification = %+v, want the two limits unset and the rest kept", v)
	}
	if got := out.Spec.Limits.Plan; got.MaxTurns != 0 || got.Timeout != nil {
		t.Errorf("limits.plan = %+v, want unset", got)
	}
	to := map[string]string{}
	for _, c := range changes {
		to[c.Field] = c.To
	}
	want := map[string]string{"limits.maxRevisions": "3", "limits.maxActiveIntents": "2",
		"limits.maxCostMicroUSD": "10000000", "limits.maxVerifyFixes": "", "limits.maxVerifications": "",
		"limits.maxEvaluateCostMicroUSD": "", "checks.timeout": "1800", "verification.handoffMax": "",
		"verification.maxToolCalls": "", "limits.plan.maxTurns": "", "limits.plan.timeout": ""}
	if !reflect.DeepEqual(to, want) {
		t.Errorf("changes land on %v, want %v", to, want)
	}
}

// However a duration was written in the manifest, it is one value: a save
// that leaves it alone sends nothing.
func TestUnchangedDurationSpellingsSendNothing(t *testing.T) {
	for _, spelling := range []string{"10m", "10m0s", "600s", "0h10m", "600000ms"} {
		var p v1alpha1.Project
		manifest := `
metadata: {name: demo, generation: 3}
spec:
  intentRepository: https://github.com/acme/intents
  approvers: {logins: [alice]}
  repositories: [{name: app, url: https://github.com/acme/app}]
  limits: {maxActiveIntents: 2, maxRevisions: 3, maxCheckFixes: 2, maxCostMicroUSD: 10000000,
    build: {timeout: ` + spelling + `}}
  checks: {timeout: ` + spelling + `}
  verification: {handoffMax: ` + spelling + `}
`
		if err := yaml.UnmarshalStrict([]byte(manifest), &p); err != nil {
			t.Fatal(err)
		}
		doc := Read(&p.Spec)
		if doc.Stages.Build.TimeoutSeconds != 600 || doc.Checks.TimeoutSeconds != 600 ||
			doc.Verification.HandoffMaxSeconds != 600 {
			t.Errorf("%s reads as %+v", spelling, doc)
		}
		out := p.DeepCopy()
		changes, err := Apply(out, doc)
		if err != nil || len(changes) != 0 {
			t.Errorf("%s: changes %+v, error %v", spelling, changes, err)
		}
		if got := mergePatch(t, &p, out); got != "{}" {
			t.Errorf("%s: merge patch = %s, want {}", spelling, got)
		}
	}
}

// unchangedVariants are ways a form may post back settings it did not
// change: as served, through JSON, with its lists null or reordered, with
// the fields the schema defaults cleared where they hold the default, and
// with a repository left out.
func unchangedVariants(t *testing.T, r *rand.Rand, p *v1alpha1.Project) []ProjectSettings {
	t.Helper()
	served := Read(&p.Spec)
	var wired ProjectSettings
	raw, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &wired); err != nil {
		t.Fatal(err)
	}
	loose := Read(&p.Spec)
	if len(loose.Checks.Fix) == 0 {
		loose.Checks.Fix = nil
	}
	r.Shuffle(len(loose.Approvers), func(i, j int) {
		loose.Approvers[i], loose.Approvers[j] = loose.Approvers[j], loose.Approvers[i]
	})
	loose.Approvers = append(loose.Approvers, loose.Approvers[0])
	slices.Reverse(loose.Checks.Fix)
	slices.Reverse(loose.Repositories)
	cleared := Read(&p.Spec)
	if cleared.Limits.MaxActiveIntents == v1alpha1.DefaultMaxActiveIntents {
		cleared.Limits.MaxActiveIntents = 0
	}
	if cleared.Limits.MaxCostMicroUSD == v1alpha1.DefaultMaxCostMicroUSD {
		cleared.Limits.MaxCostMicroUSD = 0
	}
	if n := cleared.Limits.MaxRevisions; n != nil && *n == v1alpha1.DefaultMaxRevisions {
		cleared.Limits.MaxRevisions = nil
	}
	if n := cleared.Limits.MaxCheckFixes; n != nil && *n == v1alpha1.DefaultMaxCheckFixes {
		cleared.Limits.MaxCheckFixes = nil
	}
	if cleared.Checks.TimeoutSeconds == 1800 {
		cleared.Checks.TimeoutSeconds = 0
	}
	cleared.Repositories = cleared.Repositories[:r.Intn(len(cleared.Repositories)+1)]
	return []ProjectSettings{served, wired, loose, cleared}
}

// The property the write path rests on: a save that changes no setting
// reports no change, leaves the Project exactly as it was, and its JSON
// merge patch (controller-runtime's, the one the status server sends) is
// empty. Whatever the Project: defaulted or not, optional blocks absent or
// empty, zero-valued pointers, sub-second durations, empty lists.
func TestUnchangedSaveSendsNothing(t *testing.T) {
	prop := func(p *v1alpha1.Project, variant int) bool {
		doc := unchangedVariants(t, rand.New(rand.NewSource(int64(variant))), p)[variant%4]
		out := p.DeepCopy()
		changes, err := Apply(out, doc)
		if err != nil || len(changes) != 0 {
			t.Logf("variant %d: changes %+v, error %v", variant%4, changes, err)
			return false
		}
		if !reflect.DeepEqual(out, p) {
			t.Logf("variant %d: the Project changed: %v", variant%4, differing(t, p, out))
			return false
		}
		if patch := mergePatch(t, p, out); patch != "{}" {
			t.Logf("variant %d: merge patch %s", variant%4, patch)
			return false
		}
		return true
	}
	cfg := quickConfig(2000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(r.Intn(1 << 20))
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// fieldOfLeaf maps a differing JSON leaf of a Project to the editable field
// it belongs to, with the repository it is in; ok is false for a leaf no
// editable field owns.
func fieldOfLeaf(p *v1alpha1.Project, leaf string) (field, item string, ok bool) {
	if m := classLeaf.FindStringSubmatch(leaf); m != nil {
		i, _ := strconv.Atoi(m[1])
		if i < len(p.Spec.Repositories) {
			return FieldRepositoryClass, p.Spec.Repositories[i].Name, true
		}
		return "", "", false
	}
	for _, f := range Fields() {
		if f.Path() == leaf {
			return f.ID, "", true
		}
	}
	return "", "", false
}

// Apply changes nothing but the table's fields, and reports every field it
// changed. The test finds what changed on its own, from the two objects'
// JSON, so a write Apply did not report cannot hide: every differing leaf
// must be an editable field's and must be among the changes, and every
// change must be a differing leaf. A save then lands what it asked for:
// reading the result back gives the settings that were posted.
func TestApplyChangesOnlyTheTableAndReportsAllOfIt(t *testing.T) {
	changed, fields := 0, map[string]bool{}
	prop := func(p *v1alpha1.Project, editSeed int64) bool {
		doc := Read(&p.Spec)
		genEdit(rand.New(rand.NewSource(editSeed)), &doc)
		out := p.DeepCopy()
		changes, err := Apply(out, doc)
		if err != nil {
			t.Logf("Apply: %v", err)
			return false
		}
		reported := map[string]bool{}
		for _, c := range changes {
			reported[c.Field+"|"+c.Item] = true
			fields[c.Field] = true
		}
		if len(changes) > 0 {
			changed++
		}
		found := map[string]bool{}
		for _, leaf := range differing(t, p, out) {
			field, item, ok := fieldOfLeaf(p, leaf)
			if !ok {
				t.Logf("Apply changed %s, which is no editable field", leaf)
				return false
			}
			if !reported[field+"|"+item] {
				t.Logf("Apply changed %s and did not report it (changes %+v)", leaf, changes)
				return false
			}
			found[field+"|"+item] = true
		}
		if !reflect.DeepEqual(found, reported) {
			t.Logf("reported %v, the objects differ in %v", reported, found)
			return false
		}
		again := p.DeepCopy()
		if c, _ := Apply(again, doc); !reflect.DeepEqual(c, changes) || !reflect.DeepEqual(again, out) {
			t.Log("Apply is not deterministic")
			return false
		}
		if c, err := Apply(out.DeepCopy(), doc); err != nil || len(c) != 0 {
			t.Logf("applying the same settings to the result changes it again: %+v, %v", c, err)
			return false
		}
		if got := Changes(&p.Spec, &out.Spec); !reflect.DeepEqual(got, changes) {
			t.Logf("Changes = %+v, Apply reported %+v", got, changes)
			return false
		}
		return true
	}
	cfg := quickConfig(4000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(r.Int63())
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
	// The property says something only of saves that change something, and
	// of the fields the generator reaches: all of them.
	if changed < 3000 {
		t.Errorf("only %d of 4000 generated saves changed anything", changed)
	}
	for _, f := range Fields() {
		if !fields[f.ID] {
			t.Errorf("no generated save changed %s", f.ID)
		}
	}
}

// Owner decision 7: of a repository, a save may change the resource class
// and nothing else. Whatever is posted, the repositories keep their number,
// their order, and each its name, url and preview; an entry differs from
// before only in agentResourceClass, and only when the save named it.
func TestApplyChangesOnlyARepositorysClass(t *testing.T) {
	prop := func(p *v1alpha1.Project, editSeed int64) bool {
		r := rand.New(rand.NewSource(editSeed))
		doc := Read(&p.Spec)
		genEdit(r, &doc)
		r.Shuffle(len(doc.Repositories), func(i, j int) {
			doc.Repositories[i], doc.Repositories[j] = doc.Repositories[j], doc.Repositories[i]
		})
		for i := range doc.Repositories {
			if r.Intn(2) == 0 {
				doc.Repositories[i].AgentResourceClass = pick(r, classPool)
			}
		}
		asked := map[string]string{}
		for _, repo := range doc.Repositories {
			asked[repo.Name] = repo.AgentResourceClass
		}
		out := p.DeepCopy()
		if _, err := Apply(out, doc); err != nil {
			t.Logf("Apply: %v", err)
			return false
		}
		before, after := p.Spec.Repositories, out.Spec.Repositories
		if len(after) != len(before) {
			t.Logf("%d repositories became %d", len(before), len(after))
			return false
		}
		for i := range before {
			want := before[i]
			if class, named := asked[want.Name]; named {
				want.AgentResourceClass = class
			}
			if !reflect.DeepEqual(after[i], want) {
				t.Logf("repository %d = %+v, want %+v", i, after[i], want)
				return false
			}
		}
		return true
	}
	cfg := quickConfig(3000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(r.Int63())
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// Apply never reads or writes anything of the Project but its spec.
func TestApplyLeavesMetadataAndStatusAlone(t *testing.T) {
	p := baseProject()
	out, changes := save(t, p, func(d *ProjectSettings) {
		d.Approvers = []string{"alice", "bob", "carol"}
		d.Checks.Fix = nil
		d.Verification.Enabled = false
	})
	if len(changes) != 3 {
		t.Fatalf("changes = %+v", changes)
	}
	if !reflect.DeepEqual(out.ObjectMeta, p.ObjectMeta) || !reflect.DeepEqual(out.Status, p.Status) {
		t.Error("Apply changed metadata or status")
	}
}

// The document's JSON keys are the page's contract; the schema constant
// moves with them.
func TestDocumentWireShape(t *testing.T) {
	raw, err := json.Marshal(ProjectSettings{})
	if err != nil {
		t.Fatal(err)
	}
	stage := `{"maxTurns":0,"tokenBudget":0,"maxCostMicroUSD":0,"timeoutSeconds":0}`
	want := `{"approvers":null,` +
		`"verification":{"enabled":false,"handoff":"","handoffMaxSeconds":0,"maxToolCalls":0},` +
		`"models":{"plan":"","build":"","evaluate":""},` +
		`"limits":{"maxCostMicroUSD":0,"maxEvaluateCostMicroUSD":0,"maxRevisions":null,"maxCheckFixes":null,` +
		`"maxVerifyFixes":null,"maxVerifications":0,"maxActiveIntents":0},` +
		`"stages":{"plan":` + stage + `,"build":` + stage + `,"revise":` + stage + `,"evaluate":` + stage + `},` +
		`"delegation":{"subagents":false,"workflows":false},` +
		`"checks":{"fix":null,"timeoutSeconds":0,"rerunFailed":false},"repositories":null}`
	if string(raw) != want {
		t.Errorf("document =\n%s\nwant\n%s\n(a changed document needs a new Schema)", raw, want)
	}
	if Schema != 2 {
		t.Errorf("Schema = %d: update the document shape above with it", Schema)
	}
}
