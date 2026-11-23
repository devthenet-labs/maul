// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// startServer starts an envtest API server carrying the generated CRDs.
// Skipped without KUBEBUILDER_ASSETS (`mise run envtest` sets it).
func startServer(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping the API server round trip")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../deploy/kustomize/base/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// served is a Project on a real API server, as the settings write path
// meets it: applied by an operator's manifest, its status published by
// intent-controller, and patched the way the status server patches.
type served struct {
	t   *testing.T
	c   client.Client
	key client.ObjectKey
	pol Policy
}

// serve applies the operator's manifest the way Helm 4 applies one, and
// publishes what intent-controller would through the status subresource:
// alice is an approver with write access.
func serve(t *testing.T) served {
	t.Helper()
	s := served{t: t, c: startServer(t), key: client.ObjectKey{Namespace: "default", Name: "demo"},
		pol: openPolicy()}
	ctx := context.Background()
	manifest := &v1alpha1.Project{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Project"},
		ObjectMeta: metav1.ObjectMeta{Namespace: s.key.Namespace, Name: s.key.Name},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"alice", "bob"}},
			Repositories: []v1alpha1.ProjectRepository{
				{Name: "app", URL: "https://github.com/acme/app", AgentResourceClass: "small",
					Preview: &v1alpha1.ProjectRepositoryPreview{ProjectPreview: v1alpha1.ProjectPreview{
						ImageRepository: "registry.example/acme/app", Port: 8080, ReadinessPath: "/healthz"}}},
				{Name: "lib", URL: "https://github.com/acme/lib"},
			},
			Limits: v1alpha1.ProjectLimits{MaxRevisions: i32(5),
				Build: v1alpha1.StageLimits{Timeout: dur(10 * time.Minute)}},
			Checks: v1alpha1.ProjectChecks{Fix: []string{"test", "lint"}},
		},
	}
	//nolint:staticcheck // client.Apply with a typed object is what this API server test needs.
	if err := s.c.Patch(ctx, manifest, client.Apply, client.FieldOwner("helm")); err != nil {
		t.Fatalf("apply the manifest: %v", err)
	}
	published := s.live()
	resolved := metav1.NewTime(testNow.Add(-time.Minute))
	published.Status.Approvers = &v1alpha1.ProjectApproversStatus{ResolvedAt: &resolved,
		Identities: []v1alpha1.ProjectApproverIdentity{{Login: "alice", ID: 1, Write: true}, {Login: "bob", ID: 2}}}
	published.Status.Admits = baseAdmits()
	if err := s.c.Status().Update(ctx, published); err != nil {
		t.Fatalf("publish status: %v", err)
	}
	return s
}

// live reads the Project as the server has it.
func (s served) live() *v1alpha1.Project {
	s.t.Helper()
	var p v1alpha1.Project
	if err := s.c.Get(context.Background(), s.key, &p); err != nil {
		s.t.Fatal(err)
	}
	return &p
}

// decide is alice saving doc from a page served at base.
func (s served) decide(base *v1alpha1.Project, doc ProjectSettings, confirm ...string) Decision {
	return Decide(base, ProjectSettingsRequest{Generation: base.Generation, Schema: Schema, Settings: doc,
		Confirm: confirm}, s.pol, 1, testNow)
}

// patch sends what the status server's write path sends: the JSON merge
// patch between the Project as read and the copy Decide changed, with the
// optimistic lock, under the page's field manager. It returns the Project
// as the server then has it.
func (s served) patch(base, out *v1alpha1.Project) *v1alpha1.Project {
	s.t.Helper()
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := s.c.Patch(context.Background(), out, patch, client.FieldOwner(FieldManager)); err != nil {
		s.t.Fatalf("patch: %v", err)
	}
	return s.live()
}

// What the pure core promises is held to a real API server, through Decide
// as the write path runs it: a save that changes nothing is no write at
// all, a save lands exactly the settings it posted (defaults and cleared
// fields included) with its record, a repository keeps everything but its
// class, and the field managers View reads out of real managedFields are
// the ones that apply each field.
func TestSaveThroughARealAPIServer(t *testing.T) {
	s := serve(t)
	s.unchangedSaveIsNoWrite()
	saved := s.saveLandsWhatItPosted()
	s.viewNamesTheRealManagers(saved)
	s.clearingLandsOnTheDefaults(saved)
}

// An unchanged save: nothing to write, and a patch of the untouched copy
// is no write at the server either.
func (s served) unchangedSaveIsNoWrite() {
	t := s.t
	t.Helper()
	base := s.live()
	if d := s.decide(base, Read(&base.Spec)); d.Patched != nil || d.Result.Reason != "" ||
		len(d.Result.Changes) != 0 {
		t.Fatalf("unchanged save: %+v", d.Result)
	}
	out := base.DeepCopy()
	if changes, err := Apply(out, Read(&base.Spec)); err != nil || len(changes) != 0 {
		t.Fatalf("unchanged save: changes %+v, error %v", changes, err)
	}
	if got := s.patch(base, out); got.ResourceVersion != base.ResourceVersion || got.Generation != base.Generation {
		t.Errorf("an unchanged save was a write: resourceVersion %s -> %s, generation %d -> %d",
			base.ResourceVersion, got.ResourceVersion, base.Generation, got.Generation)
	}
}

// A save of many kinds of change lands exactly what it posted, with its
// record, once its one weakening change is confirmed.
func (s served) saveLandsWhatItPosted() *v1alpha1.Project {
	t := s.t
	t.Helper()
	base := s.live()
	doc := Read(&base.Spec)
	doc.Approvers = append(doc.Approvers, "carol")
	doc.Verification = ProjectSettingsVerification{Enabled: true, Handoff: "gated", HandoffMaxSeconds: 1800,
		MaxToolCalls: 120}
	doc.Models.Plan = "anthropic/claude-a"
	doc.Limits.MaxRevisions, doc.Limits.MaxVerifyFixes = i32(7), i32(0)
	doc.Limits.MaxActiveIntents, doc.Limits.MaxVerifications = 4, 30
	doc.Limits.MaxEvaluateCostMicroUSD = 3_000_000
	doc.Stages.Evaluate = ProjectSettingsStage{MaxTurns: 50, TokenBudget: 200_000, MaxCostMicroUSD: 2_000_000,
		TimeoutSeconds: 900}
	doc.Checks.Fix, doc.Checks.TimeoutSeconds, doc.Checks.RerunFailed = []string{"test", "e2e"}, 600, true
	doc.Repositories[1].AgentResourceClass = "large"
	const changed = 19
	if d := s.decide(base, doc); d.Result.Reason != ResultConfirmRequired || len(d.Result.Confirm) != 1 {
		t.Fatalf("save without its confirmation: %+v", d.Result)
	}
	d := s.decide(base, doc, "approver-add:carol")
	if d.Patched == nil || len(d.Result.Changes) != changed {
		t.Fatalf("save: %d changes, result %+v", len(d.Result.Changes), d.Result)
	}
	saved := s.patch(base, d.Patched)
	if got, want := Read(&saved.Spec), Read(&d.Patched.Spec); !reflect.DeepEqual(got, want) {
		t.Errorf("the server stored\n%+v\nthe save made\n%+v", got, want)
	}
	if left := Changes(&saved.Spec, &d.Patched.Spec); len(left) != 0 {
		t.Errorf("the stored Project still differs from the save: %+v", left)
	}
	if saved.Generation != base.Generation+1 {
		t.Errorf("generation %d -> %d, want one more", base.Generation, saved.Generation)
	}
	if e, ok := ParseEdit(saved.Annotations[EditedAnnotation]); !ok || e.Login != "alice" || e.GitHubID != 1 ||
		len(e.Fields)+e.More != changed {
		t.Errorf("the stored record of the save = %+v, %v", e, ok)
	}
	if !reflect.DeepEqual(saved.Status.Admits, base.Status.Admits) {
		t.Error("the save changed what the controller published")
	}
	for i, repo := range saved.Spec.Repositories {
		want := base.Spec.Repositories[i]
		want.AgentResourceClass = d.Patched.Spec.Repositories[i].AgentResourceClass
		if !reflect.DeepEqual(repo, want) {
			t.Errorf("repository %d = %+v, want %+v", i, repo, want)
		}
	}
	return saved
}

// The managers View names are the ones that apply each field now: Helm
// keeps what the page did not change, and loses what it did.
func (s served) viewNamesTheRealManagers(saved *v1alpha1.Project) {
	t := s.t
	t.Helper()
	v := View(saved, s.pol, 0, testNow)
	for id, want := range map[string][]string{
		FieldMaxRevisions:        {},       // the manifest's, changed by the page
		"limits.build.timeout":   {"helm"}, // the manifest's, untouched
		FieldChecksFix:           {"helm"}, // "test" is still the manifest's
		FieldApprovers:           {"helm"},
		FieldRepositoryClass:     {"helm"}, // app's
		FieldMaxActiveIntents:    {},       // defaulted, then the page's
		FieldVerificationEnabled: {},
	} {
		if got := fieldOf(t, v, id).Managers; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: managers %v, want %v", id, got, want)
		}
	}
	logins := map[string][]string{}
	for _, a := range v.Approvers {
		logins[a.Login] = a.Managers
	}
	if want := map[string][]string{"alice": {"helm"}, "bob": {"helm"}, "carol": {}}; !reflect.DeepEqual(logins, want) {
		t.Errorf("approver managers %v, want %v", logins, want)
	}
	app, lib := v.Facts.Repositories[0], v.Facts.Repositories[1]
	if !reflect.DeepEqual(app.ClassManagers, []string{"helm"}) || len(lib.ClassManagers) != 0 {
		t.Errorf("class managers: app %v, lib %v", app.ClassManagers, lib.ClassManagers)
	}
	if !app.Previewed || app.PreviewPath != "/" {
		t.Errorf("app preview fact %+v", app)
	}
}

// Clearing: the schema's defaults come back, the rest are unset.
func (s served) clearingLandsOnTheDefaults(base *v1alpha1.Project) {
	t := s.t
	t.Helper()
	doc := Read(&base.Spec)
	doc.Limits = ProjectSettingsLimits{}
	doc.Verification.HandoffMaxSeconds, doc.Verification.MaxToolCalls = 0, 0
	doc.Stages.Evaluate = ProjectSettingsStage{}
	doc.Checks.TimeoutSeconds, doc.Checks.Fix = 0, nil
	doc.Models.Plan = ""
	doc.Repositories[1].AgentResourceClass = ""
	d := s.decide(base, doc, "checks-empty")
	if d.Patched == nil {
		t.Fatalf("clearing: %+v", d.Result)
	}
	cleared := s.patch(base, d.Patched)
	if got, want := Read(&cleared.Spec), Read(&d.Patched.Spec); !reflect.DeepEqual(got, want) {
		t.Errorf("after clearing the server stored\n%+v\nthe save made\n%+v", got, want)
	}
	l := cleared.Spec.Limits
	if l.MaxRevisions == nil || *l.MaxRevisions != 3 || l.MaxActiveIntents != 2 || l.MaxCostMicroUSD != 10_000_000 {
		t.Errorf("the schema's defaults did not come back: %+v", l)
	}
	if l.MaxVerifyFixes != nil || l.MaxVerifications != nil || l.MaxEvaluateCostMicroUSD != nil ||
		l.Evaluate.Timeout != nil || len(cleared.Spec.Checks.Fix) != 0 {
		t.Errorf("not unset: limits %+v, checks %+v", l, cleared.Spec.Checks)
	}
	if got := cleared.Spec.Checks.Timeout; got == nil || got.Seconds() != 1800 {
		t.Errorf("cleared checks.timeout = %v, want the schema's 30m", got)
	}
}
