// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// The settings policy's refusals, each its own words, so a case cannot
// pass on the wrong rule.
const (
	psPinned         = "the page changes no key of spec"
	psRepoList       = "the page may not add, drop or reorder repositories"
	psRepoKeys       = "the page may change only a repository's agentResourceClass"
	psClass          = "a resource class the operator does not define"
	psMetadata       = "the page may not change the Project's labels, finalizers or owner references"
	psAnnotations    = "the page may change no annotation but maul.run/settings-edited"
	psEditSize       = "maul.run/settings-edited is over 1 KiB"
	psApproversOff   = "the operator has not let the page change the approvers"
	psApproversOne   = "the page may add or remove one approver at a time"
	psApproversCase  = "two approvers differ only in case"
	psEvaluatorOff   = "may not turn verification off"
	psHandoff        = "may not move the hand-off earlier"
	psChecksEmpty    = "may not empty checks.fix"
	psModel          = "a model intent-controller does not admit"
	psLockedPrefix   = "the operator locked "
	psCeilingSuffix  = " above the operator's ceiling"
	psFloorSuffix    = " below the operator's floor"
	psProjectEditor  = "alice"
	psProjectEditorN = 1
)

// settingsPhaseA is the policy the first half runs under: every switch off,
// active intents locked.
const settingsPhaseA = `
statusServer:
    projectSettings:
        locked: [activeIntents]
`

// settingsPhaseB turns every switch on and locks the models instead.
const settingsPhaseB = `
statusServer:
    projectSettings:
        allow: {approvers: true, disableEvaluator: true, emptyChecks: true}
        locked: [models]
`

// TestProjectSettingsPolicyAgainstAPIServer applies the chart rendered
// with the Project settings page on to a real API server and drives the
// settings admission policy as the status server's own ServiceAccount, with
// the grant the chart gives it: the server's real saves pass; every field
// outside the editable part, a foreign annotation, a locked group, a raise
// past a ceiling, a drop under a floor, an approver change without the
// switch or of two logins at once, verification off and checks emptied
// without theirs, and a model or class the controller does not admit are
// refused; a Project the controller has not yet written status on refuses a
// model or class choice and still takes an unrelated edit; RBAC refuses an
// update and the status subresource; an admin is not judged at all. Each
// case's verdict is also held to projectsettings.Check under the policy the
// server reads from the same render, and a seeded run of random edits holds
// the two together.
func TestProjectSettingsPolicyAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	argsA := append(slices.Clone(projectSettingsArgs), valuesFile(t, settingsPhaseA)...)
	seen := applyChartRender(t, admin, argsA)
	for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		if !seen[kind+"/"+settingsPolicy] {
			t.Fatalf("the render has no %s %s", kind, settingsPolicy)
		}
	}
	sa := impersonate(t, cfg, settingsSA)
	rbactest.WaitAllowed(t, admin, rbactest.Review(settingsSA, nil, authorizationv1.ResourceAttributes{
		Namespace: egressRelease, Group: v1alpha1.GroupVersion.Group, Resource: "projects", Verb: "patch",
	}))
	e := &settingsEnv{t: t, admin: admin, sa: sa, pol: renderedPolicy(t, renderObjects(t, argsA))}
	e.project("alpha", withStatus)
	e.project("high", withStatus, func(p *v1alpha1.Project) {
		p.Spec.Limits.MaxRevisions = new(int32(15))
		p.Spec.Verification.MaxToolCalls = new(int32(60))
		p.Spec.Verification.Handoff = v1alpha1.HandoffEarly
		p.Spec.Verification.PostLogs = true
		p.Spec.Limits.Evaluate.MaxTurns = 20
		p.Spec.Limits.CheckReview.MaxTurns = 4
	})
	e.project("single", withStatus, func(p *v1alpha1.Project) {
		p.Spec.Repositories = p.Spec.Repositories[1:]
		p.Spec.Preview = &v1alpha1.ProjectPreview{ImageRepository: "registry.example/acme/lib", Port: 80,
			ReadinessPath: "/"}
	})
	e.project("bare", func(p *v1alpha1.Project) {
		p.Spec.Repositories = p.Spec.Repositories[1:]
		p.Spec.Repositories[0].AgentResourceClass = "small"
		p.Spec.Models = &v1alpha1.ProjectModels{Plan: "anthropic/claude-a"}
	})
	e.project("blocks", withStatus, func(p *v1alpha1.Project) {
		p.Spec.Verification, p.Spec.Models = nil, nil
	})
	e.project("save", withStatus)

	// The canary: until the policy refuses, an admitted case proves
	// nothing.
	waitFor(t, func() error {
		err := e.tryAlpha(func(p *v1alpha1.Project) { p.Spec.Suspend = true })
		if settingsDenied(err, psPinned) {
			return nil
		}
		return &waitError{err: err, want: psPinned}
	})

	t.Run("saves", e.checkSaves)
	t.Run("pinned", e.checkPinned)
	t.Run("metadata", e.checkMetadata)
	t.Run("rules", e.checkRules)
	t.Run("without status", e.checkWithoutStatus)
	t.Run("denial", e.checkDenialShape)
	t.Run("rbac", e.checkSettingsRBAC)
	t.Run("admin", e.checkAdminUnjudged)
	t.Run("property", func(t *testing.T) { e.checkProperty(t, 1) })

	// The second half: every switch on, the models locked instead.
	argsB := append(slices.Clone(projectSettingsArgs), valuesFile(t, settingsPhaseB)...)
	e.replacePolicy(argsB)
	waitFor(t, func() error {
		err := e.tryAlpha(func(p *v1alpha1.Project) { modelsOf(p).Plan = "anthropic/claude-b" })
		if settingsDenied(err, psLockedPrefix+"models") {
			return nil
		}
		return &waitError{err: err, want: psLockedPrefix + "models"}
	})
	t.Run("switches", e.checkSwitches)
	t.Run("property with the switches", func(t *testing.T) { e.checkProperty(t, 2) })

	// The third: every group locked, so each lock's own expression is
	// evaluated (one that failed to compile would refuse every save).
	argsC := append(slices.Clone(projectSettingsArgs), valuesFile(t, settingsPhaseC)...)
	e.replacePolicy(argsC)
	waitFor(t, func() error {
		err := e.tryAlpha(func(p *v1alpha1.Project) { p.Spec.Checks.RerunFailed = true })
		if settingsDenied(err, psLockedPrefix+"checks") {
			return nil
		}
		return &waitError{err: err, want: psLockedPrefix + "checks"}
	})
	t.Run("locks", e.checkLocks)
}

// settingsPhaseC locks every group, every switch on.
const settingsPhaseC = `
statusServer:
    projectSettings:
        allow: {approvers: true, disableEvaluator: true, emptyChecks: true}
        locked: [approvers, evaluator, handoff, models, cost, rounds, activeIntents, stages, checks, resources]
`

// checkLocks: a change to each group is refused naming that group, and
// only the page's own annotation may still change.
func (e *settingsEnv) checkLocks(t *testing.T) {
	limits := func(f func(*v1alpha1.ProjectLimits)) func(*v1alpha1.Project) {
		return func(p *v1alpha1.Project) { f(&p.Spec.Limits) }
	}
	e.run(t, []settingsCase{
		{"approvers", "alpha", func(p *v1alpha1.Project) { p.Spec.Approvers.Logins = []string{"alice"} },
			psLockedPrefix + "approvers", checkSame},
		{"evaluator", "alpha", func(p *v1alpha1.Project) { verifyOf(p).MaxToolCalls = new(int32(200)) },
			psLockedPrefix + "evaluator", checkSame},
		{"handoff", "alpha", func(p *v1alpha1.Project) { verifyOf(p).HandoffMax = minutes(90) },
			psLockedPrefix + "handoff", checkSame},
		{"models", "alpha", func(p *v1alpha1.Project) { modelsOf(p).Build = "anthropic/claude-b" },
			psLockedPrefix + "models", checkSame},
		{"cost", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.MaxEvaluateCostMicroUSD = new(int64(7_000_000)) }),
			psLockedPrefix + "cost", checkSame},
		{"rounds", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.MaxVerifyFixes = new(int32(1)) }),
			psLockedPrefix + "rounds", checkSame},
		{"activeIntents", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.MaxActiveIntents = 1 }),
			psLockedPrefix + "activeIntents", checkSame},
		{"stages", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.Revise.TokenBudget = 100_000 }),
			psLockedPrefix + "stages", checkSame},
		{"checks", "alpha", func(p *v1alpha1.Project) { p.Spec.Checks.Fix = []string{"test", "lint", "e2e"} },
			psLockedPrefix + "checks", checkSame},
		{"resources", "alpha", func(p *v1alpha1.Project) { p.Spec.Repositories[1].AgentResourceClass = "large" },
			psLockedPrefix + "resources", checkSame},
		{"its own annotation", "alpha", func(p *v1alpha1.Project) { p.Annotations[projectsettings.EditedAnnotation] = "{}" },
			"", checkBlind},
	})
}

// settingsEnv is the API server, its two clients and the policy the status
// server reads from the same render as the admission policy.
type settingsEnv struct {
	t     *testing.T
	admin client.Client
	sa    client.Client
	pol   projectsettings.Policy
}

// impersonate is a client acting as user.
func impersonate(t *testing.T, cfg *rest.Config, user string) client.Client {
	t.Helper()
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: user}
	cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client as %s: %v", user, err)
	}
	return cl
}

// settingsAdmits is what intent-controller would publish for the Projects
// here.
func settingsAdmits() *v1alpha1.ProjectAdmits {
	ceiling := func(turns int32, tokens int64, timeout time.Duration) v1alpha1.ProjectStageCeiling {
		return v1alpha1.ProjectStageCeiling{MaxTurns: turns, TokenBudget: tokens,
			Timeout: &metav1.Duration{Duration: timeout}}
	}
	return &v1alpha1.ProjectAdmits{
		Models: []string{"anthropic/claude-a", "anthropic/claude-b"},
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
			{Name: "small", Requests: v1alpha1.ProjectResourceQuantities{CPU: "1", Memory: "2Gi"}},
			{Name: "large", Requests: v1alpha1.ProjectResourceQuantities{CPU: "4", Memory: "8Gi"}},
		},
	}
}

// withStatus marks a Project for the status intent-controller would have
// written: the approvers resolved (alice is id 1 with write access) and
// what it admits.
func withStatus(p *v1alpha1.Project) {
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations["test/status"] = "yes"
}

// project creates a Project as an admin, then its status where withStatus
// asked for it.
func (e *settingsEnv) project(name string, mut ...func(*v1alpha1.Project)) {
	t := e.t
	t.Helper()
	ctx := t.Context()
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressRelease,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			Annotations: map[string]string{"meta.helm.sh/release-name": "maul-config"}},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents-" + name,
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"alice", "bob"}},
			Repositories: []v1alpha1.ProjectRepository{
				{Name: "app", URL: "https://github.com/acme/app", AgentResourceClass: "small",
					Preview: &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
						ImageRepository: "registry.example/acme/app", Port: 8080, ReadinessPath: "/healthz"}}},
				{Name: "lib", URL: "https://github.com/acme/lib"},
			},
			Checks:       v1alpha1.ProjectChecks{Fix: []string{"test", "lint"}},
			Verification: &v1alpha1.ProjectVerification{Enabled: true},
		},
	}
	for _, m := range mut {
		m(p)
	}
	status := p.Annotations["test/status"] == "yes"
	delete(p.Annotations, "test/status")
	if err := e.admin.Create(ctx, p); err != nil {
		t.Fatalf("create Project %s: %v", name, err)
	}
	if !status {
		return
	}
	resolved := metav1.NewTime(time.Now().Add(-time.Minute))
	p.Status = v1alpha1.ProjectStatus{
		ObservedGeneration: p.Generation,
		Approvers: &v1alpha1.ProjectApproversStatus{ResolvedAt: &resolved, Identities: []v1alpha1.ProjectApproverIdentity{
			{Login: "alice", ID: psProjectEditorN, Write: true}, {Login: "bob", ID: 2, Write: true}}},
		Admits: settingsAdmits(),
	}
	if err := e.admin.Status().Update(ctx, p); err != nil {
		t.Fatalf("status of Project %s: %v", name, err)
	}
}

// get is the live Project, read as an admin.
func (e *settingsEnv) get(t *testing.T, name string) *v1alpha1.Project {
	t.Helper()
	var p v1alpha1.Project
	if err := e.admin.Get(t.Context(), client.ObjectKey{Namespace: egressRelease, Name: name}, &p); err != nil {
		t.Fatalf("get Project %s: %v", name, err)
	}
	return &p
}

// tryAlpha patches Project alpha as the status server would (a JSON merge patch of
// what edit changed), in a dry run: admission runs, nothing is stored.
func (e *settingsEnv) tryAlpha(edit func(*v1alpha1.Project)) error {
	return e.tryAs(e.sa, e.get(e.t, "alpha"), edit)
}

func (e *settingsEnv) tryAs(c client.Client, live *v1alpha1.Project, edit func(*v1alpha1.Project)) error {
	updated := live.DeepCopy()
	edit(updated)
	return c.Patch(e.t.Context(), updated, client.MergeFrom(live), client.DryRunAll)
}

// settingsDenied reports an admission denial naming want.
func settingsDenied(err error, want string) bool {
	return err != nil && strings.Contains(err.Error(), "denied request") && strings.Contains(err.Error(), want)
}

// checkMode is how a case's verdict is held to projectsettings.Check.
type checkMode int

const (
	// checkSame: Check refuses exactly when the policy does.
	checkSame checkMode = iota
	// checkBlind: the change is outside what Check judges (no field of
	// the table), so only the policy refuses it.
	checkBlind
)

// settingsCase is one patch as the status server and its verdict.
type settingsCase struct {
	name    string
	project string
	edit    func(*v1alpha1.Project)
	// denied is the refusal's words, "" for admitted.
	denied string
	mode   checkMode
}

func (e *settingsEnv) run(t *testing.T, cases []settingsCase) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live := e.get(t, c.project)
			err := e.tryAs(e.sa, live, c.edit)
			switch {
			case c.denied == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.denied != "" && !settingsDenied(err, c.denied):
				t.Fatalf("got %v, want a denial naming %q", err, c.denied)
			}
			updated := live.DeepCopy()
			c.edit(updated)
			refusals := projectsettings.Check(live, updated, e.pol, psProjectEditor)
			switch {
			case c.mode == checkBlind && len(refusals) != 0:
				t.Errorf("Check judges a change outside the table: %+v", refusals)
			case c.mode == checkSame && (len(refusals) > 0) != (c.denied != ""):
				t.Errorf("the policy and Check disagree: policy %v, Check %+v", err, refusals)
			}
		})
	}
}

func modelsOf(p *v1alpha1.Project) *v1alpha1.ProjectModels {
	if p.Spec.Models == nil {
		p.Spec.Models = &v1alpha1.ProjectModels{}
	}
	return p.Spec.Models
}

func verifyOf(p *v1alpha1.Project) *v1alpha1.ProjectVerification {
	if p.Spec.Verification == nil {
		p.Spec.Verification = &v1alpha1.ProjectVerification{}
	}
	return p.Spec.Verification
}

func minutes(n int) *metav1.Duration {
	return &metav1.Duration{Duration: time.Duration(n) * time.Minute}
}

// save is one save through the status server's own write path:
// projectsettings.Decide on the live Project, confirmed, then the patch it
// sends, as the ServiceAccount, for real.
func (e *settingsEnv) save(t *testing.T, name string, edit func(*projectsettings.ProjectSettings)) *v1alpha1.Project {
	t.Helper()
	live := e.get(t, name)
	doc := projectsettings.Read(&live.Spec)
	edit(&doc)
	req := projectsettings.ProjectSettingsRequest{Generation: live.Generation, Schema: projectsettings.Schema,
		Settings: doc, Confirm: []string{}}
	d := projectsettings.Decide(live, req, e.pol, psProjectEditorN, time.Now())
	if d.Result.Reason == projectsettings.ResultConfirmRequired {
		for _, c := range d.Result.Confirm {
			req.Confirm = append(req.Confirm, c.Key)
		}
		d = projectsettings.Decide(live, req, e.pol, psProjectEditorN, time.Now())
	}
	if d.Patched == nil {
		t.Fatalf("Decide made no save: %q %+v", d.Result.Reason, d.Result.Refusals)
	}
	if err := e.sa.Patch(t.Context(), d.Patched, client.MergeFromWithOptions(live, client.MergeFromWithOptimisticLock{}),
		client.FieldOwner(projectsettings.FieldManager)); err != nil {
		t.Fatalf("the server's save was refused: %v", err)
	}
	stored := e.get(t, name)
	if got := projectsettings.Read(&stored.Spec); !equalDocs(got, projectsettings.Read(&d.Patched.Spec)) {
		t.Errorf("stored %+v, saved %+v", got, projectsettings.Read(&d.Patched.Spec))
	}
	return stored
}

func equalDocs(a, b projectsettings.ProjectSettings) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// checkSaves: the status server's real saves pass, a save of every field
// and a class change on a previewed repository included, as do one that
// creates the verification and models blocks and one that clears fields
// back to unset.
func (e *settingsEnv) checkSaves(t *testing.T) {
	t.Run("every field", func(t *testing.T) {
		stored := e.save(t, "save", func(d *projectsettings.ProjectSettings) {
			d.Verification.Handoff = "gated"
			d.Verification.HandoffMaxSeconds = 45 * 60
			d.Verification.MaxToolCalls = 200
			d.Models = projectsettings.ProjectSettingsModels{Plan: "anthropic/claude-b", Build: "anthropic/claude-b",
				Evaluate: "anthropic/claude-a"}
			d.Limits.MaxCostMicroUSD = 20_000_000
			d.Limits.MaxEvaluateCostMicroUSD = 7_000_000
			d.Limits.MaxRevisions = new(int32(5))
			d.Limits.MaxCheckFixes = new(int32(4))
			d.Limits.MaxVerifyFixes = new(int32(3))
			d.Limits.MaxVerifications = 30
			stage := func(turns int32, tokens int64, timeout int64) projectsettings.ProjectSettingsStage {
				return projectsettings.ProjectSettingsStage{MaxTurns: turns, TokenBudget: tokens,
					MaxCostMicroUSD: 1_000_000, TimeoutSeconds: timeout * 60}
			}
			d.Stages = projectsettings.ProjectSettingsStages{Plan: stage(30, 150_000, 15), Build: stage(100, 600_000, 50),
				Revise: stage(60, 300_000, 40), Evaluate: stage(50, 200_000, 15)}
			d.Checks = projectsettings.ProjectSettingsChecks{Fix: []string{"test", "lint", "e2e"}, TimeoutSeconds: 45 * 60,
				RerunFailed: true}
			d.Repositories = []projectsettings.ProjectSettingsRepository{{Name: "app", AgentResourceClass: "large"},
				{Name: "lib", AgentResourceClass: "small"}}
		})
		if p := stored.Spec.Repositories[0].Preview; p == nil || p.Port != 8080 {
			t.Errorf("the previewed repository lost its preview: %+v", p)
		}
	})
	t.Run("new blocks", func(t *testing.T) {
		stored := e.save(t, "blocks", func(d *projectsettings.ProjectSettings) {
			d.Models.Plan = "anthropic/claude-a"
			d.Verification.MaxToolCalls = 150
			d.Verification.Handoff = "gated"
		})
		if stored.Spec.Models == nil || stored.Spec.Verification == nil {
			t.Errorf("the blocks were not created: %+v", stored.Spec)
		}
	})
	t.Run("cleared", func(t *testing.T) {
		e.save(t, "save", func(d *projectsettings.ProjectSettings) {
			d.Verification.HandoffMaxSeconds, d.Verification.MaxToolCalls = 0, 0
			d.Models = projectsettings.ProjectSettingsModels{}
			d.Limits.MaxEvaluateCostMicroUSD, d.Limits.MaxVerifications = 0, 0
			d.Limits.MaxRevisions, d.Limits.MaxVerifyFixes = nil, nil
			d.Stages = projectsettings.ProjectSettingsStages{}
			d.Checks.TimeoutSeconds = 0
			d.Repositories = []projectsettings.ProjectSettingsRepository{{Name: "app"}, {Name: "lib"}}
		})
	})
}

// checkPinned: no field outside the page's editable part may change,
// whichever level of the spec it is at, and the repository list keeps its
// entries, their order and every key but the class.
func (e *settingsEnv) checkPinned(t *testing.T) {
	lib := func(p *v1alpha1.Project) *v1alpha1.ProjectRepository { return &p.Spec.Repositories[1] }
	e.run(t, []settingsCase{
		{"labels", "alpha", func(p *v1alpha1.Project) { p.Spec.Labels.Trigger = "maul:other" }, psPinned, checkBlind},
		{"suspend", "alpha", func(p *v1alpha1.Project) { p.Spec.Suspend = true }, psPinned, checkBlind},
		{"requireRepositoryImage", "alpha", func(p *v1alpha1.Project) { p.Spec.RequireRepositoryImage = new(false) },
			psPinned, checkBlind},
		{"harness", "alpha", func(p *v1alpha1.Project) { p.Spec.Harness = "codex" }, psPinned, checkBlind},
		{"surfaces", "bare", func(p *v1alpha1.Project) {
			p.Spec.Surfaces = &v1alpha1.ProjectSurfaces{Library: &v1alpha1.LibrarySurface{Language: v1alpha1.LibraryLanguageGo}}
		}, psPinned, checkBlind},
		{"preview", "bare", func(p *v1alpha1.Project) {
			p.Spec.Preview = &v1alpha1.ProjectPreview{ImageRepository: "registry.example/acme/x", Port: 80, ReadinessPath: "/"}
		}, psPinned, checkBlind},
		{"verification.manualGatesStatus", "alpha", func(p *v1alpha1.Project) { verifyOf(p).ManualGatesStatus = true },
			psPinned, checkBlind},
		{"verification.postLogs", "alpha", func(p *v1alpha1.Project) { verifyOf(p).PostLogs = true }, psPinned, checkBlind},
		{"verification.timeout", "alpha", func(p *v1alpha1.Project) { verifyOf(p).Timeout = minutes(5) }, psPinned,
			checkBlind},
		{"models.checkReview", "alpha", func(p *v1alpha1.Project) { modelsOf(p).CheckReview = "anthropic/claude-a" },
			psPinned, checkBlind},
		{"limits.checkReview", "alpha", func(p *v1alpha1.Project) { p.Spec.Limits.CheckReview.MaxTurns = 3 }, psPinned,
			checkBlind},
		// Removing a key is a change too: an operator's postLogs, the
		// check reviewer's turns and the spec's own preview gone.
		{"a pinned key removed", "high", func(p *v1alpha1.Project) { verifyOf(p).PostLogs = false }, psPinned, checkBlind},
		{"a pinned stage removed", "high", func(p *v1alpha1.Project) { p.Spec.Limits.CheckReview = v1alpha1.StageLimits{} },
			psPinned, checkBlind},
		{"a pinned block removed", "single", func(p *v1alpha1.Project) { p.Spec.Preview = nil }, psPinned, checkBlind},
		{"a repository's url", "alpha", func(p *v1alpha1.Project) { lib(p).URL = "https://github.com/acme/other" },
			psRepoKeys, checkBlind},
		{"a repository's new preview", "alpha", func(p *v1alpha1.Project) {
			lib(p).Preview = &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
				ImageRepository: "registry.example/acme/lib", Port: 8080, ReadinessPath: "/healthz"}, Path: "/lib"}
		}, psRepoKeys, checkBlind},
		{"a preview's port beside a class", "alpha", func(p *v1alpha1.Project) {
			p.Spec.Repositories[0].Preview.Port = 9090
			p.Spec.Repositories[0].AgentResourceClass = "large"
		}, psRepoKeys, checkBlind},
		{"a repository's preview dropped beside a class", "alpha", func(p *v1alpha1.Project) {
			p.Spec.Repositories[0].Preview = nil
			p.Spec.Repositories[0].AgentResourceClass = "large"
		}, psRepoKeys, checkBlind},
		{"repositories reordered", "alpha", func(p *v1alpha1.Project) {
			r := p.Spec.Repositories
			p.Spec.Repositories = []v1alpha1.ProjectRepository{r[1], r[0]}
		}, psRepoList, checkBlind},
		{"a repository dropped", "alpha", func(p *v1alpha1.Project) { p.Spec.Repositories = p.Spec.Repositories[:1] },
			psRepoList, checkBlind},
		{"a repository added", "alpha", func(p *v1alpha1.Project) {
			p.Spec.Repositories = append(p.Spec.Repositories,
				v1alpha1.ProjectRepository{Name: "web", URL: "https://github.com/acme/web"})
		}, psRepoList, checkBlind},
	})
}

// checkMetadata: the page writes one annotation of its own, bounded, and
// nothing else of the Project's metadata.
func (e *settingsEnv) checkMetadata(t *testing.T) {
	ann := func(k, v string) func(*v1alpha1.Project) {
		return func(p *v1alpha1.Project) { p.Annotations[k] = v }
	}
	e.run(t, []settingsCase{
		{"a foreign annotation", "alpha", ann("example.com/x", "y"), psAnnotations, checkBlind},
		{"Helm's annotation changed", "alpha", ann("meta.helm.sh/release-name", "other"), psAnnotations, checkBlind},
		{"Helm's annotation removed", "alpha", func(p *v1alpha1.Project) {
			delete(p.Annotations, "meta.helm.sh/release-name")
		}, psAnnotations, checkBlind},
		{"its own annotation", "alpha",
			ann(projectsettings.EditedAnnotation, strings.Repeat("x", projectsettings.MaxEditBytes)),
			"", checkBlind},
		{"its own annotation over 1 KiB", "alpha",
			ann(projectsettings.EditedAnnotation, strings.Repeat("x", projectsettings.MaxEditBytes+1)), psEditSize, checkBlind},
		{"a label", "alpha", func(p *v1alpha1.Project) { p.Labels["example.com/x"] = "y" }, psMetadata, checkBlind},
		{"a finalizer", "alpha", func(p *v1alpha1.Project) { p.Finalizers = append(p.Finalizers, "example.com/hold") },
			psMetadata, checkBlind},
		{"an owner", "alpha", func(p *v1alpha1.Project) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x",
				UID: "4f2d9a3c-0000-4000-8000-000000000000"}}
		}, psMetadata, checkBlind},
	})
}

// checkRules: the operator's locks, ceilings, floors and switches, and the
// controller's admitted models and classes, each judged only where the
// value moved the wrong way.
func (e *settingsEnv) checkRules(t *testing.T) {
	limits := func(f func(*v1alpha1.ProjectLimits)) func(*v1alpha1.Project) {
		return func(p *v1alpha1.Project) { f(&p.Spec.Limits) }
	}
	ver := func(f func(*v1alpha1.ProjectVerification)) func(*v1alpha1.Project) {
		return func(p *v1alpha1.Project) { f(verifyOf(p)) }
	}
	e.run(t, []settingsCase{
		{"a locked group", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.MaxActiveIntents = 3 }),
			psLockedPrefix + "activeIntents", checkSame},
		{"past a ceiling", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.MaxCostMicroUSD = 30_000_000 }),
			projectsettings.FieldMaxCost + psCeilingSuffix, checkSame},
		{"at a ceiling", "alpha", limits(func(l *v1alpha1.ProjectLimits) { l.MaxCostMicroUSD = 25_000_000 }), "", checkSame},
		{"a round limit past its ceiling", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.MaxRevisions = new(int32(11)) }),
			projectsettings.FieldMaxRevisions + psCeilingSuffix, checkSame},
		{"an operator's higher value lowered", "high",
			limits(func(l *v1alpha1.ProjectLimits) { l.MaxRevisions = new(int32(12)) }), "", checkSame},
		{"an operator's higher value raised", "high",
			limits(func(l *v1alpha1.ProjectLimits) { l.MaxRevisions = new(int32(16)) }),
			projectsettings.FieldMaxRevisions + psCeilingSuffix, checkSame},
		{"cleared to a default under the ceiling", "high",
			limits(func(l *v1alpha1.ProjectLimits) { l.MaxRevisions = nil }), "", checkSame},
		{"evaluate turns under the floor", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.MaxTurns = 30 }),
			projectsettings.StageField("evaluate", "maxTurns") + psFloorSuffix, checkSame},
		{"evaluate tokens under the floor", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.TokenBudget = 50_000 }),
			projectsettings.StageField("evaluate", "tokenBudget") + psFloorSuffix, checkSame},
		{"evaluate timeout under the floor", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.Timeout = minutes(5) }),
			projectsettings.StageField("evaluate", "timeout") + psFloorSuffix, checkSame},
		{"evaluate timeout at the floor", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.Timeout = minutes(10) }), "", checkSame},
		{"evaluation cost under the floor", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.MaxEvaluateCostMicroUSD = new(int64(1_000_000)) }),
			projectsettings.FieldMaxEvaluateCost + psFloorSuffix, checkSame},
		{"verifications under the floor", "alpha",
			limits(func(l *v1alpha1.ProjectLimits) { l.MaxVerifications = new(int32(2)) }),
			projectsettings.FieldMaxVerifications + psFloorSuffix, checkSame},
		{"tool calls under the floor", "alpha",
			ver(func(v *v1alpha1.ProjectVerification) { v.MaxToolCalls = new(int32(80)) }),
			projectsettings.FieldMaxToolCalls + psFloorSuffix, checkSame},
		{"hand-off wait under the floor", "alpha", ver(func(v *v1alpha1.ProjectVerification) { v.HandoffMax = minutes(20) }),
			projectsettings.FieldHandoffMax + psFloorSuffix, checkSame},
		{"under the floor, raised toward it", "high",
			ver(func(v *v1alpha1.ProjectVerification) { v.MaxToolCalls = new(int32(70)) }), "", checkSame},
		{"under the floor, lowered", "high",
			ver(func(v *v1alpha1.ProjectVerification) { v.MaxToolCalls = new(int32(50)) }),
			projectsettings.FieldMaxToolCalls + psFloorSuffix, checkSame},
		{"evaluate turns under the floor, raised", "high",
			limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.MaxTurns = 30 }), "", checkSame},
		{"evaluate turns under the floor, lowered", "high",
			limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.MaxTurns = 10 }),
			projectsettings.StageField("evaluate", "maxTurns") + psFloorSuffix, checkSame},
		{"evaluate turns cleared", "high", limits(func(l *v1alpha1.ProjectLimits) { l.Evaluate.MaxTurns = 0 }), "",
			checkSame},
		{"an approver added without the switch", "alpha", func(p *v1alpha1.Project) {
			p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, "carol")
		}, psApproversOff, checkSame},
		{"the approvers reordered", "alpha", func(p *v1alpha1.Project) {
			p.Spec.Approvers.Logins = []string{"bob", "alice"}
		}, "", checkSame},
		{"verification off without the switch", "alpha", ver(func(v *v1alpha1.ProjectVerification) { v.Enabled = false }),
			psEvaluatorOff, checkSame},
		{"the hand-off made early", "alpha", ver(func(v *v1alpha1.ProjectVerification) { v.Handoff = v1alpha1.HandoffEarly }),
			psHandoff, checkSame},
		{"the hand-off made gated", "alpha", ver(func(v *v1alpha1.ProjectVerification) { v.Handoff = v1alpha1.HandoffGated }),
			"", checkSame},
		{"an early hand-off back to the default", "high",
			ver(func(v *v1alpha1.ProjectVerification) { v.Handoff = "" }), "", checkSame},
		{"the checks emptied without the switch", "alpha", func(p *v1alpha1.Project) { p.Spec.Checks.Fix = nil },
			psChecksEmpty, checkSame},
		{"a check dropped", "alpha", func(p *v1alpha1.Project) { p.Spec.Checks.Fix = []string{"lint"} }, "", checkSame},
		{"a model not admitted", "alpha", func(p *v1alpha1.Project) { modelsOf(p).Plan = "anthropic/claude-zzz" }, psModel,
			checkSame},
		{"a model admitted", "alpha", func(p *v1alpha1.Project) { modelsOf(p).Evaluate = "anthropic/claude-b" }, "",
			checkSame},
		{"a class not defined", "alpha", func(p *v1alpha1.Project) { p.Spec.Repositories[0].AgentResourceClass = "huge" },
			psClass, checkSame},
		{"a class defined", "alpha", func(p *v1alpha1.Project) { p.Spec.Repositories[1].AgentResourceClass = "large" }, "",
			checkSame},
		{"a class cleared", "alpha", func(p *v1alpha1.Project) { p.Spec.Repositories[0].AgentResourceClass = "" }, "",
			checkSame},
	})
}

// checkWithoutStatus: on a Project the controller has not written status on
// yet, a model or class choice is refused (nothing is admitted yet), and an
// edit that makes none is not, even beside a model and a class set before.
func (e *settingsEnv) checkWithoutStatus(t *testing.T) {
	e.run(t, []settingsCase{
		{"a model", "bare", func(p *v1alpha1.Project) { modelsOf(p).Build = "anthropic/claude-b" }, psModel, checkSame},
		{"a class", "bare", func(p *v1alpha1.Project) { p.Spec.Repositories[0].AgentResourceClass = "large" }, psClass,
			checkSame},
		{"an unrelated edit", "bare", func(p *v1alpha1.Project) { p.Spec.Limits.MaxRevisions = new(int32(4)) }, "",
			checkSame},
		{"a model cleared", "bare", func(p *v1alpha1.Project) { modelsOf(p).Plan = "" }, "", checkSame},
	})
}

// checkDenialShape records what a denial looks like to the status server:
// 422 Invalid (the policy leaves the reason at its default), which the page
// shows as a refusal.
func (e *settingsEnv) checkDenialShape(t *testing.T) {
	err := e.tryAlpha(func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 30_000_000 })
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		t.Fatalf("not an API status: %v", err)
	}
	s := status.Status()
	if s.Reason != metav1.StatusReasonInvalid || s.Code != 422 {
		t.Errorf("a denial is %s %d, want Invalid 422", s.Reason, s.Code)
	}
	var causes []string
	if s.Details != nil {
		for _, c := range s.Details.Causes {
			causes = append(causes, fmt.Sprintf("{type %q field %q}", c.Type, c.Field))
		}
	}
	t.Logf("a denial: reason %s, code %d, %d causes %v, message %q", s.Reason, s.Code, len(causes), causes, s.Message)
}

// checkSettingsRBAC: the grant is patch alone: no update, no status.
func (e *settingsEnv) checkSettingsRBAC(t *testing.T) {
	ctx := t.Context()
	p := e.get(t, "alpha")
	p.Spec.Limits.MaxRevisions = new(int32(4))
	if err := e.sa.Update(ctx, p.DeepCopy(), client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Errorf("an update as the status server: %v, want Forbidden", err)
	}
	live := e.get(t, "alpha")
	forged := live.DeepCopy()
	forged.Status.Admits.Models = append(forged.Status.Admits.Models, "evil/model")
	if err := e.sa.Status().Patch(ctx, forged, client.MergeFrom(live), client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Errorf("a status patch as the status server: %v, want Forbidden", err)
	}
	if err := e.sa.Delete(ctx, live.DeepCopy(), client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Errorf("a delete as the status server: %v, want Forbidden", err)
	}
}

// checkAdminUnjudged: the policy judges the status server alone; an
// admin's write of anything it pins is admitted.
func (e *settingsEnv) checkAdminUnjudged(t *testing.T) {
	for name, edit := range map[string]func(*v1alpha1.Project){
		"suspend":            func(p *v1alpha1.Project) { p.Spec.Suspend = true },
		"a foreign annotion": func(p *v1alpha1.Project) { p.Annotations["example.com/x"] = "y" },
		"past a ceiling":     func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 900_000_000 },
		"verification off":   func(p *v1alpha1.Project) { verifyOf(p).Enabled = false },
	} {
		if err := e.tryAs(e.admin, e.get(t, "alpha"), edit); err != nil {
			t.Errorf("%s as an admin: %v", name, err)
		}
	}
}

// replacePolicy puts the settings policy of another render in place of the
// one applied.
func (e *settingsEnv) replacePolicy(args []string) {
	t := e.t
	t.Helper()
	ctx := t.Context()
	next := findObject(renderObjects(t, args), "ValidatingAdmissionPolicy", settingsPolicy)
	var vap admissionregistrationv1.ValidatingAdmissionPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(next.Object, &vap); err != nil {
		t.Fatal(err)
	}
	var live admissionregistrationv1.ValidatingAdmissionPolicy
	if err := e.admin.Get(ctx, client.ObjectKey{Name: settingsPolicy}, &live); err != nil {
		t.Fatal(err)
	}
	live.Spec = vap.Spec
	if err := e.admin.Update(ctx, &live); err != nil {
		t.Fatalf("replace the policy: %v", err)
	}
	e.pol = renderedPolicy(t, renderObjects(t, args))
}

// checkSwitches: with each switch on, what it guards is admitted, and the
// approver rules that stay are still enforced.
func (e *settingsEnv) checkSwitches(t *testing.T) {
	approvers := func(logins ...string) func(*v1alpha1.Project) {
		return func(p *v1alpha1.Project) { p.Spec.Approvers.Logins = logins }
	}
	e.project("dupes", withStatus, func(p *v1alpha1.Project) { p.Spec.Approvers.Logins = []string{"alice", "Alice"} })
	e.run(t, []settingsCase{
		{"an approver added", "alpha", approvers("alice", "bob", "carol"), "", checkSame},
		{"an approver removed", "alpha", approvers("alice"), "", checkSame},
		{"an approver swapped", "alpha", approvers("alice", "carol"), psApproversOne, checkSame},
		{"two approvers added", "alpha", approvers("alice", "bob", "carol", "dave"), psApproversOne, checkSame},
		{"a login twice in two cases", "alpha", approvers("alice", "bob", "Alice"), psApproversCase, checkSame},
		{"an old case twin left alone", "dupes", func(p *v1alpha1.Project) { p.Spec.Limits.MaxRevisions = new(int32(4)) },
			"", checkSame},
		{"verification off", "alpha", func(p *v1alpha1.Project) { verifyOf(p).Enabled = false }, "", checkSame},
		{"the hand-off made early", "alpha", func(p *v1alpha1.Project) { verifyOf(p).Handoff = v1alpha1.HandoffEarly },
			"", checkSame},
		{"the checks emptied", "alpha", func(p *v1alpha1.Project) { p.Spec.Checks.Fix = nil }, "", checkSame},
		{"evaluate turns under the floor", "alpha", func(p *v1alpha1.Project) { p.Spec.Limits.Evaluate.MaxTurns = 10 }, "",
			checkSame},
		{"a ceiling still holds", "alpha", func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 30_000_000 },
			projectsettings.FieldMaxCost + psCeilingSuffix, checkSame},
		{"a pinned field still pinned", "alpha", func(p *v1alpha1.Project) { p.Spec.Suspend = true }, psPinned, checkBlind},
	})
}

// policyReasons are Check's refusals the admission policy mirrors; the
// rest (the schema's ranges, the controller's own ceilings, the run
// budgets, self-removal) are the status server's alone.
var policyReasons = []string{projectsettings.RefusedLocked, projectsettings.RefusedApproversNotAllowed,
	projectsettings.RefusedApproversOneAtATime, projectsettings.RefusedApproversInvalid,
	projectsettings.RefusedEvaluatorOffNotAllowed, projectsettings.RefusedHandoffNotAllowed,
	projectsettings.RefusedBelowFloor, projectsettings.RefusedAboveCeiling, projectsettings.RefusedModelNotAdmitted,
	projectsettings.RefusedClassNotAdmitted, projectsettings.RefusedChecksEmptyNotAllowed}

// settingsSeed keeps the property deterministic in the gate, as the
// repository's other seeded properties are; MAUL_SETTINGS_SEED explores.
const settingsSeed uint64 = 20261123

// checkProperty: random edits of every field the policy judges, from random
// starting values an admin wrote, are refused by the policy exactly when
// Check refuses them for a reason the policy mirrors.
func (e *settingsEnv) checkProperty(t *testing.T, phase uint64) {
	seed := settingsSeed
	if s, err := strconv.ParseUint(os.Getenv("MAUL_SETTINGS_SEED"), 10, 64); err == nil {
		seed = s
	}
	t.Logf("seed %d (MAUL_SETTINGS_SEED)", seed)
	rng := rand.New(rand.NewPCG(seed, phase))
	name := fmt.Sprintf("prop-%d", phase)
	e.project(name, withStatus)
	for i := range 60 {
		live := e.get(t, name)
		old := live.DeepCopy()
		randomSettings(rng, old)
		if err := e.admin.Patch(t.Context(), old, client.MergeFrom(live)); err != nil {
			t.Fatalf("case %d: the admin's starting values: %v", i, err)
		}
		live = e.get(t, name)
		updated := live.DeepCopy()
		randomSettings(rng, updated)
		err := e.sa.Patch(t.Context(), updated.DeepCopy(), client.MergeFrom(live), client.DryRunAll)
		if err != nil && !strings.Contains(err.Error(), "denied request") {
			t.Fatalf("case %d: refused outside the policy: %v", i, err)
		}
		var mirrored []projectsettings.ProjectSettingsRefusal
		for _, r := range projectsettings.Check(live, updated, e.pol, psProjectEditor) {
			if slices.Contains(policyReasons, r.Reason) {
				mirrored = append(mirrored, r)
			}
		}
		if (err != nil) != (len(mirrored) > 0) {
			t.Errorf("case %d: policy %v, Check %+v\nfrom %+v\nto   %+v", i, err, mirrored,
				projectsettings.Read(&live.Spec), projectsettings.Read(&updated.Spec))
		}
	}
}

// randomSettings moves a few of the fields the policy judges to random
// values inside the schema's ranges (a stage's cost never, which the
// schema holds to the per-intent cost).
func randomSettings(rng *rand.Rand, p *v1alpha1.Project) {
	pick := func(xs ...int64) int64 { return xs[rng.IntN(len(xs))] }
	opt32 := func(xs ...int64) *int32 {
		if rng.IntN(4) == 0 {
			return nil
		}
		return new(int32(pick(xs...)))
	}
	l := &p.Spec.Limits
	moves := []func(){
		func() { l.MaxCostMicroUSD = pick(5_000_000, 10_000_000, 25_000_000, 26_000_000, 40_000_000) },
		func() { l.MaxRevisions = opt32(0, 3, 10, 11, 20) },
		func() { l.MaxCheckFixes = opt32(0, 2, 10, 11) },
		func() { l.MaxVerifyFixes = opt32(0, 2, 5, 6, 10) },
		func() { l.MaxVerifications = opt32(1, 2, 3, 20, 50, 51) },
		func() { l.MaxActiveIntents = int32(pick(1, 2, 5, 6)) },
		func() {
			if rng.IntN(4) == 0 {
				l.MaxEvaluateCostMicroUSD = nil
			} else {
				l.MaxEvaluateCostMicroUSD = new(pick(1_000_000, 2_000_000, 6_000_000, 7_000_000))
			}
		},
		func() { l.Evaluate.MaxTurns = int32(pick(0, 10, 39, 40, 41, 100)) },
		func() { l.Evaluate.TokenBudget = pick(0, 50_000, 100_000, 300_000) },
		func() {
			if n := pick(0, 5, 10, 15); n == 0 {
				l.Evaluate.Timeout = nil
			} else {
				l.Evaluate.Timeout = minutes(int(n))
			}
		},
		func() { l.Plan.MaxTurns = int32(pick(0, 10, 30)) },
		func() { verifyOf(p).Enabled = rng.IntN(2) == 0 },
		func() {
			verifyOf(p).Handoff = []v1alpha1.HandoffMode{"", v1alpha1.HandoffGated, v1alpha1.HandoffEarly}[rng.IntN(3)]
		},
		func() {
			if n := pick(0, 15, 30, 60, 90); n == 0 {
				verifyOf(p).HandoffMax = nil
			} else {
				verifyOf(p).HandoffMax = minutes(int(n))
			}
		},
		func() { verifyOf(p).MaxToolCalls = opt32(50, 99, 100, 250, 300) },
		func() {
			modelsOf(p).Plan = []string{"", "anthropic/claude-a", "anthropic/claude-b", "anthropic/claude-zzz"}[rng.IntN(4)]
		},
		func() {
			modelsOf(p).Evaluate = []string{"", "anthropic/claude-b", "anthropic/claude-zzz"}[rng.IntN(3)]
		},
		func() {
			p.Spec.Checks.Fix = [][]string{nil, {"test"}, {"test", "lint"}, {"lint", "e2e"}}[rng.IntN(4)]
		},
		func() {
			p.Spec.Approvers.Logins = [][]string{{"alice"}, {"alice", "bob"}, {"alice", "bob", "carol"},
				{"alice", "carol"}, {"alice", "bob", "Bob"}}[rng.IntN(5)]
		},
		func() {
			p.Spec.Repositories[rng.IntN(2)].AgentResourceClass = []string{"", "small", "large", "huge"}[rng.IntN(4)]
		},
	}
	for range 1 + rng.IntN(4) {
		moves[rng.IntN(len(moves))]()
	}
}
