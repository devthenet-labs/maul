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
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/controller/intent"
	"github.com/devthenet-labs/maul/internal/kube"
)

// The CRDs of the release before slice 1's criteria fields were removed
// (origin/main 90913cc2, 0.13), frozen for the upgrade test, and this
// release's.
const (
	oldCRDs = "../../hack/testdata/crd-upgrade/0.13/"
	newCRDs = "../../deploy/kustomize/base/crds/"
)

// slice1Upgrade is one API server upgraded from the old CRDs to the new.
type slice1Upgrade struct {
	t     *testing.T
	admin client.Client
	// crds is a client of the CustomResourceDefinitions.
	crds client.Client
	h    helmRunner
}

// TestSlice1UpgradeAgainstAPIServer installs the CRDs of the release that
// still stored slice 1's criteria, with what that release left behind: a
// terminal Intent and an open one recording criteria (and a judged failure
// and a CriterionError halt), a criteria PreviewVerification, and an outcome
// PreviewVerification settled with the retired stale-criteria reason and a
// part of the retired tester role. It then holds the upgrade to what the
// release promises, before and after this release's CRDs are applied:
//
//   - the chart's pre-upgrade guard fails, naming the criteria verification,
//     whichever CRDs it reads (the old with spec.criteria, the new with it
//     pruned), and passes once it is deleted;
//   - intent-controller's census names the open Intent and the criteria
//     verification, never the terminal Intent, and is clear once they are
//     deleted;
//   - the terminal Intent is still updatable (metadata and status), so its
//     finalizers and garbage collection keep working;
//   - the outcome verification still validates and updates, and a new one is
//     admitted; a criteria request is refused.
func TestSlice1UpgradeAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	u := startSlice1Upgrade(t)
	u.leaveSliceOneBehind()
	ctx := t.Context()

	u.guardFails("the previous release's CRDs")
	u.census([]string{"beatbattle-48"}, []string{"beatbattle-48-v1-a1"})

	u.applyNewCRDs()
	terminal := u.get("Intent", "beatbattle-47")
	if _, found, _ := unstructured.NestedSlice(terminal.Object, "status", "acceptance", "criteria"); found {
		t.Error("the terminal Intent still reads its criteria under the new CRDs: want them pruned")
	}
	if got, _, _ := unstructured.NestedString(terminal.Object, "status", "verification", "autoFix", "reason"); got !=
		string(v1alpha1.AutoFixCriterionError) {
		t.Errorf("terminal Intent's auto-fix reason = %q, want CriterionError kept", got)
	}
	u.updatable("Intent", "beatbattle-47")
	u.updatable("PreviewVerification", "beatbattle-49-v1-a1")
	if err := u.admin.Create(ctx, outcomeVerification("beatbattle-50-v1-a1"), client.DryRunAll); err != nil {
		t.Errorf("a new outcome verification is refused under the new CRDs: %v", err)
	}
	if err := u.admin.Create(ctx, criteriaVerification("beatbattle-51-v1-a1"), client.DryRunAll); !deniedWith(err,
		"slice 1's criteria are removed") {
		t.Errorf("a criteria verification under the new CRDs: %v, want it refused", err)
	}

	// The criteria verification, pruned, still takes preview-controller's
	// Stale answer and a delete's finalizer removal: the API server
	// ratchets the spec rule it no longer meets while the spec is unchanged.
	u.updatable("PreviewVerification", "beatbattle-48-v1-a1")
	u.guardFails("this release's CRDs, its criteria pruned")
	u.census([]string{"beatbattle-48"}, []string{"beatbattle-48-v1-a1"})

	for _, kind := range []string{"Intent/beatbattle-48", "PreviewVerification/beatbattle-48-v1-a1"} {
		k, name, _ := strings.Cut(kind, "/")
		if err := u.admin.Delete(ctx, u.get(k, name)); err != nil {
			t.Fatalf("delete %s: %v", kind, err)
		}
	}
	u.census(nil, nil)
	if out, err := u.h.run(u.renderArgs()...); err != nil {
		t.Errorf("the guard still fails once the criteria verification is deleted: %v\n%s", err, out)
	}
}

func startSlice1Upgrade(t *testing.T) *slice1Upgrade {
	t.Helper()
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: []string{
		oldCRDs + "maul.run_intents.yaml", oldCRDs + "maul.run_previewverifications.yaml",
		newCRDs + "maul.run_projects.yaml",
	}}}
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
	return &slice1Upgrade{t: t, admin: admin, crds: crds, h: newHelm(t, env)}
}

// sliceOneCriteria is one slice-1 http criterion as the previous release
// stored it.
func sliceOneCriteria() []any {
	return []any{map[string]any{"id": "home", "kind": "http", "statement": "the home page answers",
		"target": "app", "steps": []any{map[string]any{
			"request": map[string]any{"method": "GET", "path": "/"},
			"expect":  map[string]any{"status": int64(200)}}}}}
}

// leaveSliceOneBehind creates what the previous release left: a verified
// Project, its terminal and open Intents recording criteria, a criteria
// PreviewVerification of the open one, and a settled outcome verification
// holding retired spellings.
func (u *slice1Upgrade) leaveSliceOneBehind() {
	t, ctx := u.t, u.t.Context()
	project := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "beatbattle", Namespace: egressRelease},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octocat"}},
			Repositories:     []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app"}},
			Preview: &v1alpha1.ProjectPreview{ImageRepository: "registry.example/maul/previews/app", Port: 8080,
				ReadinessPath: "/healthz"},
			Verification: &v1alpha1.ProjectVerification{Enabled: true},
		},
	}
	if err := u.admin.Create(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	status := func(phase v1alpha1.IntentPhase) map[string]any {
		return map[string]any{
			"phase": string(phase),
			"acceptance": map[string]any{"planRevision": int64(1), "digest": "sha256:" + strings.Repeat("a", 64),
				"criteria": sliceOneCriteria()},
			"verification": map[string]any{"state": "Failing",
				"failing": []any{map[string]any{"id": "home", "outcome": "fail"}},
				"autoFix": map[string]any{"state": "Halted", "reason": "CriterionError"}},
		}
	}
	for n, phase := range map[int64]v1alpha1.IntentPhase{47: v1alpha1.IntentMerged, 48: v1alpha1.IntentInReview} {
		in := attributedIntent(n, 0)
		name := v1alpha1.IntentName("beatbattle", n)
		in.Name, in.Spec.Project = name, "beatbattle"
		obj := toUnstructured(t, in)
		u.create(obj)
		obj.Object["status"] = status(phase)
		if err := u.admin.Status().Update(ctx, obj); err != nil {
			t.Fatalf("status of %s under the old CRDs: %v", name, err)
		}
	}
	u.create(toUnstructured(t, criteriaVerification("beatbattle-48-v1-a1")))
	settled := toUnstructured(t, outcomeVerification("beatbattle-49-v1-a1"))
	u.create(settled)
	settled.Object["status"] = map[string]any{"phase": "Stale", "reason": "stale-criteria",
		"environment": []any{map[string]any{"name": "tester", "role": "tester", "ready": true}}}
	if err := u.admin.Status().Update(ctx, settled); err != nil {
		t.Fatalf("status of the outcome verification under the old CRDs: %v", err)
	}
}

// criteriaVerification is a slice-1 criteria request, as the previous
// release's intent-controller wrote one.
func criteriaVerification(name string) *unstructured.Unstructured {
	obj := toUnstructured(nil, outcomeVerification(name))
	spec := obj.Object["spec"].(map[string]any)
	for _, k := range []string{"outcomes", "outcomesDigest", "rung", "tools"} {
		delete(spec, k)
	}
	spec["criteria"], spec["criteriaDigest"] = sliceOneCriteria(), "sha256:"+strings.Repeat("a", 64)
	return obj
}

// outcomeVerification is an outcome request of Intent beatbattle-49.
func outcomeVerification(name string) *v1alpha1.PreviewVerification {
	v := verification(name)
	v.Spec.IntentRef.Name, v.Spec.PreviewRef.Name = "beatbattle-49", "beatbattle-49"
	return v
}

func toUnstructured(t *testing.T, obj client.Object) *unstructured.Unstructured {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		if t == nil {
			panic(err)
		}
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	gvk, err := apiutil.GVKForObject(obj, kube.Scheme())
	if err != nil {
		panic(err)
	}
	u.SetGroupVersionKind(gvk)
	delete(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u
}

func (u *slice1Upgrade) create(obj *unstructured.Unstructured) {
	u.t.Helper()
	if err := u.admin.Create(u.t.Context(), obj); err != nil {
		u.t.Fatalf("create %s %s under the old CRDs: %v", obj.GetKind(), obj.GetName(), err)
	}
}

func (u *slice1Upgrade) get(kind, name string) *unstructured.Unstructured {
	u.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	if err := u.admin.Get(u.t.Context(), client.ObjectKey{Namespace: egressRelease, Name: name}, obj); err != nil {
		u.t.Fatalf("get %s %s: %v", kind, name, err)
	}
	return obj
}

// updatable: obj takes a metadata update and a status update under the new
// CRDs, as a finalizer's removal or a status write would.
func (u *slice1Upgrade) updatable(kind, name string) {
	u.t.Helper()
	obj := u.get(kind, name)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["maul.run/upgraded"] = "true"
	obj.SetLabels(labels)
	if err := u.admin.Update(u.t.Context(), obj); err != nil {
		u.t.Errorf("%s %s refuses a metadata update under the new CRDs: %v", kind, name, err)
		return
	}
	obj = u.get(kind, name)
	field := []string{"status", "completedAt"}
	if kind == "Intent" {
		field = []string{"status", "observedGeneration"}
	}
	value := any(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).Format(time.RFC3339))
	if kind == "Intent" {
		value = obj.GetGeneration()
	}
	if err := unstructured.SetNestedField(obj.Object, value, field...); err != nil {
		u.t.Fatal(err)
	}
	if err := u.admin.Status().Update(u.t.Context(), obj); err != nil {
		u.t.Errorf("%s %s refuses a status update under the new CRDs: %v", kind, name, err)
	}
}

// applyNewCRDs replaces the Intent and PreviewVerification CRDs with this
// release's, and waits until the API server serves them: a criteria
// verification is refused.
func (u *slice1Upgrade) applyNewCRDs() {
	t, ctx := u.t, u.t.Context()
	for _, file := range []string{"maul.run_intents.yaml", "maul.run_previewverifications.yaml"} {
		b, err := os.ReadFile(newCRDs + file)
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
		if err := u.crds.Get(ctx, client.ObjectKey{Name: want.Name}, &crd); err != nil {
			t.Fatal(err)
		}
		crd.Spec = want.Spec
		if err := u.crds.Update(ctx, &crd); err != nil {
			t.Fatalf("apply the new %s: %v", want.Name, err)
		}
	}
	deadline := time.Now().Add(time.Minute)
	for {
		err := u.admin.Create(ctx, criteriaVerification("beatbattle-52-v1-a1"), client.DryRunAll)
		if deniedWith(err, "slice 1's criteria are removed") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new PreviewVerification schema is not served after a minute: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// renderArgs renders the chart against the API server, its lookups real.
// The CRDs this test installed are not the release's, so the render leaves
// them out (a server dry run refuses to adopt them).
func (u *slice1Upgrade) renderArgs() []string {
	return []string{"template", "maul", ".", "--namespace", egressRelease, "--dry-run=server",
		"--set", "crds.install=false"}
}

// guardFails: the render fails, naming the criteria verification and the
// command that deletes it.
func (u *slice1Upgrade) guardFails(crds string) {
	u.t.Helper()
	out, err := u.h.run(u.renderArgs()...)
	if err == nil {
		u.t.Fatalf("with %s and a criteria verification left, the render passed", crds)
	}
	for _, want := range []string{"slice 1's acceptance criteria are removed", "beatbattle-48-v1-a1",
		"kubectl -n maul delete previewverification beatbattle-48-v1-a1"} {
		if !strings.Contains(out, want) {
			u.t.Errorf("with %s the render failed without naming %q:\n%s", crds, want, out)
		}
	}
	if strings.Contains(out, "beatbattle-49-v1-a1") {
		u.t.Errorf("with %s the guard names the outcome verification:\n%s", crds, out)
	}
}

// census: intent-controller's startup census, under the evaluator, finds
// exactly these records and refuses to start while it finds any.
func (u *slice1Upgrade) census(intents, verifications []string) {
	u.t.Helper()
	got, err := intent.TakeSlice1Census(u.t.Context(), u.admin, intent.Settings{Namespace: egressRelease,
		Verification: true, Previews: true, Evaluator: true})
	if err != nil {
		u.t.Fatalf("census: %v", err)
	}
	if !slices.Equal(got.Intents, intents) || !slices.Equal(got.Verifications, verifications) {
		u.t.Errorf("census = intents %v, verifications %v; want %v and %v", got.Intents, got.Verifications,
			intents, verifications)
	}
	if (got.Err() != nil) != (len(intents)+len(verifications) > 0) {
		u.t.Errorf("census refusal = %v with %d records", got.Err(), got.Count())
	}
}
