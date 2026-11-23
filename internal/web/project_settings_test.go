// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/web/auth"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// The settings fixture's callers on top of the intents fixture's: an
// approver GitHub gives write access, and one it does not. Both see alpha at
// tier 1; viewerAlpha sees it too, signed in without a GitHub id.
var (
	approverAlpha = &auth.Identity{Username: "github:octo", DisplayName: "Octo", Session: true, GitHubID: 101}
	noWriteAlpha  = &auth.Identity{Username: "github:wil", DisplayName: "Wil", Session: true, GitHubID: 102}
)

const settingsPath = "/api/projects/alpha/settings"

// settingsProject is alpha as an uncached read gives it: approvers resolved
// a minute ago, what the controller admits published, conditions whose
// messages hold a private string, and Helm's managedFields.
func settingsProject() *v1alpha1.Project {
	mr := int32(3)
	resolved := metav1.NewTime(testClock.Add(-time.Minute))
	ceiling := func(turns int32, tokens int64) v1alpha1.ProjectStageCeiling {
		return v1alpha1.ProjectStageCeiling{MaxTurns: turns, TokenBudget: tokens,
			Timeout: &metav1.Duration{Duration: 20 * time.Minute}}
	}
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "maul", Generation: 4,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply,
				FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(
					`{"f:spec":{"f:limits":{"f:maxRevisions":{}},"f:approvers":{"f:logins":{"v:\"octo\"":{}}}}}`)}}}},
		Spec: v1alpha1.ProjectSpec{
			IntentRepository: "https://github.com/acme/intents\u202e",
			Approvers:        v1alpha1.ProjectApprovers{Logins: []string{"octo", "wil"}},
			Repositories: []v1alpha1.ProjectRepository{{Name: "app", URL: "https://github.com/acme/app",
				AgentResourceClass: "small"}},
			Limits: v1alpha1.ProjectLimits{MaxRevisions: &mr, MaxCostMicroUSD: 10_000_000,
				CheckReview: v1alpha1.StageLimits{MaxTurns: 4}},
			Models:       &v1alpha1.ProjectModels{Plan: "anthropic/claude-a", CheckReview: "anthropic/claude-b"},
			Verification: &v1alpha1.ProjectVerification{Enabled: true},
		},
		Status: v1alpha1.ProjectStatus{
			ObservedGeneration: 4,
			Approvers: &v1alpha1.ProjectApproversStatus{ResolvedAt: &resolved, Identities: []v1alpha1.ProjectApproverIdentity{
				{Login: "octo", ID: 101, Write: true}, {Login: "wil", ID: 102}}},
			Admits: &v1alpha1.ProjectAdmits{
				Models: []string{"anthropic/claude-a", "anthropic/claude-b"},
				Stages: v1alpha1.ProjectStageCeilings{Plan: ceiling(40, 200_000), Build: ceiling(150, 800_000),
					Revise: ceiling(80, 400_000), Evaluate: ceiling(100, 400_000), CheckReview: ceiling(6, 100_000)},
				Evaluator:       true,
				ResourceClasses: []v1alpha1.ProjectResourceClass{{Name: "small"}, {Name: "large"}},
			},
		},
	}
	for _, c := range []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Validated", Message: leakNode},
		{Type: "VerificationOff", Status: metav1.ConditionTrue, Reason: "EvaluatorOff", Message: leakNode},
		{Type: v1alpha1.ConditionApproversResolved, Status: metav1.ConditionTrue, Reason: "Resolved",
			Message: leakNode},
		{Type: "Unshown", Status: metav1.ConditionTrue, Reason: "Whatever", Message: leakNode},
	} {
		meta.SetStatusCondition(&p.Status.Conditions, c)
	}
	return p
}

// projectReader is the uncached reader: it serves the Projects it holds,
// managedFields included, and nothing else.
type projectReader struct {
	client.Reader
	projects map[string]*v1alpha1.Project
	gets     int
}

func (r *projectReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.gets++
	p, ok := r.projects[key.Name]
	out, isProject := obj.(*v1alpha1.Project)
	if !ok || !isProject || key.Namespace != "maul" {
		return apierrors.NewNotFound(schema.GroupResource{Group: "maul.run", Resource: "projects"}, key.Name)
	}
	p.DeepCopyInto(out)
	return nil
}

// settingsServer is the intents server with the settings page on: its
// cached client holds alpha without managedFields (the cache strips them),
// and the uncached reader holds alpha as the API server serves it.
func settingsServer(t *testing.T, o ProjectSettingsOptions) (*Server, *tierGranter, *projectReader) {
	t.Helper()
	cached := settingsProject()
	cached.ManagedFields = nil
	objs := []client.Object{cached}
	for _, obj := range intentsFixture(t) {
		if p, ok := obj.(*v1alpha1.Project); !ok || p.Name != "alpha" {
			objs = append(objs, obj)
		}
	}
	s, g := intentsServer(t, nil, objs...)
	g.set(approverAlpha.Username, "alpha", authz.TierIntents)
	g.set(noWriteAlpha.Username, "alpha", authz.TierIntents)
	r := &projectReader{projects: map[string]*v1alpha1.Project{"alpha": settingsProject()}}
	beta := settingsProject()
	beta.Name = "beta"
	r.projects["beta"] = beta
	s.reader = r
	return s.WithProjectSettings(o), g, r
}

func settingsView(t *testing.T, s *Server, id *auth.Identity) projectsettings.ProjectSettingsView {
	t.Helper()
	code, body := get(t, as(t, s, id), settingsPath)
	if code != http.StatusOK {
		t.Fatalf("GET %s as %s = %d %s", settingsPath, id.Username, code, body)
	}
	return decode[projectsettings.ProjectSettingsView](t, body)
}

// The page's routes exist only while it is on, and only beside the intents
// views, whose visibility it shares; /api/me says for which Projects it is.
func TestProjectSettingsRoutesOnlyWithThePage(t *testing.T) {
	off, _ := intentsServer(t, nil)
	ts := as(t, off, viewerAlpha)
	if code, _ := get(t, ts, settingsPath); code != http.StatusNotFound {
		t.Errorf("page off: GET = %d, want 404", code)
	}
	_, body := get(t, ts, "/api/me")
	for _, p := range decode[Me](t, body).Projects {
		if p.Settings {
			t.Errorf("page off: /api/me offers settings for %s", p.Name)
		}
	}

	lone := NewServer(nil, "maul", stubAuth{id: approverAlpha}, stubGranter{grants: allGrants()}, nil).
		WithProjectSettings(ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy()})
	if !lone.hardened() {
		t.Error("the settings page does not harden the browser envelope")
	}
	srv := httptest.NewServer(lone.Handler())
	t.Cleanup(srv.Close)
	if code, _ := get(t, srv, settingsPath); code != http.StatusNotFound {
		t.Errorf("page on without the intents views: GET = %d, want 404", code)
	}

	on, _, _ := settingsServer(t, ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy()})
	_, body = get(t, as(t, on, viewerAlpha), "/api/me")
	if got := decode[Me](t, body).Projects; !slices.Equal(got, []ProjectAccess{{Name: "alpha", Tier: "intents",
		Settings: true}}) {
		t.Errorf("page on: /api/me projects = %+v", got)
	}
	if code, _ := get(t, as(t, on, viewerAlpha), settingsPath); code != http.StatusOK {
		t.Errorf("page on: GET = %d", code)
	}
	res := request(t, http.MethodGet, as(t, on, viewerAlpha).URL+settingsPath, "same-site")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("a same-site read = %d, CSP %q: the hardened envelope must refuse it", res.StatusCode,
			res.Header.Get("Content-Security-Policy"))
	}
}

// Projects narrows the page to the Projects the status server may patch
// (the chart's statusServer.projectSettings.projects, the RBAC grant's
// resourceNames): for any other there is no page, and /api/me says so.
func TestProjectSettingsOnlyForTheListedProjects(t *testing.T) {
	s, g, _ := settingsServer(t, ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy(),
		Projects: []string{"beta"}})
	g.set(approverAlpha.Username, "beta", authz.TierIntents)
	ts := as(t, s, approverAlpha)
	if code, _ := get(t, ts, settingsPath); code != http.StatusNotFound {
		t.Errorf("alpha, not listed: GET = %d, want 404", code)
	}
	if code, _ := get(t, ts, "/api/projects/beta/settings"); code != http.StatusOK {
		t.Errorf("beta, listed: GET = %d, want 200", code)
	}
	_, body := get(t, ts, "/api/me")
	want := []ProjectAccess{{Name: "alpha", Tier: "intents"}, {Name: "beta", Tier: "intents", Settings: true}}
	if got := decode[Me](t, body).Projects; !slices.Equal(got, want) {
		t.Errorf("/api/me projects = %+v, want %+v", got, want)
	}
}

func TestProjectSettingsNeedsASignIn(t *testing.T) {
	s, _, _ := settingsServer(t, ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy()})
	if code, _ := get(t, as(t, s, nil), settingsPath); code != http.StatusUnauthorized {
		t.Errorf("no session: GET = %d, want 401", code)
	}
}

// A Project the caller may not see answers exactly as one that does not
// exist, and neither is read before visibility is known.
func TestProjectSettingsInvisibleIsMissing(t *testing.T) {
	s, _, r := settingsServer(t, ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy()})
	type answer struct {
		code int
		body string
	}
	ask := func(id *auth.Identity, path string) answer {
		code, body := get(t, as(t, s, id), path)
		return answer{code, body}
	}
	missing := ask(approverAlpha, "/api/projects/nosuch/settings")
	if missing.code != http.StatusNotFound {
		t.Fatalf("a missing Project = %d %s", missing.code, missing.body)
	}
	gets := r.gets
	for name, got := range map[string]answer{
		"alpha to a beta viewer":     ask(viewerBeta, settingsPath),
		"alpha to nobody":            ask(nobodyCaller, settingsPath),
		"a missing one to nobody":    ask(nobodyCaller, "/api/projects/nosuch/settings"),
		"a name that is no Project":  ask(approverAlpha, "/api/projects/Not_A_Name/settings"),
		"beta to an alpha approver:": ask(approverAlpha, "/api/projects/beta/settings"),
	} {
		if got != missing {
			t.Errorf("%s = %+v, want what a missing Project gets, %+v", name, got, missing)
		}
	}
	if r.gets != gets {
		t.Errorf("%d reads of a Project the caller may not see", r.gets-gets)
	}
}

// The view is the pure core's of the uncached read, with the Project's
// conditions in fixed wording and the operator's own words made fit to
// show; what the caller may do follows their GitHub id.
func TestProjectSettingsView(t *testing.T) {
	pol := projectsettings.DefaultPolicy()
	s, _, _ := settingsServer(t, ProjectSettingsOptions{Policy: pol})
	v := settingsView(t, s, approverAlpha)
	if !v.CanEdit || v.Reason != "" || v.Project != "alpha" || v.Generation != 4 {
		t.Errorf("an approver: canEdit %v reason %q project %q generation %d", v.CanEdit, v.Reason, v.Project,
			v.Generation)
	}
	want := []projectsettings.ProjectSettingsApprover{
		{Login: "octo", State: projectsettings.ApproverOK, You: true,
			Managers: []projectsettings.ProjectSettingsManager{{Name: "helm", Operation: projectsettings.ManagerApply}}},
		{Login: "wil", State: projectsettings.ApproverNoWrite, Managers: []projectsettings.ProjectSettingsManager{}},
	}
	if !equalJSON(t, v.Approvers, want) {
		t.Errorf("approvers = %+v, want %+v", v.Approvers, want)
	}
	field := func(id string) projectsettings.ProjectSettingsField {
		for _, f := range v.Fields {
			if f.ID == id {
				return f
			}
		}
		t.Fatalf("no field %s", id)
		return projectsettings.ProjectSettingsField{}
	}
	if f := field(projectsettings.FieldMaxRevisions); f.Ceiling != 10 || f.Max != 20 ||
		!equalJSON(t, f.Managers, []projectsettings.ProjectSettingsManager{{Name: "helm",
			Operation: projectsettings.ManagerApply}}) {
		t.Errorf("maxRevisions = %+v: the policy's ceiling and Helm's ownership, read uncached", f)
	}
	if f := field("limits.build.maxTurns"); f.Ceiling != 150 || f.Default != 150 {
		t.Errorf("build turns = %+v: the controller's ceiling", f)
	}
	wantConditions := []projectsettings.ProjectSettingsCondition{
		{Type: v1alpha1.ConditionReady, Status: "True", Reason: "Validated",
			Text: intentview.ProjectConditionText(v1alpha1.ConditionReady, "Validated")},
		{Type: "VerificationOff", Status: "True", Reason: "EvaluatorOff",
			Text: intentview.ProjectConditionText("VerificationOff", "EvaluatorOff")},
		{Type: v1alpha1.ConditionApproversResolved, Status: "True", Reason: "Resolved",
			Text: intentview.ProjectConditionText(v1alpha1.ConditionApproversResolved, "Resolved")},
	}
	if !equalJSON(t, v.Conditions, wantConditions) {
		t.Errorf("conditions = %+v, want %+v", v.Conditions, wantConditions)
	}
	_, body := get(t, as(t, s, approverAlpha), settingsPath)
	for _, leak := range []string{leakNode, "\u202e", "checkReview"} {
		if strings.Contains(body, leak) {
			t.Errorf("the view carries %q", leak)
		}
	}
	if !strings.HasPrefix(v.Facts.IntentRepository, "https://github.com/acme/intents") {
		t.Errorf("intent repository fact = %q", v.Facts.IntentRepository)
	}

	for id, reason := range map[*auth.Identity]string{
		viewerAlpha:  projectsettings.ReasonNoGitHubIdentity,
		noWriteAlpha: projectsettings.ReasonNoWriteAccess,
	} {
		if v := settingsView(t, s, id); v.CanEdit || v.Reason != reason {
			t.Errorf("%s: canEdit %v reason %q, want %q", id.Username, v.CanEdit, v.Reason, reason)
		}
	}
}

// Under --intents-dev-insecure (auth mode none) the page is served with no
// write path: nobody may save, the approver included.
func TestProjectSettingsReadOnly(t *testing.T) {
	s, _, _ := settingsServer(t, ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy(), ReadOnly: true})
	if v := settingsView(t, s, approverAlpha); v.CanEdit || v.Reason != SettingsReadOnly {
		t.Errorf("read-only page: canEdit %v reason %q, want %q", v.CanEdit, v.Reason, SettingsReadOnly)
	}
}

// The document the page is served is the live settings exactly: posted back
// unchanged it changes nothing, so nothing the server does to make text fit
// to show may touch it.
func TestProjectSettingsServedDocumentSavesAsNothing(t *testing.T) {
	pol := projectsettings.DefaultPolicy()
	s, _, r := settingsServer(t, ProjectSettingsOptions{Policy: pol})
	r.projects["alpha"].Spec.Checks.Fix = []string{"test\u202e", "lint"}
	_, body := get(t, as(t, s, approverAlpha), settingsPath)
	var served struct {
		Generation int64           `json:"generation"`
		Schema     int             `json:"schema"`
		Settings   json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal([]byte(body), &served); err != nil {
		t.Fatal(err)
	}
	req := projectsettings.ProjectSettingsRequest{Generation: served.Generation, Schema: served.Schema}
	dec := json.NewDecoder(strings.NewReader(string(served.Settings)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req.Settings); err != nil {
		t.Fatalf("the served settings are no document: %v", err)
	}
	d := projectsettings.Decide(r.projects["alpha"], req, pol, approverAlpha.GitHubID, testClock)
	if d.Patched != nil || d.Result.Reason != "" || len(d.Result.Changes) != 0 {
		t.Errorf("an unchanged save decided %+v, patched %v", d.Result, d.Patched != nil)
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}
