// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// seed keeps every property deterministic in the gate.
const seed = 20261123

// testNow is the clock every test reads.
var testNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func quickConfig(n int, values func([]reflect.Value, *rand.Rand)) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed)), Values: values}
}

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func i32(n int32) *int32 { return &n }

func i64(n int64) *int64 { return &n }

// baseProject is a Project as the API server stores one (schema defaults
// applied) that intent-controller has resolved and published for: alice (id
// 1) and bob (id 2) are approvers with write access.
func baseProject(mut ...func(*v1alpha1.Project)) *v1alpha1.Project {
	resolved := metav1.NewTime(testNow.Add(-time.Minute))
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "maul", Generation: 7,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			Annotations: map[string]string{"meta.helm.sh/release-name": "maul-config"}},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Labels:           v1alpha1.ProjectLabels{Approve: v1alpha1.DefaultApproveLabel},
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"alice", "bob"}},
			Repositories: []v1alpha1.ProjectRepository{
				{Name: "app", URL: "https://github.com/acme/app", AgentResourceClass: "small",
					Preview: &v1alpha1.ProjectRepositoryPreview{
						ProjectPreview: v1alpha1.ProjectPreview{ImageRepository: "registry.example/app", Port: 8080,
							ReadinessPath: "/healthz"}, Path: "/"}},
				{Name: "lib", URL: "https://github.com/acme/lib"},
			},
			Limits: v1alpha1.ProjectLimits{
				MaxActiveIntents: v1alpha1.DefaultMaxActiveIntents,
				MaxRevisions:     i32(v1alpha1.DefaultMaxRevisions),
				MaxCheckFixes:    i32(v1alpha1.DefaultMaxCheckFixes),
				MaxCostMicroUSD:  v1alpha1.DefaultMaxCostMicroUSD,
			},
			Checks:                 v1alpha1.ProjectChecks{Fix: []string{"test", "lint"}, Timeout: dur(30 * time.Minute)},
			RequireRepositoryImage: new(true),
			Verification:           &v1alpha1.ProjectVerification{Enabled: true},
		},
		Status: v1alpha1.ProjectStatus{
			ObservedGeneration: 7,
			ActiveIntents:      1,
			Approvers: &v1alpha1.ProjectApproversStatus{
				Identities: []v1alpha1.ProjectApproverIdentity{
					{Login: "alice", ID: 1, Write: true},
					{Login: "bob", ID: 2, Write: true},
				},
				ResolvedAt: &resolved,
			},
			Admits: baseAdmits(),
		},
	}
	for _, m := range mut {
		m(p)
	}
	return p
}

// baseAdmits is what a claude controller with the evaluator on publishes.
func baseAdmits() *v1alpha1.ProjectAdmits {
	ceiling := func(turns int32, tokens int64, timeout time.Duration) v1alpha1.ProjectStageCeiling {
		return v1alpha1.ProjectStageCeiling{MaxTurns: turns, TokenBudget: tokens, Timeout: dur(timeout)}
	}
	return &v1alpha1.ProjectAdmits{
		Models: []string{"anthropic/claude-a", "anthropic/claude-b", "anthropic/claude-dear"},
		DefaultModels: v1alpha1.ProjectStageModels{Plan: "anthropic/claude-a", Build: "anthropic/claude-a",
			Evaluate: "anthropic/claude-b", CheckReview: "anthropic/claude-b"},
		Stages: v1alpha1.ProjectStageCeilings{
			Plan:        ceiling(40, 200_000, 20*time.Minute),
			Build:       ceiling(150, 800_000, 60*time.Minute),
			Revise:      ceiling(80, 400_000, 45*time.Minute),
			Evaluate:    ceiling(100, 400_000, 20*time.Minute),
			CheckReview: ceiling(6, 100_000, 15*time.Minute),
		},
		MaxEvaluateCostMicroUSD: 8_000_000,
		MaxToolCalls:            250,
		Evaluator:               true,
		ResourceClasses: []v1alpha1.ProjectResourceClass{
			{Name: "small", Requests: v1alpha1.ProjectResourceQuantities{CPU: "1", Memory: "2Gi"},
				Limits: v1alpha1.ProjectResourceQuantities{Memory: "4Gi"}},
			{Name: "large", Requests: v1alpha1.ProjectResourceQuantities{CPU: "4", Memory: "8Gi"},
				Limits: v1alpha1.ProjectResourceQuantities{Memory: "16Gi"}},
		},
	}
}

// openPolicy is DefaultPolicy with every switch on.
func openPolicy() Policy {
	p := DefaultPolicy()
	p.Allow = ProjectSettingsAllow{Approvers: true, DisableEvaluator: true, EmptyChecks: true}
	return p
}

// save applies a document edit to a copy of p, as the write path does: it
// reads the live settings, lets edit change them, and applies the result.
func save(t *testing.T, p *v1alpha1.Project, edit func(*ProjectSettings)) (*v1alpha1.Project, []ProjectSettingsChange) {
	t.Helper()
	doc := Read(&p.Spec)
	edit(&doc)
	out := p.DeepCopy()
	changes, err := Apply(out, doc)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return out, changes
}

// mergePatch is the JSON merge patch the status server would send for
// base -> obj: controller-runtime's own, the one the write path uses.
func mergePatch(t *testing.T, base, obj *v1alpha1.Project) string {
	t.Helper()
	data, err := client.MergeFrom(base).Data(obj)
	if err != nil {
		t.Fatalf("merge patch: %v", err)
	}
	return string(data)
}

// leaves flattens an object's JSON into path -> value. A list of scalars is
// one leaf (a merge patch replaces it whole); a list of objects is indexed.
func leaves(t *testing.T, obj any) map[string]string {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	flatten("", tree, out)
	return out
}

func flatten(path string, node any, out map[string]string) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			flatten(strings.TrimPrefix(path+"."+k, "."), v, out)
		}
	case []any:
		if len(n) > 0 {
			if _, objects := n[0].(map[string]any); objects {
				for i, v := range n {
					flatten(fmt.Sprintf("%s[%d]", path, i), v, out)
				}
				return
			}
		}
		raw, _ := json.Marshal(n)
		out[path] = string(raw)
	default:
		raw, _ := json.Marshal(n)
		out[path] = string(raw)
	}
}

// differing are the leaf paths whose value differs between two objects.
func differing(t *testing.T, a, b any) []string {
	t.Helper()
	la, lb := leaves(t, a), leaves(t, b)
	seen := map[string]bool{}
	for k, v := range la {
		if w, ok := lb[k]; !ok || w != v {
			seen[k] = true
		}
	}
	for k := range lb {
		if _, ok := la[k]; !ok {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var classLeaf = regexp.MustCompile(`^spec\.repositories\[(\d+)\]\.agentResourceClass$`)

// The pools the generators draw from.
var (
	loginPool = []string{"alice", "bob", "carol", "dave", "Erin", "frank-1", "g.h"}
	checkPool = []string{"test", "lint", "e2e", "build", "docs"}
	modelPool = []string{"", "anthropic/claude-a", "anthropic/claude-b", "anthropic/claude-dear", "openai/gpt-x"}
	classPool = []string{"", "small", "large", "huge"}
	repoPool  = []string{"app", "lib", "web", "api", "docs"}
)

func pick[T any](r *rand.Rand, pool []T) T { return pool[r.Intn(len(pool))] }

// subset is a random subset of pool, in a random order, of at least least.
func subset(r *rand.Rand, pool []string, least int) []string {
	out := append([]string{}, pool...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:least+r.Intn(len(out)-least+1)]
}

func genStage(r *rand.Rand) v1alpha1.StageLimits {
	var s v1alpha1.StageLimits
	if r.Intn(2) == 0 {
		s.MaxTurns = int32(r.Intn(200))
	}
	if r.Intn(2) == 0 {
		s.TokenBudget = int64(r.Intn(900_000))
	}
	if r.Intn(3) == 0 {
		s.MaxCostMicroUSD = int64(r.Intn(12_000_000))
	}
	if r.Intn(2) == 0 {
		s.Timeout = dur(time.Duration(60+r.Intn(4000)) * time.Second)
		if r.Intn(8) == 0 {
			s.Timeout.Duration += 500 * time.Millisecond
		}
	}
	return s
}

// genProject is a random Project: defaulted by the schema or not, its
// optional blocks absent, empty or filled, its repositories with and
// without previews and classes, its status resolved and published or not.
func genProject(r *rand.Rand) *v1alpha1.Project {
	p := baseProject()
	p.Generation = 1 + r.Int63n(40)
	s := &p.Spec
	s.Approvers.Logins = subset(r, loginPool, 1)
	s.Repositories = nil
	for _, name := range subset(r, repoPool, 1) {
		repo := v1alpha1.ProjectRepository{Name: name, URL: "https://github.com/acme/" + name,
			AgentResourceClass: pick(r, classPool)}
		if r.Intn(3) == 0 {
			repo.Preview = &v1alpha1.ProjectRepositoryPreview{Path: "/" + name,
				ProjectPreview: v1alpha1.ProjectPreview{ImageRepository: "registry.example/" + name, Port: 8080}}
		}
		s.Repositories = append(s.Repositories, repo)
	}
	s.Limits = v1alpha1.ProjectLimits{Plan: genStage(r), Build: genStage(r), Revise: genStage(r),
		Evaluate: genStage(r), CheckReview: genStage(r)}
	if r.Intn(4) != 0 { // a Project the schema defaulted
		s.Limits.MaxActiveIntents = int32(1 + r.Intn(20))
		s.Limits.MaxRevisions = i32(int32(r.Intn(21)))
		s.Limits.MaxCheckFixes = i32(int32(r.Intn(21)))
		s.Limits.MaxCostMicroUSD = int64(1 + r.Intn(60_000_000))
	}
	if r.Intn(2) == 0 {
		s.Limits.MaxVerifyFixes = i32(int32(r.Intn(11)))
	}
	if r.Intn(2) == 0 {
		s.Limits.MaxVerifications = i32(int32(1 + r.Intn(100)))
	}
	if r.Intn(2) == 0 {
		s.Limits.MaxEvaluateCostMicroUSD = i64(int64(1 + r.Intn(12_000_000)))
	}
	s.Checks = v1alpha1.ProjectChecks{RerunFailed: r.Intn(2) == 0}
	switch r.Intn(3) {
	case 0:
		s.Checks.Fix = []string{}
	case 1:
		s.Checks.Fix = subset(r, checkPool, 1)
	}
	if r.Intn(4) != 0 {
		s.Checks.Timeout = dur(time.Duration(60+r.Intn(20000)) * time.Second)
	}
	switch r.Intn(3) {
	case 0:
		s.Models = nil
	case 1:
		s.Models = &v1alpha1.ProjectModels{}
	default:
		s.Models = &v1alpha1.ProjectModels{Plan: pick(r, modelPool), Build: pick(r, modelPool),
			Evaluate: pick(r, modelPool), CheckReview: pick(r, modelPool)}
	}
	switch r.Intn(3) {
	case 0:
		s.Verification = nil
	case 1:
		s.Verification = &v1alpha1.ProjectVerification{}
	default:
		s.Verification = &v1alpha1.ProjectVerification{Enabled: r.Intn(2) == 0, PostLogs: r.Intn(2) == 0,
			ManualGatesStatus: r.Intn(2) == 0, Handoff: pick(r, []v1alpha1.HandoffMode{"", "gated", "early"})}
		if r.Intn(2) == 0 {
			s.Verification.HandoffMax = dur(time.Duration(600+r.Intn(13000)) * time.Second)
		}
		if r.Intn(2) == 0 {
			s.Verification.MaxToolCalls = i32(int32(50 + r.Intn(251)))
		}
		if r.Intn(4) == 0 {
			s.Verification.Timeout = dur(5 * time.Minute)
		}
	}
	s.Suspend = r.Intn(5) == 0
	if r.Intn(4) == 0 {
		s.Harness = v1alpha1.ProjectHarnessCodex
	}
	genStatus(r, p)
	return p
}

func genStatus(r *rand.Rand, p *v1alpha1.Project) {
	p.Status.Approvers = nil
	if r.Intn(5) != 0 {
		at := metav1.NewTime(testNow.Add(-time.Duration(r.Intn(50)) * time.Minute))
		st := &v1alpha1.ProjectApproversStatus{ResolvedAt: &at}
		for i, l := range loginPool {
			if r.Intn(3) != 0 {
				st.Identities = append(st.Identities, v1alpha1.ProjectApproverIdentity{Login: l,
					ID: int64(r.Intn(2) * (i + 1)), Write: r.Intn(3) != 0})
			}
		}
		if r.Intn(6) == 0 {
			st.ResolvedAt = nil
		}
		p.Status.Approvers = st
	}
	switch r.Intn(5) {
	case 0:
		p.Status.Admits = nil
	case 1:
		p.Status.Admits = &v1alpha1.ProjectAdmits{}
	default:
		a := baseAdmits()
		a.CostBudgets = r.Intn(2) == 0
		if a.CostBudgets && r.Intn(2) == 0 {
			a.MaxRunCostMicroUSD = int64(5_000_000 + r.Intn(30_000_000))
		}
		if r.Intn(3) == 0 {
			a.MaxEvaluateCostMicroUSD = 0
		}
		if a.CostBudgets && r.Intn(2) == 0 {
			a.RunBudgetFloors = []v1alpha1.ProjectRunBudgetFloor{
				{Model: "anthropic/claude-b", EvaluateMicroUSD: 1_600_000, CheckReviewMicroUSD: 400_000},
				{Model: "anthropic/claude-dear", EvaluateMicroUSD: 4_800_000, CheckReviewMicroUSD: 1_200_000},
			}
		}
		p.Status.Admits = a
	}
}

// genEdit is a random edit of a document: a few fields set to new values,
// some sensible, some cleared, some out of every bound.
func genEdit(r *rand.Rand, doc *ProjectSettings) {
	num := func() int64 {
		return pick(r, []int64{0, 0, 1, 2, 3, 5, 7, 10, 12, 20, 30, 45, 60, 99, 100, 150, 300, 600, 900, 1800, 3600,
			7200, 100_000, 400_000, 1_000_000, 2_000_000, 6_000_000, 9_000_000, 25_000_000, 40_000_000})
	}
	ptr := func() *int32 {
		if r.Intn(4) == 0 {
			return nil
		}
		return i32(int32(r.Intn(25)))
	}
	stage := func(s *ProjectSettingsStage) {
		switch r.Intn(4) {
		case 0:
			s.MaxTurns = int32(num() % 400)
		case 1:
			s.TokenBudget = num()
		case 2:
			s.MaxCostMicroUSD = num()
		default:
			s.TimeoutSeconds = num() % 20000
		}
	}
	edits := []func(){
		func() { doc.Approvers = subset(r, loginPool, 0) },
		func() { doc.Verification.Enabled = !doc.Verification.Enabled },
		func() { doc.Verification.Handoff = pick(r, []string{"", "gated", "early"}) },
		func() { doc.Verification.HandoffMaxSeconds = num() % 20000 },
		func() { doc.Verification.MaxToolCalls = int32(num() % 400) },
		func() { doc.Models.Plan = pick(r, modelPool) },
		func() { doc.Models.Build = pick(r, modelPool) },
		func() { doc.Models.Evaluate = pick(r, modelPool) },
		func() { doc.Models.CheckReview = pick(r, modelPool) },
		func() { doc.Limits.MaxCostMicroUSD = num() },
		func() { doc.Limits.MaxEvaluateCostMicroUSD = num() },
		func() { doc.Limits.MaxRevisions = ptr() },
		func() { doc.Limits.MaxCheckFixes = ptr() },
		func() { doc.Limits.MaxVerifyFixes = ptr() },
		func() { doc.Limits.MaxVerifications = int32(num() % 120) },
		func() { doc.Limits.MaxActiveIntents = int32(num() % 25) },
		func() { stage(&doc.Stages.Plan) },
		func() { stage(&doc.Stages.Build) },
		func() { stage(&doc.Stages.Revise) },
		func() { stage(&doc.Stages.Evaluate) },
		func() { stage(&doc.Stages.Evaluate) },
		func() { stage(&doc.Stages.CheckReview) },
		func() { doc.Checks.Fix = subset(r, checkPool, 0) },
		func() { doc.Checks.TimeoutSeconds = num() % 30000 },
		func() { doc.Checks.RerunFailed = !doc.Checks.RerunFailed },
		func() {
			if n := len(doc.Repositories); n > 0 {
				doc.Repositories[r.Intn(n)].AgentResourceClass = pick(r, classPool)
			}
		},
		func() {
			if n := len(doc.Repositories); n > 1 { // a save may leave a repository out
				i := r.Intn(n)
				doc.Repositories = append(doc.Repositories[:i:i], doc.Repositories[i+1:]...)
			}
		},
	}
	for range 1 + r.Intn(4) {
		pick(r, edits)()
	}
}

// genPolicy is a random valid Policy.
func genPolicy(r *rand.Rand) Policy {
	p := DefaultPolicy()
	p.Allow = ProjectSettingsAllow{Approvers: r.Intn(2) == 0, DisableEvaluator: r.Intn(3) == 0,
		EmptyChecks: r.Intn(2) == 0}
	for _, g := range Groups() {
		if r.Intn(8) == 0 {
			p.Locked = append(p.Locked, g)
		}
	}
	p.Ceilings = Ceilings{MaxCostMicroUSD: int64(1 + r.Intn(40_000_000)), MaxRevisions: int32(r.Intn(21)),
		MaxCheckFixes: int32(r.Intn(21)), MaxVerifyFixes: int32(r.Intn(11)),
		MaxVerifications: int32(10 + r.Intn(91)), MaxActiveIntents: int32(1 + r.Intn(20))}
	if r.Intn(4) == 0 {
		p.Floors = Floors{}
	} else {
		p.Floors.MaxVerifications = int32(1 + r.Intn(10))
		p.Floors.EvaluateMaxTurns = int32(r.Intn(120))
	}
	return p
}

// A generator that drifts from what the schema admits would test nothing,
// so the policies it makes must be valid ones.
func TestGeneratedPoliciesAreValid(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	for range 200 {
		if err := genPolicy(r).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
