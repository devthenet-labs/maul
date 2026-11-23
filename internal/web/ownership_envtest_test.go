// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// The status server stamps spec.backfill/replay/reset on Integrations Helm
// applies. Each stamp must take ownership of its own field only: a full
// Update re-serialised the spec ("10m" reads back as "10m0s"), took the
// operator's durations and made the next helm upgrade conflict.

const serverManager = "maul-status-server"

// startOwnershipEnv starts an API server with the CRDs and returns an admin
// client and a Server whose writes land under the status server's field
// manager.
func startOwnershipEnv(t *testing.T) (client.Client, *Server) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping ownership envtest")
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
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build admin client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maul"}}
	if err := admin.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	srvCfg := rest.CopyConfig(cfg)
	srvCfg.UserAgent = serverManager
	srvClient, err := client.New(srvCfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build server client: %v", err)
	}
	s := NewServer(srvClient, "maul", stubAuth{id: operator}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }
	return admin, s
}

func TestIntegrationStampsLeaveHelmFieldsToHelm(t *testing.T) {
	admin, s := startOwnershipEnv(t)

	helm := func() error {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "Integration",
			"metadata":   map[string]any{"name": "gh", "namespace": "maul"},
			"spec": map[string]any{
				"provider":  "github",
				"interval":  "10m",
				"secretRef": map[string]any{"name": "github-app"},
				"github": map[string]any{
					"codeScanningAlerts": map[string]any{"enabled": true},
					"redelivery":         map[string]any{"enabled": true, "lookback": "24h"},
				},
			},
		}}
		return admin.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("helm"))
	}
	if err := helm(); err != nil {
		t.Fatalf("first helm apply: %v", err)
	}

	res, body := postBackfill(t, s, "gh", `{"repositories":["acme/"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("backfill status = %d (%s)", res.StatusCode, body)
	}
	if err := s.requestReplay(t.Context(), *operator); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := s.resetAll(t.Context(), *operator); err != nil {
		t.Fatalf("reset: %v", err)
	}

	var integ v1alpha1.Integration
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "maul", Name: "gh"}, &integ); err != nil {
		t.Fatalf("get integration: %v", err)
	}
	if integ.Spec.Backfill == nil || integ.Spec.Replay == nil || integ.Spec.Reset == nil {
		t.Fatalf("stamps missing: backfill %v replay %v reset %v",
			integ.Spec.Backfill, integ.Spec.Replay, integ.Spec.Reset)
	}
	want := []string{"backfill", "replay", "reset"}
	if got := ownedSpecFields(t, &integ, serverManager); !slices.Equal(got, want) {
		t.Errorf("status server owns spec fields %v, want only %v", got, want)
	}
	if err := helm(); err != nil {
		t.Fatalf("helm re-apply after the status server's stamps conflicted: %v", err)
	}
}

// TestFindingActionsLeaveControllerFieldsAlone covers the action handler's
// write. The Finding is applied by a controller with an approval timestamp
// in fractional seconds, which the API server stores as written and
// metav1.Time drops on a round trip: a full Update would change that value
// and take spec.approval from the controller, whose next apply conflicts.
func TestFindingActionsLeaveControllerFieldsAlone(t *testing.T) {
	admin, s := startOwnershipEnv(t)

	const controller = "integration-controller"
	apply := func() error {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "Finding",
			"metadata":   map[string]any{"name": "fnd-1", "namespace": "maul"},
			"spec": map[string]any{
				"integrationRef": map[string]any{"name": "gh"},
				"source":         "github-code-scanning",
				"advisories":     []any{"CVE-2026-0001"},
				"approval":       map[string]any{"by": "octocat", "at": "2026-07-24T09:00:00.250Z"},
			},
		}}
		return admin.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(controller))
	}
	if err := apply(); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	key := client.ObjectKey{Namespace: "maul", Name: "fnd-1"}
	var fnd v1alpha1.Finding
	if err := admin.Get(t.Context(), key, &fnd); err != nil {
		t.Fatalf("get finding: %v", err)
	}
	fnd.Status.Phase = v1alpha1.PhaseQueued
	if err := admin.Status().Update(t.Context(), &fnd); err != nil {
		t.Fatalf("seed finding status: %v", err)
	}

	for _, verb := range []string{"suspend", "resume", "expedite"} {
		if res, body := postAction(t, s, "fnd-1", verb); res.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d (%s)", verb, res.StatusCode, body)
		}
	}

	var got v1alpha1.Finding
	if err := admin.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if got.Spec.Expedite == nil || got.Spec.Suspend {
		t.Fatalf("spec = expedite %+v suspend %v, want expedited and resumed", got.Spec.Expedite, got.Spec.Suspend)
	}
	if fields, want := ownedSpecFields(t, &got, serverManager), []string{"expedite"}; !slices.Equal(fields, want) {
		t.Errorf("status server owns spec fields %v, want only %v", fields, want)
	}
	if err := apply(); err != nil {
		t.Fatalf("controller re-apply after the status server's actions conflicted: %v", err)
	}
}

// ownedSpecFields reports the top-level spec fields manager owns on obj.
func ownedSpecFields(t *testing.T, obj metav1.Object, manager string) []string {
	t.Helper()
	var out []string
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != manager || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]map[string]any
		if err := json.Unmarshal(mf.FieldsV1.GetRawBytes(), &fields); err != nil {
			t.Fatalf("decode managed fields of %s: %v", manager, err)
		}
		for k := range fields["f:spec"] {
			if name, ok := strings.CutPrefix(k, "f:"); ok {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// ---- The Project settings page's saves against an operator's manifest ----

// ownershipManifest is a Project as Helm applies it from an operator's
// values: charts/maul-config renders spec verbatim, so only what the values
// state, with the chart's labels, and Helm adds its managed-by label and
// release annotations to every object it installs. limits.maxActiveIntents
// is not rendered (the schema defaults it to 2); limits.build.timeout is,
// in the operator's own spelling, which a re-serialising write would take
// from Helm.
func ownershipManifest(name string, maxRevisions int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "Project",
		"metadata": map[string]any{
			"name": name, "namespace": "maul",
			"labels": map[string]any{"app": "intent-controller", "app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/part-of": "maul"},
			"annotations": map[string]any{"meta.helm.sh/release-name": "maul-config",
				"meta.helm.sh/release-namespace": "maul"},
		},
		"spec": map[string]any{
			"intentRepository": "https://github.com/acme/intents",
			"approvers":        map[string]any{"logins": []any{"octo", "bob"}},
			"repositories": []any{
				map[string]any{"name": "app", "url": "https://github.com/acme/app", "agentResourceClass": "small"},
				map[string]any{"name": "lib", "url": "https://github.com/acme/lib"},
			},
			"limits": map[string]any{"maxRevisions": maxRevisions, "build": map[string]any{"timeout": "10m"}},
			"checks": map[string]any{"fix": []any{"test", "lint"}},
		},
	}}
}

// ownershipCase is one Project on the envtest API server, applied by
// "helm" (server-side apply, as Helm 4 does by default) and saved through
// the settings page's POST handler by approverAlpha (octo).
type ownershipCase struct {
	t     *testing.T
	admin client.Client
	f     *saveFixture
	name  string
}

// newOwnershipCase applies name's manifest as helm, publishes what
// intent-controller would (octo resolved with write access, the models
// and the classes small and large admitted) and builds a settings server
// of its own, so no case meets another's rate limit. The server writes
// through the admin client, whose user agent is not the page's field
// manager: what the API server records is what the patch names.
func newOwnershipCase(t *testing.T, admin client.Client, name string) *ownershipCase {
	t.Helper()
	c := &ownershipCase{t: t, admin: admin, name: name}
	if err := c.apply(5, false); err != nil {
		t.Fatalf("helm install: %v", err)
	}
	p := c.project()
	p.Status = settingsProject().Status
	resolved := metav1.NewTime(testClock.Add(-time.Minute))
	p.Status.Approvers = &v1alpha1.ProjectApproversStatus{ResolvedAt: &resolved,
		Identities: []v1alpha1.ProjectApproverIdentity{{Login: "octo", ID: 101, Write: true}, {Login: "bob", ID: 2}}}
	if err := admin.Status().Update(t.Context(), p); err != nil {
		t.Fatalf("publish status: %v", err)
	}

	g := fixtureGranter()
	g.set(approverAlpha.Username, name, authz.TierIntents)
	s := NewServer(admin, "maul", stubAuth{}, stubGranter{grants: allGrants()}, nil)
	s.now = func() time.Time { return testClock }
	s = s.WithTranscripts(admin, nil).WithIntents(IntentsOptions{Projects: g}).
		WithProjectSettings(ProjectSettingsOptions{Policy: savePolicy()})
	c.f = &saveFixture{t: t, s: s, c: admin, g: g, logs: &syncBuffer{}}
	s.log = slog.New(slog.NewJSONHandler(c.f.logs, nil))
	return c
}

// apply re-applies the manifest as helm, stating maxRevisions, forced
// (helm upgrade --force-conflicts) or not (helm's default).
func (c *ownershipCase) apply(maxRevisions int64, force bool) error {
	opts := []client.ApplyOption{client.FieldOwner("helm")}
	if force {
		opts = append(opts, client.ForceOwnership)
	}
	return c.admin.Apply(c.t.Context(), client.ApplyConfigurationFromUnstructured(ownershipManifest(c.name,
		maxRevisions)), opts...)
}

// project is the Project as the API server has it, managedFields included.
func (c *ownershipCase) project() *v1alpha1.Project {
	c.t.Helper()
	var p v1alpha1.Project
	if err := c.admin.Get(c.t.Context(), client.ObjectKey{Namespace: "maul", Name: c.name}, &p); err != nil {
		c.t.Fatal(err)
	}
	return &p
}

// save posts the page's document as it is now with edit applied, and
// fails the test unless the server made the change.
func (c *ownershipCase) save(edit func(*projectsettings.ProjectSettings), confirm ...string) {
	c.t.Helper()
	p := c.project()
	req := projectsettings.ProjectSettingsRequest{Generation: p.Generation, Schema: projectsettings.Schema,
		Settings: projectsettings.Read(&p.Spec), Confirm: append([]string{}, confirm...)}
	edit(&req.Settings)
	raw, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	code, body := c.f.postTo(approverAlpha, "/api/projects/"+c.name+"/settings", "application/json", string(raw),
		"same-origin")
	res := decode[projectsettings.ProjectSettingsResult](c.t, body)
	if code != http.StatusOK || res.Reason != "" || len(res.Changes) == 0 {
		c.t.Fatalf("save of %s = %d %s", c.name, code, body)
	}
}

// leaves are the fields manager owns on the Project's main resource, one
// path per owned leaf: "f:" dropped, set elements as v:<value>, list-map
// entries as k:<key>, an entry owned as a whole by its own path.
func (c *ownershipCase) leaves(manager string) []string {
	c.t.Helper()
	return ownedLeaves(c.t, c.project(), manager)
}

func ownedLeaves(t *testing.T, obj metav1.Object, manager string) []string {
	t.Helper()
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if k == "." {
				out = append(out, prefix)
				continue
			}
			path := strings.TrimPrefix(k, "f:")
			if prefix != "" {
				path = prefix + "." + path
			}
			if child, _ := v.(map[string]any); len(child) > 0 {
				walk(path, child)
			} else {
				out = append(out, path)
			}
		}
	}
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != manager || mf.Subresource != "" || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(mf.FieldsV1.GetRawBytes(), &fields); err != nil {
			t.Fatalf("decode managed fields of %s: %v", manager, err)
		}
		walk("", fields)
	}
	slices.Sort(out)
	return out
}

// operations are the operations manager's main-resource entries record.
func operations(obj metav1.Object, manager string) []metav1.ManagedFieldsOperationType {
	var out []metav1.ManagedFieldsOperationType
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager == manager && mf.Subresource == "" {
			out = append(out, mf.Operation)
		}
	}
	return out
}

// helmLeaves are the leaves helm's apply of ownershipManifest owns.
var helmLeaves = []string{
	`metadata.annotations.meta.helm.sh/release-name`,
	`metadata.annotations.meta.helm.sh/release-namespace`,
	`metadata.labels.app`,
	`metadata.labels.app.kubernetes.io/managed-by`,
	`metadata.labels.app.kubernetes.io/part-of`,
	`spec.approvers.logins.v:"bob"`,
	`spec.approvers.logins.v:"octo"`,
	`spec.checks.fix.v:"lint"`,
	`spec.checks.fix.v:"test"`,
	`spec.intentRepository`,
	`spec.limits.build.timeout`,
	`spec.limits.maxRevisions`,
	`spec.repositories.k:{"name":"app"}`,
	`spec.repositories.k:{"name":"app"}.agentResourceClass`,
	`spec.repositories.k:{"name":"app"}.name`,
	`spec.repositories.k:{"name":"app"}.url`,
	`spec.repositories.k:{"name":"lib"}`,
	`spec.repositories.k:{"name":"lib"}.name`,
	`spec.repositories.k:{"name":"lib"}.url`,
}

// without is leaves less drop.
func without(leaves []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(leaves), func(l string) bool { return slices.Contains(drop, l) })
}

// edited is the save's record: its one annotation, beside Helm's own.
var edited = []string{"metadata.annotations." + projectsettings.EditedAnnotation}

// owns fails the test unless manager owns exactly want on the Project now.
func (c *ownershipCase) owns(manager string, want []string, when string) {
	c.t.Helper()
	if got := c.leaves(manager); !slices.Equal(got, want) {
		c.t.Errorf("%s, %s owns\n%q\nwant exactly\n%q", when, manager, got, want)
	}
}

// conflicts fails the test unless err is an apply conflict whose message
// holds every part in names and none in never.
func conflicts(t *testing.T, err error, names, never []string) {
	t.Helper()
	if !apierrors.IsConflict(err) {
		t.Fatalf("re-apply = %v, want a conflict", err)
	}
	for _, part := range names {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("conflict %q does not name %q", err, part)
		}
	}
	for _, part := range never {
		if strings.Contains(err.Error(), part) {
			t.Errorf("conflict %q names %q", err, part)
		}
	}
}

// TestProjectSettingsOwnOnlyEditedFields holds what the docs quote about a
// Project applied by Helm (server-side apply, manager helm, Helm 4's
// default) and edited on the settings page: the page's manager owns
// exactly the leaves it changed and its record annotation, and a later
// re-apply of the operator's unchanged manifest
//
//	(a) leaves a scalar the manifest does not render as the page set it,
//	    forced or not;
//	(b) FAILS on a scalar the manifest renders, unless forced, which puts
//	    the manifest's value back; "Copy as values" (the manifest stating
//	    the page's value) applies, and the next page edit conflicts again;
//	(c) silently restores a set element (approvers.logins, checks.fix) the
//	    page removed, and keeps one the page added, forced or not.
//
// The MEASURED log lines are the record W9 left for the docs (go test -v).
func TestProjectSettingsOwnOnlyEditedFields(t *testing.T) {
	admin, _ := startOwnershipEnv(t)
	t.Run("the page owns exactly what it edited", func(t *testing.T) { ownsExactlyWhatItEdited(t, admin) })
	t.Run("(a) a scalar the manifest does not render", func(t *testing.T) { unrenderedScalar(t, admin) })
	t.Run("(b) a scalar the manifest renders", func(t *testing.T) { renderedScalar(t, admin) })
	t.Run("(c) sets: approvers.logins and checks.fix", func(t *testing.T) { setElements(t, admin) })
}

// ownsExactlyWhatItEdited: one save of every kind of edit, then the
// manifest re-applied as it was, not forced and forced.
func ownsExactlyWhatItEdited(t *testing.T, admin client.Client) {
	c := newOwnershipCase(t, admin, "every")
	c.owns("helm", helmLeaves, "after the install")
	c.save(func(s *projectsettings.ProjectSettings) {
		mr := int32(7)
		s.Limits.MaxRevisions, s.Limits.MaxActiveIntents = &mr, 4
		s.Approvers = append(s.Approvers, "carol")
		s.Checks.Fix = []string{"test", "e2e"}
		s.Repositories[0].AgentResourceClass, s.Repositories[1].AgentResourceClass = "large", "large"
	}, "approver-add:carol")
	want := append(slices.Clone(edited),
		`spec.approvers.logins.v:"carol"`,
		`spec.checks.fix.v:"e2e"`,
		`spec.limits.maxActiveIntents`,
		`spec.limits.maxRevisions`,
		`spec.repositories.k:{"name":"app"}.agentResourceClass`,
		`spec.repositories.k:{"name":"lib"}.agentResourceClass`,
	)
	c.owns(projectsettings.FieldManager, want, "after the save")
	p := c.project()
	if ops := operations(p, projectsettings.FieldManager); !slices.Equal(ops,
		[]metav1.ManagedFieldsOperationType{metav1.ManagedFieldsOperationUpdate}) {
		t.Errorf("%s's operations %v, want one Update", projectsettings.FieldManager, ops)
	}
	// A changed leaf leaves helm; a removed set element (lint) belongs to
	// nobody; everything else, the operator's duration spelling included,
	// stays helm's.
	c.owns("helm", without(helmLeaves, `spec.checks.fix.v:"lint"`, `spec.limits.maxRevisions`,
		`spec.repositories.k:{"name":"app"}.agentResourceClass`), "after the save")
	if got := p.Spec.Limits.Build.Timeout; got == nil || got.Duration != 10*time.Minute {
		t.Errorf("build timeout %v, want the manifest's 10m", got)
	}

	// helm upgrade with the same values, default flags: refused, two
	// conflicts, the rendered scalars only, nothing applied.
	err := c.apply(5, false)
	t.Logf("MEASURED re-apply, not forced: %v", err)
	conflicts(t, err, []string{`Apply failed with 2 conflicts`, `conflicts with "maul-status-server"`,
		`.spec.limits.maxRevisions`, `.spec.repositories[name="app"].agentResourceClass`},
		[]string{"maxActiveIntents", "logins", "fix", `name="lib"`, "settings-edited"})
	if got := c.project().ResourceVersion; got != p.ResourceVersion {
		t.Errorf("a refused apply changed the Project: resourceVersion %s -> %s", p.ResourceVersion, got)
	}

	// helm upgrade --force-conflicts: the rendered scalars go back to the
	// manifest's and to helm; everything else the page did stays the page's.
	if err := c.apply(5, true); err != nil {
		t.Fatalf("forced re-apply: %v", err)
	}
	p = c.project()
	got := fmt.Sprintf("maxRevisions %d, maxActiveIntents %d, approvers %v, checks %v, classes %s/%s",
		*p.Spec.Limits.MaxRevisions, p.Spec.Limits.MaxActiveIntents, p.Spec.Approvers.Logins, p.Spec.Checks.Fix,
		p.Spec.Repositories[0].AgentResourceClass, p.Spec.Repositories[1].AgentResourceClass)
	t.Logf("MEASURED forced re-apply: %s", got)
	if want := "maxRevisions 5, maxActiveIntents 4, approvers [octo bob carol], checks [test e2e lint], " +
		"classes small/large"; got != want {
		t.Errorf("after the forced re-apply: %s\nwant %s", got, want)
	}
	c.owns(projectsettings.FieldManager, without(want, `spec.limits.maxRevisions`,
		`spec.repositories.k:{"name":"app"}.agentResourceClass`), "after the forced re-apply")
	c.owns("helm", helmLeaves, "after the forced re-apply")
}

// unrenderedScalar: (a), limits.maxActiveIntents.
func unrenderedScalar(t *testing.T, admin client.Client) {
	c := newOwnershipCase(t, admin, "unrendered")
	c.save(func(s *projectsettings.ProjectSettings) { s.Limits.MaxActiveIntents = 4 })
	want := append(slices.Clone(edited), `spec.limits.maxActiveIntents`)
	c.owns(projectsettings.FieldManager, want, "after the save")
	for _, force := range []bool{false, true} {
		err := c.apply(5, force)
		got := c.project().Spec.Limits.MaxActiveIntents
		t.Logf("MEASURED re-apply (forced %v): error %v, maxActiveIntents %d", force, err, got)
		if err != nil || got != 4 {
			t.Errorf("re-apply (forced %v) = %v, maxActiveIntents %d; want applied and the page's 4", force, err, got)
		}
		c.owns(projectsettings.FieldManager, want, fmt.Sprintf("after the re-apply (forced %v)", force))
	}
}

// renderedScalar: (b), limits.maxRevisions.
func renderedScalar(t *testing.T, admin client.Client) {
	c := newOwnershipCase(t, admin, "rendered")
	setRevisions := func(n int32) {
		c.save(func(s *projectsettings.ProjectSettings) { s.Limits.MaxRevisions = &n })
	}
	revisions := func() int32 { return *c.project().Spec.Limits.MaxRevisions }
	setRevisions(7)
	c.owns(projectsettings.FieldManager, append(slices.Clone(edited), `spec.limits.maxRevisions`), "after the save")
	c.owns("helm", without(helmLeaves, `spec.limits.maxRevisions`), "after the save")

	err := c.apply(5, false)
	t.Logf("MEASURED re-apply, not forced: %v", err)
	conflicts(t, err, []string{"Apply failed with 1 conflict", `conflict with "maul-status-server"`,
		".spec.limits.maxRevisions"}, nil)
	if got := revisions(); got != 7 {
		t.Errorf("maxRevisions %d after the refused apply, want the page's 7", got)
	}

	if err := c.apply(5, true); err != nil {
		t.Fatalf("forced re-apply: %v", err)
	}
	t.Logf("MEASURED forced re-apply: maxRevisions %d", revisions())
	if got := revisions(); got != 5 {
		t.Errorf("maxRevisions %d after the forced apply, want the manifest's 5", got)
	}
	c.owns(projectsettings.FieldManager, edited, "after the forced apply")
	c.owns("helm", helmLeaves, "after the forced apply")

	// "Copy as values": the page sets 7 again and the manifest states 7.
	// The apply succeeds and both own the leaf; the page's next edit makes
	// the same manifest conflict again.
	setRevisions(7)
	if err := c.apply(7, false); err != nil {
		t.Fatalf("re-apply stating the page's value = %v, want applied", err)
	}
	for _, m := range []string{"helm", projectsettings.FieldManager} {
		if !slices.Contains(c.leaves(m), `spec.limits.maxRevisions`) {
			t.Errorf("after copying the page's value into the manifest, %s does not own maxRevisions", m)
		}
	}
	setRevisions(6)
	err = c.apply(7, false)
	t.Logf("MEASURED re-apply stating 7 after the page set 6: %v", err)
	conflicts(t, err, []string{".spec.limits.maxRevisions"}, nil)
}

// setElements: (c), a login and a check the page added, and ones the
// manifest renders that the page removed (one login per save).
func setElements(t *testing.T, admin client.Client) {
	c := newOwnershipCase(t, admin, "sets")
	c.save(func(s *projectsettings.ProjectSettings) {
		s.Approvers = append(s.Approvers, "carol")
		s.Checks.Fix = []string{"test", "e2e"}
	}, "approver-add:carol")
	c.save(func(s *projectsettings.ProjectSettings) {
		s.Approvers = slices.DeleteFunc(s.Approvers, func(l string) bool { return l == "bob" })
	}, "approver-remove:bob")
	if got, want := c.project().Spec.Approvers.Logins, []string{"octo", "carol"}; !slices.Equal(got, want) {
		t.Fatalf("approvers %v after the saves, want %v", got, want)
	}
	wantPage := append(slices.Clone(edited), `spec.approvers.logins.v:"carol"`, `spec.checks.fix.v:"e2e"`)
	c.owns(projectsettings.FieldManager, wantPage, "after the saves")
	// The removed elements belong to nobody: neither manager names them.
	c.owns("helm", without(helmLeaves, `spec.approvers.logins.v:"bob"`, `spec.checks.fix.v:"lint"`),
		"after the saves")

	for _, force := range []bool{false, true} {
		err := c.apply(5, force)
		p := c.project()
		got := fmt.Sprintf("error %v, approvers %v, checks %v", err, p.Spec.Approvers.Logins, p.Spec.Checks.Fix)
		t.Logf("MEASURED re-apply (forced %v): %s", force, got)
		// bob and lint come back (last), carol and e2e stay: no conflict,
		// no warning.
		if want := "error <nil>, approvers [octo carol bob], checks [test e2e lint]"; got != want {
			t.Errorf("re-apply (forced %v): %s\nwant %s", force, got, want)
		}
		when := fmt.Sprintf("after the re-apply (forced %v)", force)
		c.owns(projectsettings.FieldManager, wantPage, when)
		c.owns("helm", helmLeaves, when)
	}
}
