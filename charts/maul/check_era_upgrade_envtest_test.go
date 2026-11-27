// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/kube"
)

// checkEraOldCRDs are the CRDs of 0.14.18, the last release that knew
// saved checks' fields and settled what they left (the supersede, the
// check reviewer's runs stopped), frozen for the check-era upgrade test
// (tag v0.14.18, deploy/kustomize/base/crds). Each later release reads them
// from here, never from the tag.
const checkEraOldCRDs = "../../hack/testdata/crd-upgrade/0.14.18/"

// checkEraKinds are the CRDs the upgrade swaps.
var checkEraKinds = []string{"maul.run_intents.yaml", "maul.run_previewverifications.yaml",
	"maul.run_intentruns.yaml", "maul.run_projects.yaml"}

// The objects an install that skipped 0.14.18 (or stopped it early) leaves.
const (
	ceProject = "beatbattle"
	// ceJudged is open, its verification unmarked and judged under saved
	// checks (a quarantined outcome, the replay rung, a round in flight).
	ceJudged = "beatbattle-48"
	// ceMerged is terminal, its unmarked judgement kept for its comments.
	ceMerged = "beatbattle-47"
	// ceSettled is open, superseded by 0.14.18: marked, its fresh round
	// started, the condition still standing until that round is judged.
	ceSettled = "beatbattle-49"
	// ceLadderOnly is open and unmarked, holding the retired ladder's
	// records alone (pruned: nothing judged).
	ceLadderOnly = "beatbattle-50"
	// ceWithdrawn is open: 0.14.18 withdrew its result and its fresh round
	// has not started.
	ceWithdrawn = "beatbattle-51"
	// ceReview is an unfinished check review; ceReviewDone a finished one.
	ceReview     = "beatbattle-48-cr-r1-a1"
	ceReviewDone = "beatbattle-47-cr-r1-a1"
	// ceEvaluate is a finished evaluate run recording its rung.
	ceEvaluate = "beatbattle-47-ev-r1-a1"
	// ceReplayPV is the judged intent's replay round, running; ceKeptPV a
	// settled agents round with retired reason kept-copy-failed and a part
	// of the retired tester role.
	ceReplayPV = "beatbattle-48-v2-a1"
	ceKeptPV   = "beatbattle-47-v1-a1"
)

// checkEraUpgrade is one API server moved from 0.14.18's CRDs to this
// release's.
type checkEraUpgrade struct {
	t     *testing.T
	admin client.Client
	crds  client.Client
	h     helmRunner
}

// TestCheckEraUpgradeAgainstAPIServer: this release removes the check-era
// fields 0.14.18 kept for its own settling, so an install must pass through
// 0.14.18. Against a real API server it installs 0.14.18's CRDs with what an
// install that skipped 0.14.18 leaves (an open Intent whose unmarked
// verification an earlier release judged with saved checks, one 0.14.18
// withdrew whose fresh round has not started, an unfinished check review)
// beside what is harmless (a terminal Intent with a quarantined outcome, an
// Intent 0.14.18 superseded, an unmarked one holding only the retired
// ladder's records, a finished check review, an evaluate run and a settled
// verification recording retired spellings, a Project choosing a check
// review model and limit), and holds:
//
//   - the chart's pre-upgrade guard fails, naming exactly the leftovers,
//     with 0.14.18's CRDs and again with this release's;
//   - intent-controller's census finds exactly the same records both ways,
//     never the harmless ones, and is clear, as is the guard, once the
//     leftovers are deleted;
//   - under this release's CRDs every harmless object, its retired fields
//     pruned, still takes a status write and a metadata update (as a
//     finalizer's removal would), so no stored object fails validation on
//     its next write.
func TestCheckEraUpgradeAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	u := startCheckEraUpgrade(t)
	u.leaveCheckEraBehind()
	wantIntents, wantRuns := []string{ceJudged, ceWithdrawn}, []string{ceReview}

	u.guardFails("0.14.18's CRDs", wantIntents, wantRuns)
	u.census(wantIntents, wantRuns)

	u.applyCRDs(newCRDs)
	u.waitPruned()
	u.guardFails("this release's CRDs", wantIntents, wantRuns)
	u.census(wantIntents, wantRuns)
	for _, w := range []struct {
		kind, name string
	}{
		{"Intent", ceMerged}, {"Intent", ceSettled}, {"Intent", ceLadderOnly},
		{"IntentRun", ceReviewDone}, {"IntentRun", ceEvaluate},
		{"PreviewVerification", ceKeptPV}, {"Project", ceProject},
	} {
		u.writable(w.kind, w.name)
	}

	for _, kind := range []string{"Intent/" + ceJudged, "Intent/" + ceWithdrawn, "IntentRun/" + ceReview} {
		k, name, _ := strings.Cut(kind, "/")
		if err := u.admin.Delete(t.Context(), u.get(k, name)); err != nil {
			t.Fatalf("delete %s: %v", kind, err)
		}
	}
	u.census(nil, nil)
	if out, err := u.h.run(u.renderArgs()...); err != nil {
		t.Errorf("the guard still fails once the leftovers are deleted: %v\n%s", err, out)
	}

	// Back to 0.14.18: what this release created (an outcome verification
	// and an evaluate run, neither naming the rung 0.14.18's rules require)
	// still takes 0.14.18's writes, its unchanged spec ratcheted.
	pv := cePV("beatbattle-52-v1-a1", "beatbattle-52")
	pv.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning, StartedAt: &ceNow,
		Environment: []v1alpha1.PartFact{{Name: "tools", Role: v1alpha1.PreviewRoleToolPod}}}
	u.create(pv)
	run := ceRun("beatbattle-52-ev-r1-a1", "beatbattle-52", v1alpha1.IntentStageEvaluate, v1alpha1.RunRunning)
	run.Spec.Evaluation = &v1alpha1.IntentRunEvaluation{Verification: pv.Name, UID: "u-" + types.UID(pv.Name),
		ToolIP: "10.0.0.7"}
	u.create(run)
	u.applyCRDs(checkEraOldCRDs)
	u.waitServedOld()
	u.writable("PreviewVerification", pv.Name)
	u.writable("IntentRun", run.Name)
}

func startCheckEraUpgrade(t *testing.T) *checkEraUpgrade {
	t.Helper()
	paths := make([]string, 0, len(checkEraKinds))
	for _, f := range checkEraKinds {
		paths = append(paths, checkEraOldCRDs+f)
	}
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: paths}}
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}
	if err := admin.Create(t.Context(), ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crds, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("crd client: %v", err)
	}
	return &checkEraUpgrade{t: t, admin: admin, crds: crds, h: newHelm(t, env)}
}

var (
	ceNow     = metav1.NewTime(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	ceDigestA = "sha256:" + strings.Repeat("a", 64)
	ceDigestB = "sha256:" + strings.Repeat("b", 64)
	ceDigestC = "sha256:" + strings.Repeat("c", 64)
	ceHeads   = []v1alpha1.VerificationHead{{Component: "app", Repository: "https://github.com/acme/app",
		Revision: strings.Repeat("a", 40)}}
	ceOutcomes = []v1alpha1.AcceptanceOutcome{{ID: "counts", Kind: v1alpha1.OutcomeKindChange,
		Outcome: "bbcount prints the number of beats.", From: "print the beat count"}}
	// ceQuarantined is the retired outcome result 0.14.16 wrote, a stored
	// spelling the schema keeps.
	ceQuarantined = v1alpha1.OutcomeResultValue("quarantined")
)

// retired is a field 0.14.18's CRDs hold and this release's prune: set on
// the object as created, so this release's Go types need not know it.
type retired struct {
	value any
	path  []string
}

// ceIntent is the open Intent for issue n of Project beatbattle in phase,
// with verification v and the outcomes recorded.
func ceIntent(n int64, phase v1alpha1.IntentPhase, v *v1alpha1.IntentVerification,
	conds ...metav1.Condition) *v1alpha1.Intent {
	in := attributedIntent(n, 0)
	in.Name, in.Spec.Project = v1alpha1.IntentName(ceProject, n), ceProject
	in.Status = v1alpha1.IntentStatus{Phase: phase, Conditions: conds,
		Acceptance: &v1alpha1.IntentAcceptance{PlanRevision: 1, Digest: v1alpha1.EmptyListDigest,
			Outcomes: ceOutcomes, OutcomesDigest: ceDigestB},
		Verification: v}
	return in
}

// ceRun is a role run of stage of the Intent intentName.
func ceRun(name, intentName string, stage v1alpha1.IntentStage, phase v1alpha1.RunPhase) *v1alpha1.IntentRun {
	run := attributedIntentRun("x", nil)
	run.Name = name
	run.Spec.IntentRef = v1alpha1.ObjectReference{Name: intentName, UID: "u-intent"}
	run.Spec.Stage, run.Spec.Round = stage, 1
	run.Spec.Inputs.PlanRevision, run.Spec.Inputs.PlanDigest = 1, ceDigestA
	run.Status = v1alpha1.IntentRunStatus{Phase: phase, StartedAt: &ceNow}
	if phase == v1alpha1.RunComplete {
		run.Status.Outcome, run.Status.FinishedAt = "evaluated", &ceNow
	}
	return run
}

// cePV is an agents verification of the Intent intentName.
func cePV(name, intentName string) *v1alpha1.PreviewVerification {
	v := agentsVerification(name)
	v.Spec.IntentRef = v1alpha1.ObjectReference{Name: intentName, UID: "u-intent"}
	return v
}

// leaveCheckEraBehind creates what an install that skipped 0.14.18 leaves,
// under 0.14.18's CRDs, each with the retired fields it held.
func (u *checkEraUpgrade) leaveCheckEraBehind() {
	ref := func(name string) *v1alpha1.ObjectReference {
		return &v1alpha1.ObjectReference{Name: name, UID: types.UID("u-" + name)}
	}
	superseded := metav1.Condition{Type: v1alpha1.ConditionVerificationSuperseded, Status: metav1.ConditionTrue,
		Reason: "VerificationRulesChanged", Message: "withdrawn", LastTransitionTime: ceNow}
	quarantined := []v1alpha1.OutcomeResult{{ID: "counts", Result: ceQuarantined}}
	ladder := []retired{
		{"replay", []string{"status", "verification", "rung"}},
		{map[string]any{"round": int64(1), "headSetDigest": ceDigestA, "outcomesDigest": ceDigestB, "clear": true},
			[]string{"status", "verification", "modelRound"}},
	}

	project := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: ceProject, Namespace: egressRelease},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app"}},
			Verification:     &v1alpha1.ProjectVerification{Enabled: true},
			Models:           &v1alpha1.ProjectModels{Evaluate: "anthropic/claude-opus-5-5"},
		},
	}
	project.Spec.Limits.Evaluate.MaxCostMicroUSD = 6000000
	project.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: "Ready", Message: "the Project is ready", LastTransitionTime: ceNow, ObservedGeneration: 1}}
	u.create(project,
		retired{"anthropic/claude-opus-5-5", []string{"spec", "models", "checkReview"}},
		retired{int64(1000000), []string{"spec", "limits", "checkReview", "maxCostMicroUSD"}})

	u.create(ceIntent(48, v1alpha1.IntentInReview, &v1alpha1.IntentVerification{
		State: v1alpha1.VerificationEvaluating, Round: 2, Attempt: 1, Ref: ref(ceReplayPV),
		HeadSetDigest: ceDigestA, Heads: ceHeads, Outcomes: quarantined, OutcomesRound: 1,
		OutcomesHeadSetDigest: ceDigestA}),
		append(ladder, retired{int64(1), []string{"status", "verification", "quarantined"}})...)
	u.create(ceIntent(47, v1alpha1.IntentMerged, &v1alpha1.IntentVerification{
		State: v1alpha1.VerificationUnconfirmed, Round: 1, Outcomes: quarantined, OutcomesRound: 1,
		OutcomesHeadSetDigest: ceDigestA}),
		append(ladder, retired{[]any{"counts"}, []string{"status", "verification", "quarantinedIDs"}})...)
	u.create(ceIntent(49, v1alpha1.IntentInReview, &v1alpha1.IntentVerification{Judge: v1alpha1.JudgeObserved,
		State: v1alpha1.VerificationEvaluating, Round: 3, Attempt: 1, Ref: ref("beatbattle-49-v3-a1"),
		HeadSetDigest: ceDigestA, Heads: ceHeads}, superseded))
	u.create(ceIntent(50, v1alpha1.IntentInReview, &v1alpha1.IntentVerification{
		State: v1alpha1.VerificationWaitingForCI, Round: 2}), ladder...)
	u.create(ceIntent(51, v1alpha1.IntentInReview, &v1alpha1.IntentVerification{
		State: v1alpha1.VerificationWaitingForCI, Round: 2}, superseded))

	review := ceRun(ceReview, ceJudged, "checkreview", v1alpha1.RunRunning)
	review.Status.JobRef = &v1alpha1.JobReference{Name: ceReview}
	u.create(review)
	u.create(ceRun(ceReviewDone, ceMerged, "checkreview", v1alpha1.RunComplete))
	ev := ceRun(ceEvaluate, ceMerged, v1alpha1.IntentStageEvaluate, v1alpha1.RunComplete)
	ev.Spec.Evaluation = &v1alpha1.IntentRunEvaluation{Verification: ceKeptPV, UID: types.UID("u-" + ceKeptPV),
		ToolIP: "10.0.0.7"}
	u.create(ev, retired{"full", []string{"spec", "evaluation", "rung"}})

	u.create(cePV(ceReplayPV, ceJudged), retired{"replay", []string{"spec", "rung"}},
		retired{[]any{map[string]any{"id": "counts-1", "outcome": "counts", "form": "run",
			"body": `{"files":{"main.go":"package main"}}`, "digest": ceDigestC}}, []string{"spec", "checks"}},
		retired{ceDigestC, []string{"spec", "checksDigest"}})
	kept := cePV(ceKeptPV, ceMerged)
	kept.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationInfra,
		Reason: "kept-copy-failed", StartedAt: &ceNow, CompletedAt: &ceNow,
		Environment: []v1alpha1.PartFact{{Name: "tester", Role: "tester"}}}
	u.create(kept, retired{"full", []string{"spec", "rung"}})
}

// create creates obj with the retired fields set, then writes its status
// (with the retired status fields among them) under the CRDs served now.
func (u *checkEraUpgrade) create(obj client.Object, fields ...retired) {
	u.t.Helper()
	t, ctx := u.t, u.t.Context()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if err := unstructured.SetNestedField(m, runtime.DeepCopyJSONValue(f.value), f.path...); err != nil {
			t.Fatalf("set %s on %s: %v", strings.Join(f.path, "."), obj.GetName(), err)
		}
	}
	gvk, _, err := kube.Scheme().ObjectKinds(obj)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := m["status"].(map[string]any)
	delete(m, "status")
	o := &unstructured.Unstructured{Object: m}
	o.SetGroupVersionKind(gvk[0])
	if err := u.admin.Create(ctx, o); err != nil {
		t.Fatalf("create %s %s under 0.14.18's CRDs: %v", gvk[0].Kind, obj.GetName(), err)
	}
	if len(status) == 0 {
		return
	}
	o.Object["status"] = status
	if err := u.admin.Status().Update(ctx, o); err != nil {
		t.Fatalf("status of %s %s under 0.14.18's CRDs: %v", gvk[0].Kind, obj.GetName(), err)
	}
}

// get is the stored kind/name, unstructured.
func (u *checkEraUpgrade) get(kind, name string) *unstructured.Unstructured {
	u.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	if err := u.admin.Get(u.t.Context(), client.ObjectKey{Namespace: egressRelease, Name: name}, obj); err != nil {
		u.t.Fatalf("get %s %s: %v", kind, name, err)
	}
	return obj
}

// writable: the stored kind/name, as this release reads it, takes a status
// write that changes something and a metadata update, as a controller's
// next write and a finalizer's removal would.
func (u *checkEraUpgrade) writable(kind, name string) {
	u.t.Helper()
	ctx := u.t.Context()
	obj := u.get(kind, name)
	if kind == "PreviewVerification" {
		env, _, _ := unstructured.NestedSlice(obj.Object, "status", "environment")
		env[0].(map[string]any)["restarts"] = int64(1)
		if err := unstructured.SetNestedSlice(obj.Object, env, "status", "environment"); err != nil {
			u.t.Fatal(err)
		}
	} else {
		conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		conds = append(conds, map[string]any{"type": "Upgraded", "status": "True", "reason": "Upgraded",
			"message": "written by the next release", "lastTransitionTime": ceNow.UTC().Format(time.RFC3339)})
		if err := unstructured.SetNestedSlice(obj.Object, conds, "status", "conditions"); err != nil {
			u.t.Fatal(err)
		}
	}
	if err := u.admin.Status().Update(ctx, obj); err != nil {
		u.t.Errorf("%s %s refuses a status write under this release's CRDs: %v", kind, name, err)
		return
	}
	obj = u.get(kind, name)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["maul.run/upgraded"] = "true"
	obj.SetLabels(labels)
	if err := u.admin.Update(ctx, obj); err != nil {
		u.t.Errorf("%s %s refuses a metadata update under this release's CRDs: %v", kind, name, err)
	}
}

// applyCRDs replaces the four CRDs with those under dir.
func (u *checkEraUpgrade) applyCRDs(dir string) {
	applyCRDFiles(u.t, u.crds, dir, checkEraKinds)
}

// applyCRDFiles replaces each CRD of files with its definition under dir.
func applyCRDFiles(t *testing.T, crds client.Client, dir string, files []string) {
	t.Helper()
	ctx := t.Context()
	for _, file := range files {
		b, err := os.ReadFile(dir + file)
		if err != nil {
			t.Fatal(err)
		}
		var want apiextensionsv1.CustomResourceDefinition
		dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(b)), 4096)
		for want.Name == "" {
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("decode %s: %v", file, err)
			}
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := crds.Get(ctx, client.ObjectKey{Name: want.Name}, &crd); err != nil {
			t.Fatal(err)
		}
		crd.Spec = want.Spec
		if err := crds.Update(ctx, &crd); err != nil {
			t.Fatalf("apply %s%s: %v", dir, file, err)
		}
	}
}

// waitPruned waits until the API server serves this release's CRDs: the
// Project's retired spec.models.checkReview reads back pruned on a dry-run
// update (the field is in no schema of this release). Without it the
// writes below could run against 0.14.18's schema and pass vacuously.
func (u *checkEraUpgrade) waitPruned() {
	t, ctx := u.t, u.t.Context()
	deadline := time.Now().Add(time.Minute)
	for {
		obj := u.get("Project", ceProject)
		err := u.admin.Update(ctx, obj, client.DryRunAll)
		_, found, _ := unstructured.NestedString(obj.Object, "spec", "models", "checkReview")
		if err == nil && !found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("this release's CRDs are not served after a minute: %v, checkReview kept %v", err, found)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitServedOld waits until the API server serves 0.14.18's CRDs again: a
// dry-run update of the Project keeps a check-review model. Without it the
// rollback's writes could run against this release's schema and pass
// vacuously.
func (u *checkEraUpgrade) waitServedOld() {
	t, ctx := u.t, u.t.Context()
	deadline := time.Now().Add(time.Minute)
	for {
		obj := u.get("Project", ceProject)
		if err := unstructured.SetNestedField(obj.Object, "anthropic/claude-opus-5-5", "spec", "models",
			"checkReview"); err != nil {
			t.Fatal(err)
		}
		err := u.admin.Update(ctx, obj, client.DryRunAll)
		_, found, _ := unstructured.NestedString(obj.Object, "spec", "models", "checkReview")
		if err == nil && found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("0.14.18's CRDs are not served after a minute: %v, checkReview kept %v", err, found)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// renderArgs renders the chart against the API server, its lookups real,
// leaving out the CRDs this test installed.
func (u *checkEraUpgrade) renderArgs() []string {
	return []string{"template", "maul", ".", "--namespace", egressRelease, "--dry-run=server",
		"--set", "crds.install=false"}
}

// guardFails: the render fails, naming each leftover and none of the
// harmless records.
func (u *checkEraUpgrade) guardFails(crds string, intents, runs []string) {
	u.t.Helper()
	out, err := u.h.run(u.renderArgs()...)
	if err == nil {
		u.t.Fatalf("with %s and check-era leftovers, the render passed", crds)
	}
	for _, want := range append(append([]string{"an install must pass through 0.14.18",
		"migrating-from-saved-checks"}, intents...), runs...) {
		if !strings.Contains(out, want) {
			u.t.Errorf("with %s the render failed without naming %q:\n%s", crds, want, out)
		}
	}
	for _, harmless := range []string{ceMerged, ceSettled, ceLadderOnly, ceReviewDone, ceEvaluate} {
		if strings.Contains(out, harmless+",") || strings.Contains(out, harmless+" ") ||
			strings.Contains(out, harmless+")") {
			u.t.Errorf("with %s the guard names %s:\n%s", crds, harmless, out)
		}
	}
}

// census: intent-controller's startup census finds exactly these records
// and refuses to start while it finds any.
func (u *checkEraUpgrade) census(intents, runs []string) {
	u.t.Helper()
	got, err := intent.TakeCheckEraCensus(u.t.Context(), u.admin, egressRelease)
	if err != nil {
		u.t.Fatalf("census: %v", err)
	}
	if !slices.Equal(got.Intents, intents) || !slices.Equal(got.Runs, runs) {
		u.t.Errorf("census = intents %v, runs %v; want %v and %v", got.Intents, got.Runs, intents, runs)
	}
	if (got.Err() != nil) != (len(intents)+len(runs) > 0) {
		u.t.Errorf("census refusal = %v with %d records", got.Err(), got.Count())
	}
}
