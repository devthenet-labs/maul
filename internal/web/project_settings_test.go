// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kfield "k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/kube"
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
	s, g, r := settingsServer(t, ProjectSettingsOptions{Policy: projectsettings.DefaultPolicy()})
	type answer struct {
		code int
		body string
	}
	ask := func(id *auth.Identity, path string) answer {
		code, body := get(t, as(t, s, id), path)
		return answer{code, body}
	}
	calls := g.calls
	missing := ask(approverAlpha, "/api/projects/nosuch/settings")
	if missing.code != http.StatusNotFound {
		t.Fatalf("a missing Project = %d %s", missing.code, missing.body)
	}
	// A name the caller chose that is no Project costs no access review.
	if g.calls != calls {
		t.Errorf("%d access reviews for a Project that does not exist", g.calls-calls)
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

// ---- The write path: POST /api/projects/{name}/settings ----

// savePolicy is the chart's default policy with the approver list editable,
// so an approver can be added (a change that needs a confirmation).
func savePolicy() projectsettings.Policy {
	pol := projectsettings.DefaultPolicy()
	pol.Allow.Approvers = true
	return pol
}

// patchCall is one Patch the server sent: the object's name, the patch's
// body and the field manager it was sent under.
type patchCall struct {
	name    string
	body    string
	manager string
}

// saveFixture is the settings page over one fake client that is both the
// manager's client and the uncached reader, with Project status a
// subresource as on the API server, and every Patch recorded. before runs
// ahead of a Patch (its call number from 1) and may answer it instead: a
// non-nil error is returned in place of the write.
type saveFixture struct {
	t       *testing.T
	s       *Server
	c       client.Client
	g       *tierGranter
	logs    *syncBuffer
	patches []patchCall
	before  func(n int, cl client.WithWatch, obj client.Object) error
}

func newSaveFixture(t *testing.T, o ProjectSettingsOptions, objs ...client.Object) *saveFixture {
	t.Helper()
	if objs == nil {
		p := settingsProject()
		p.ManagedFields = nil
		objs = []client.Object{p}
	}
	f := &saveFixture{t: t, logs: &syncBuffer{}}
	f.c = fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Project{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch,
				opts ...client.PatchOption) error {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				po := &client.PatchOptions{}
				po.ApplyOptions(opts)
				f.patches = append(f.patches, patchCall{name: obj.GetName(), body: string(data),
					manager: po.FieldManager})
				if f.before != nil {
					if err := f.before(len(f.patches), cl, obj); err != nil {
						return err
					}
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	f.g = fixtureGranter()
	f.g.set(approverAlpha.Username, "alpha", authz.TierIntents)
	f.g.set(noWriteAlpha.Username, "alpha", authz.TierIntents)
	f.g.set(strangerAlpha.Username, "alpha", authz.TierIntents)
	f.s = NewServer(f.c, "maul", stubAuth{}, stubGranter{grants: allGrants()},
		slog.New(slog.NewJSONHandler(f.logs, nil)))
	f.s.now = func() time.Time { return testClock }
	f.s = f.s.WithTranscripts(f.c, nil).WithIntents(IntentsOptions{Projects: f.g}).WithProjectSettings(o)
	return f
}

// strangerAlpha sees alpha and has a GitHub id the approver list resolved
// to no one.
var strangerAlpha = &auth.Identity{Username: "github:str", DisplayName: "Str", Session: true, GitHubID: 999}

// syncBuffer is a log sink the handler's goroutine writes and the test
// reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// lines are the JSON log records whose msg is msg.
func (b *syncBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(b.b.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// project is alpha as stored now.
func (f *saveFixture) project() *v1alpha1.Project {
	f.t.Helper()
	var p v1alpha1.Project
	if err := f.c.Get(f.t.Context(), client.ObjectKey{Namespace: "maul", Name: "alpha"}, &p); err != nil {
		f.t.Fatal(err)
	}
	return &p
}

// request is a save of alpha's settings as they are now, at its generation.
func (f *saveFixture) request() projectsettings.ProjectSettingsRequest {
	p := f.project()
	return projectsettings.ProjectSettingsRequest{Generation: p.Generation, Schema: projectsettings.Schema,
		Settings: projectsettings.Read(&p.Spec), Confirm: []string{}}
}

// post sends body to alpha's settings as id, from the page's own origin,
// and returns the status and the body.
func (f *saveFixture) post(id *auth.Identity, contentType, body string) (int, string) {
	f.t.Helper()
	return f.postTo(id, settingsPath, contentType, body, "same-origin")
}

func (f *saveFixture) postTo(id *auth.Identity, path, contentType, body, site string) (int, string) {
	f.t.Helper()
	ts := as(f.t, f.s, id)
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

// save posts req as id and decodes the answer, which every answer past
// the body checks is.
func (f *saveFixture) save(id *auth.Identity, req projectsettings.ProjectSettingsRequest) (int,
	projectsettings.ProjectSettingsResult) {
	f.t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		f.t.Fatal(err)
	}
	code, body := f.post(id, "application/json", string(raw))
	return code, decode[projectsettings.ProjectSettingsResult](f.t, body)
}

// Who may not edit is told why, as the gate's fixed key, with a 403 and
// nothing written; the read-only page has no write path for anyone.
func TestProjectSettingsSaveGateReasons(t *testing.T) {
	stale := func(p *v1alpha1.Project) {
		at := metav1.NewTime(testClock.Add(-projectsettings.ApproversMaxAge - time.Minute))
		p.Status.Approvers.ResolvedAt = &at
	}
	for _, tc := range []struct {
		name   string
		id     *auth.Identity
		mutate func(*v1alpha1.Project)
		want   string
	}{
		{"no GitHub id", viewerAlpha, nil, projectsettings.ReasonNoGitHubIdentity},
		{"not an approver", strangerAlpha, nil, projectsettings.ReasonNotApprover},
		{"no write access", noWriteAlpha, nil, projectsettings.ReasonNoWriteAccess},
		{"approvers unresolved", approverAlpha, func(p *v1alpha1.Project) { p.Status.Approvers = nil },
			projectsettings.ReasonApproversUnresolved},
		{"approvers stale", approverAlpha, stale, projectsettings.ReasonApproversStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := settingsProject()
			p.ManagedFields = nil
			if tc.mutate != nil {
				tc.mutate(p)
			}
			f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()}, p)
			req := f.request()
			mr := int32(5)
			req.Settings.Limits.MaxRevisions = &mr
			code, res := f.save(tc.id, req)
			if code != http.StatusForbidden || res.Reason != tc.want {
				t.Errorf("save = %d %q, want 403 %q", code, res.Reason, tc.want)
			}
			if len(f.patches) != 0 {
				t.Errorf("%d patches for a save the gate refused", len(f.patches))
			}
			if len(res.Changes) != 0 {
				t.Errorf("the refused account learns the changes: %+v", res.Changes)
			}
		})
	}

	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy(), ReadOnly: true})
	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusForbidden || res.Reason != SettingsReadOnly {
		t.Errorf("read-only page: save = %d %q, want 403 %q", code, res.Reason, SettingsReadOnly)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches on the read-only page", len(f.patches))
	}
}

// The gate asks the spec as well as the status: a login an operator took
// off the list while intent-controller has not resolved the list again
// still has its identity in status, and is no approver for it.
func TestProjectSettingsSaveLoginRemovedFromSpec(t *testing.T) {
	p := settingsProject()
	p.ManagedFields = nil
	p.Spec.Approvers.Logins = []string{"wil"}
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()}, p)
	if ids := f.project().Status.Approvers.Identities; len(ids) == 0 || ids[0].Login != "octo" || !ids[0].Write {
		t.Fatalf("the fixture's status no longer lists octo with write access: %+v", ids)
	}
	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusForbidden ||
		res.Reason != projectsettings.ReasonNotApprover {
		t.Errorf("save by a login no longer listed = %d %q, want 403 %q", code, res.Reason,
			projectsettings.ReasonNotApprover)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches for a login no longer listed", len(f.patches))
	}
}

// The document has no slot for a field outside the page's editable part:
// a body naming one (a repository's url, the harness, the labels, the
// check reviewer's model or limits) is refused as a 400 before anything is
// decided, and nothing is written.
func TestProjectSettingsSaveRefusesPinnedFields(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	before := f.project()
	good, err := json.Marshal(f.request())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(settings map[string]any)
	}{
		{"a repository's url", func(s map[string]any) {
			repo := s["repositories"].([]any)[0].(map[string]any)
			repo["url"] = "https://github.com/evil/app"
		}},
		{"the harness", func(s map[string]any) { s["harness"] = "codex" }},
		{"the labels", func(s map[string]any) { s["labels"] = map[string]any{"trigger": "go"} }},
		{"the check reviewer's model", func(s map[string]any) {
			s["models"].(map[string]any)["checkReview"] = "anthropic/claude-b"
		}},
		{"the check reviewer's limits", func(s map[string]any) {
			s["stages"].(map[string]any)["checkReview"] = map[string]any{"maxTurns": 50}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(good, &doc); err != nil {
				t.Fatal(err)
			}
			tc.edit(doc["settings"].(map[string]any))
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) == string(good) {
				t.Fatal("the body names nothing the good one does not")
			}
			if code, out := f.post(approverAlpha, "application/json", string(body)); code != http.StatusBadRequest {
				t.Errorf("POST = %d %s, want 400", code, out)
			}
		})
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches for bodies naming pinned fields", len(f.patches))
	}
	if after := f.project(); after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
}

// A save is one application/json object of at most 16 KiB naming only the
// document's fields; anything else is refused before the Project is read
// for a decision, and nothing is written.
func TestProjectSettingsSaveRefusesBadBodies(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	good, err := json.Marshal(f.request())
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(good), `"schema":`, `"surprise":1,"schema":`, 1)
	nested := strings.Replace(string(good), `"maxRevisions":`, `"maxRevisionz":1,"maxRevisions":`, 1)
	big := `{"generation":1,"schema":1,"confirm":["` + strings.Repeat("x", 17<<10) + `"]}`
	for _, tc := range []struct {
		name, contentType, body string
		want                    int
	}{
		{"no content type", "", string(good), http.StatusUnsupportedMediaType},
		{"a form", "application/x-www-form-urlencoded", string(good), http.StatusUnsupportedMediaType},
		{"text", "text/plain", string(good), http.StatusUnsupportedMediaType},
		{"an unknown field", "application/json", unknown, http.StatusBadRequest},
		{"an unknown nested field", "application/json", nested, http.StatusBadRequest},
		{"two objects", "application/json", string(good) + string(good), http.StatusBadRequest},
		{"trailing data", "application/json", string(good) + "}", http.StatusBadRequest},
		{"not JSON", "application/json", "settings please", http.StatusBadRequest},
		{"too large", "application/json", big, http.StatusRequestEntityTooLarge},
	} {
		if code, body := f.post(approverAlpha, tc.contentType, tc.body); code != tc.want {
			t.Errorf("%s: POST = %d %s, want %d", tc.name, code, body, tc.want)
		}
	}
	if code, body := f.post(approverAlpha, "application/json; charset=utf-8", string(good)); code != http.StatusOK {
		t.Errorf("application/json with a charset: POST = %d %s, want 200", code, body)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches for refused bodies and an unchanged save", len(f.patches))
	}
}

// Each person may save ten times a minute; the eleventh is a 429, which
// spends nothing of anyone else's.
func TestProjectSettingsSaveRateLimited(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	now := testClock
	f.s.now = func() time.Time { return now }
	raw, err := json.Marshal(f.request())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if code, body := f.post(approverAlpha, "application/json", string(raw)); code != http.StatusOK {
			t.Fatalf("save %d = %d %s", i+1, code, body)
		}
		now = now.Add(time.Second)
	}
	if code, _ := f.post(approverAlpha, "application/json", string(raw)); code != http.StatusTooManyRequests {
		t.Errorf("the eleventh save in a minute = %d, want 429", code)
	}
	if code, _ := f.post(approverAlpha, "text/plain", "x"); code != http.StatusTooManyRequests {
		t.Errorf("a refused body counts too: = %d, want 429", code)
	}
	if code, res := f.save(noWriteAlpha, f.request()); code != http.StatusForbidden ||
		res.Reason != projectsettings.ReasonNoWriteAccess {
		t.Errorf("another person = %d %q: their budget is their own", code, res.Reason)
	}
	now = testClock.Add(time.Minute + time.Second)
	if code, body := f.post(approverAlpha, "application/json", string(raw)); code != http.StatusOK {
		t.Errorf("a minute after the first save = %d %s, want 200", code, body)
	}
}

// A save made on a page of another generation or schema is refused with a
// 409, so the page reloads; nothing is merged.
func TestProjectSettingsSaveStalePage(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	mr := int32(5)
	req := f.request()
	req.Settings.Limits.MaxRevisions = &mr
	req.Generation--
	if code, res := f.save(approverAlpha, req); code != http.StatusConflict ||
		res.Reason != projectsettings.ResultGenerationChanged {
		t.Errorf("an old generation = %d %q, want 409 %q", code, res.Reason, projectsettings.ResultGenerationChanged)
	}
	req = f.request()
	req.Settings.Limits.MaxRevisions = &mr
	req.Schema++
	if code, res := f.save(approverAlpha, req); code != http.StatusConflict ||
		res.Reason != projectsettings.ResultSchemaChanged {
		t.Errorf("another schema = %d %q, want 409 %q", code, res.Reason, projectsettings.ResultSchemaChanged)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches for stale pages", len(f.patches))
	}
}

// A change the policy refuses is a 422 naming the field; a document the
// page could not have posted is a 400; neither writes.
func TestProjectSettingsSaveRefused(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	req := f.request()
	mr := int32(15)
	req.Settings.Limits.MaxRevisions = &mr
	code, res := f.save(approverAlpha, req)
	want := []projectsettings.ProjectSettingsRefusal{{Field: projectsettings.FieldMaxRevisions,
		Reason: projectsettings.RefusedAboveCeiling, Limit: 10}}
	if code != http.StatusUnprocessableEntity || res.Reason != projectsettings.ResultRefused ||
		!equalJSON(t, res.Refusals, want) {
		t.Errorf("above the ceiling = %d %+v, want 422 %+v", code, res, want)
	}
	req = f.request()
	req.Settings.Repositories = append(req.Settings.Repositories,
		projectsettings.ProjectSettingsRepository{Name: "nosuch", AgentResourceClass: "small"})
	code, res = f.save(approverAlpha, req)
	if code != http.StatusBadRequest || res.Reason != projectsettings.ResultInvalidDocument ||
		len(res.Refusals) != 1 || res.Refusals[0].Reason != projectsettings.DocumentUnknownRepository {
		t.Errorf("an unknown repository = %d %+v, want 400 %s", code, res, projectsettings.DocumentUnknownRepository)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches for refused saves", len(f.patches))
	}
}

// A delegation switch is turned on only where intent-controller allows it:
// refused with no write while status.admits says no, saved once it says
// yes.
func TestProjectSettingsSaveDelegation(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	req := f.request()
	req.Settings.Delegation.Subagents = true
	code, res := f.save(approverAlpha, req)
	want := []projectsettings.ProjectSettingsRefusal{{Field: projectsettings.FieldSubagents,
		Reason: projectsettings.RefusedDelegationNotAdmitted}}
	if code != http.StatusUnprocessableEntity || res.Reason != projectsettings.ResultRefused ||
		!equalJSON(t, res.Refusals, want) {
		t.Errorf("subagents on, not allowed = %d %+v, want 422 %+v", code, res, want)
	}
	if len(f.patches) != 0 {
		t.Fatalf("%d patches for a refused save", len(f.patches))
	}
	p := f.project()
	p.Status.Admits.Subagents = true
	if err := f.c.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	req = f.request()
	req.Settings.Delegation.Subagents = true
	if code, res = f.save(approverAlpha, req); code != http.StatusOK || res.Reason != "" {
		t.Fatalf("subagents on, allowed = %d %+v, want 200", code, res)
	}
	if got := f.project(); !got.Spec.SubagentsEnabled() || got.Spec.WorkflowsEnabled() {
		t.Errorf("stored delegation = %+v, want subagents alone", got.Spec.Delegation)
	}
}

// A weakening change is answered with the confirmations it needs; posted
// again with them it is saved, and the answer carries the generation the
// page waits for.
func TestProjectSettingsSaveConfirmThenSave(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	req := f.request()
	req.Settings.Approvers = append(req.Settings.Approvers, "carol")
	code, res := f.save(approverAlpha, req)
	if code != http.StatusConflict || res.Reason != projectsettings.ResultConfirmRequired || len(res.Confirm) != 1 {
		t.Fatalf("adding an approver unconfirmed = %d %+v, want 409 with one confirmation", code, res)
	}
	if len(f.patches) != 0 {
		t.Fatalf("%d patches before the confirmation", len(f.patches))
	}
	req.Confirm = []string{res.Confirm[0].Key}
	code, res = f.save(approverAlpha, req)
	if code != http.StatusOK || res.Reason != "" || len(res.Changes) != 1 {
		t.Fatalf("confirmed = %d %+v, want 200 with the one change", code, res)
	}
	p := f.project()
	if !slices.Equal(p.Spec.Approvers.Logins, []string{"octo", "wil", "carol"}) {
		t.Errorf("approvers = %v after the save", p.Spec.Approvers.Logins)
	}
	if res.Generation != p.Generation {
		t.Errorf("answer generation %d, stored %d", res.Generation, p.Generation)
	}
	e, ok := projectsettings.ParseEdit(p.Annotations[projectsettings.EditedAnnotation])
	if !ok || e.GitHubID != approverAlpha.GitHubID || e.Login != "octo" ||
		!slices.Equal(e.Fields, []string{projectsettings.FieldApprovers}) {
		t.Errorf("edit record = %+v, %v", e, ok)
	}
}

// Posting the settings unchanged is a 200 that writes nothing and logs no
// change.
func TestProjectSettingsSaveUnchangedWritesNothing(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	before := f.project()
	code, res := f.save(approverAlpha, f.request())
	if code != http.StatusOK || res.Reason != "" || len(res.Changes) != 0 || res.Generation != before.Generation {
		t.Errorf("an unchanged save = %d %+v", code, res)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches for an unchanged save", len(f.patches))
	}
	if after := f.project(); after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if got := f.logs.lines(t, "project settings changed"); len(got) != 0 {
		t.Errorf("an unchanged save logged %v", got)
	}
}

// The write is a JSON merge patch of exactly the changed leaves, the edit
// record and the resourceVersion it was decided at, under the page's own
// field manager: never the whole spec, never status.
func TestProjectSettingsSavePatchBody(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	live := f.project()
	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	req.Settings.Repositories = []projectsettings.ProjectSettingsRepository{{Name: "app", AgentResourceClass: "large"}}
	if code, res := f.save(approverAlpha, req); code != http.StatusOK || res.Reason != "" {
		t.Fatalf("save = %d %+v", code, res)
	}
	if len(f.patches) != 1 {
		t.Fatalf("%d patches, want 1", len(f.patches))
	}
	got := f.patches[0]
	if got.manager != projectsettings.FieldManager {
		t.Errorf("field manager %q, want %q", got.manager, projectsettings.FieldManager)
	}
	changes := []projectsettings.ProjectSettingsChange{{Field: projectsettings.FieldMaxRevisions},
		{Field: projectsettings.FieldRepositoryClass}}
	edit := projectsettings.EncodeEdit(projectsettings.NewEdit(approverAlpha.GitHubID, "octo", testClock, changes))
	want := map[string]any{
		"metadata": map[string]any{
			"annotations":     map[string]any{projectsettings.EditedAnnotation: edit},
			"resourceVersion": live.ResourceVersion,
		},
		"spec": map[string]any{
			"limits": map[string]any{"maxRevisions": float64(5)},
			"repositories": []any{map[string]any{"name": "app", "url": "https://github.com/acme/app",
				"agentResourceClass": "large"}},
		},
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(t, body, want) {
		t.Errorf("patch body\n got %s\nwant %s", got.body, mustJSON(t, want))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The controller writes status every poll, which moves the resourceVersion
// a save was decided at: the write conflicts, and the save is read and
// decided again, the gate included.
func TestProjectSettingsSaveRetriesAStatusConflict(t *testing.T) {
	bump := func(revoke bool) func(int, client.WithWatch, client.Object) error {
		return func(n int, cl client.WithWatch, obj client.Object) error {
			if n != 1 {
				return nil
			}
			var cur v1alpha1.Project
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), &cur); err != nil {
				return err
			}
			at := metav1.NewTime(testClock)
			cur.Status.LastPolledAt = &at
			if revoke {
				cur.Status.Approvers.Identities[0].Write = false
			}
			return cl.Status().Update(context.Background(), &cur)
		}
	}
	mr := int32(5)

	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	f.before = bump(false)
	req := f.request()
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusOK || res.Reason != "" {
		t.Errorf("save over a status write = %d %+v, want 200 after the retry", code, res)
	}
	if len(f.patches) != 2 {
		t.Errorf("%d patches, want the conflicted one and its retry", len(f.patches))
	}
	if got := f.project().Spec.Limits.MaxRevisions; got == nil || *got != 5 {
		t.Errorf("maxRevisions = %v after the retry", got)
	}

	f = newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	f.before = bump(true)
	req = f.request()
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusForbidden ||
		res.Reason != projectsettings.ReasonNoWriteAccess {
		t.Errorf("save while write access went = %d %+v, want 403 %s", code, res, projectsettings.ReasonNoWriteAccess)
	}
	if got := f.project().Spec.Limits.MaxRevisions; got == nil || *got != 3 {
		t.Errorf("maxRevisions = %v: written for someone who lost write access", got)
	}

	f = newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	f.before = func(int, client.WithWatch, client.Object) error {
		return apierrors.NewConflict(schema.GroupResource{Group: "maul.run", Resource: "projects"}, "alpha", nil)
	}
	req = f.request()
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusConflict || res.Reason != SettingsConflict {
		t.Errorf("conflicts every time = %d %+v, want 409 %s", code, res, SettingsConflict)
	}
}

// Another approver's save lands between this one's read and its patch: the
// write conflicts, the retry reads the Project again and decides again, and
// the spec's generation has moved since the page was served, so the answer
// is a 409 that makes the page reload. The other save stands, and this one
// writes nothing over it.
func TestProjectSettingsSaveRacedByAnotherSave(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	start := f.project()
	f.before = func(n int, cl client.WithWatch, obj client.Object) error {
		if n != 1 {
			return nil
		}
		var cur v1alpha1.Project
		if err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), &cur); err != nil {
			return err
		}
		cur.Spec.Limits.MaxCheckFixes = new(int32(7))
		// The fake client keeps metadata.generation as it is given; the API
		// server bumps it on a spec change.
		cur.Generation++
		return cl.Update(context.Background(), &cur)
	}
	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	code, res := f.save(approverAlpha, req)
	if code != http.StatusConflict || res.Reason != projectsettings.ResultGenerationChanged {
		t.Errorf("save raced by another = %d %+v, want 409 %q", code, res, projectsettings.ResultGenerationChanged)
	}
	if len(f.patches) != 1 {
		t.Errorf("%d patches, want the one that conflicted and no retry write", len(f.patches))
	}
	stored := f.project()
	if stored.Generation != start.Generation+1 {
		t.Fatalf("generation %d -> %d: the race did not move it", start.Generation, stored.Generation)
	}
	if got := stored.Spec.Limits.MaxCheckFixes; got == nil || *got != 7 {
		t.Errorf("maxCheckFixes = %v: the other approver's save was lost", got)
	}
	if got := stored.Spec.Limits.MaxRevisions; got == nil || *got != 3 {
		t.Errorf("maxRevisions = %v: written over a save it never saw", got)
	}
}

// The API server is the authority on the schema and its cross-field rules,
// which the page does not check first: its Invalid answer is told as a
// refusal of each changed field at or below the path it names, and an
// answer naming none still refuses the save.
func TestProjectSettingsSaveAPIInvalid(t *testing.T) {
	gr := schema.GroupKind{Group: "maul.run", Kind: "Project"}
	for _, tc := range []struct {
		name   string
		causes kfield.ErrorList
		want   []projectsettings.ProjectSettingsRefusal
	}{
		{"a cross-field rule on limits", kfield.ErrorList{kfield.Invalid(kfield.NewPath("spec", "limits"), "object",
			"limits.plan.maxCostMicroUSD must not exceed limits.maxCostMicroUSD")},
			[]projectsettings.ProjectSettingsRefusal{
				{Field: projectsettings.FieldMaxCost, Reason: projectsettings.RefusedInvalid},
				{Field: "limits.plan.maxCostMicroUSD", Reason: projectsettings.RefusedInvalid},
			}},
		{"a leaf", kfield.ErrorList{kfield.Invalid(kfield.NewPath("spec", "limits", "maxCostMicroUSD"), 1, "too small")},
			[]projectsettings.ProjectSettingsRefusal{
				{Field: projectsettings.FieldMaxCost, Reason: projectsettings.RefusedInvalid}}},
		{"a repository's class", kfield.ErrorList{kfield.Invalid(
			kfield.NewPath("spec", "repositories").Index(0).Child("agentResourceClass"), "huge", "no")},
			[]projectsettings.ProjectSettingsRefusal{{Field: projectsettings.FieldRepositoryClass, Item: "app",
				Reason: projectsettings.RefusedInvalid}}},
		{"a path no change is under", kfield.ErrorList{kfield.Invalid(kfield.NewPath("spec", "surfaces"), "x", "no")},
			[]projectsettings.ProjectSettingsRefusal{{Field: "surfaces", Reason: projectsettings.RefusedInvalid}}},
		// How the API server words an admission policy's denial (SPIKE
		// (d): `The projects "p" is invalid: : ValidatingAdmissionPolicy
		// ... denied request: ...`): one cause with no field.
		{"an admission policy's denial", kfield.ErrorList{kfield.Invalid(kfield.NewPath(""), "x",
			"ValidatingAdmissionPolicy 'p' with binding 'p' denied request: no")},
			[]projectsettings.ProjectSettingsRefusal{{Reason: projectsettings.RefusedInvalid}}},
		{"no causes at all", nil,
			[]projectsettings.ProjectSettingsRefusal{{Reason: projectsettings.RefusedInvalid}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
			f.before = func(int, client.WithWatch, client.Object) error {
				return apierrors.NewInvalid(gr, "alpha", tc.causes)
			}
			req := f.request()
			req.Settings.Limits.MaxCostMicroUSD = 2_000_000
			req.Settings.Stages.Plan.MaxCostMicroUSD = 1_000_000
			req.Settings.Repositories = []projectsettings.ProjectSettingsRepository{{Name: "app",
				AgentResourceClass: "large"}}
			code, res := f.save(approverAlpha, req)
			if code != http.StatusUnprocessableEntity || res.Reason != projectsettings.ResultRefused {
				t.Fatalf("save = %d %+v, want 422 %s", code, res, projectsettings.ResultRefused)
			}
			for i := range res.Refusals {
				res.Refusals[i].Limit = 0
			}
			if !equalJSON(t, res.Refusals, tc.want) {
				t.Errorf("refusals = %+v, want %+v", res.Refusals, tc.want)
			}
			if len(res.Changes) != 3 {
				t.Errorf("changes = %+v: the page shows what was refused", res.Changes)
			}
			if strings.Contains(mustJSON(t, res), "must not exceed") {
				t.Error("the API server's message reached the page")
			}
			if got := f.logs.lines(t, "project settings refused"); len(got) != 1 ||
				got[0]["reason"] != SettingsAPIInvalid {
				t.Errorf("refusal log = %v", got)
			}
		})
	}
}

// A write the API server forbids (the patch grant's resourceNames, an
// admission policy denying as Forbidden) is a 403 of its own reason, with
// nothing written and the refusal recorded.
func TestProjectSettingsSaveForbiddenByTheCluster(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	f.before = func(int, client.WithWatch, client.Object) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "maul.run", Resource: "projects"}, "alpha",
			errors.New("denied"))
	}
	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusForbidden || res.Reason != SettingsClusterRefused {
		t.Errorf("forbidden write = %d %+v, want 403 %s", code, res, SettingsClusterRefused)
	}
	if got := f.project().Spec.Limits.MaxRevisions; got == nil || *got != 3 {
		t.Errorf("maxRevisions = %v", got)
	}
	if got := f.logs.lines(t, "project settings refused"); len(got) != 1 || got[0]["reason"] != SettingsClusterRefused {
		t.Errorf("refusal lines = %v", got)
	}
}

// Every save is recorded: a made change as "project settings changed" with
// the editor's account, the login it resolved to, the generations and each
// change; a refusal as "project settings refused" with its reason; both
// counted by result.
func TestProjectSettingsSaveAudit(t *testing.T) {
	settingsMetrics()
	changed := settingsWrites(t, "changed")
	refused := settingsWrites(t, projectsettings.ResultRefused)
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy()})
	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	if code, res := f.save(approverAlpha, req); code != http.StatusOK {
		t.Fatalf("save = %d %+v", code, res)
	}
	lines := f.logs.lines(t, "project settings changed")
	if len(lines) != 1 {
		t.Fatalf("change lines = %v", lines)
	}
	got := lines[0]
	want := map[string]any{"project": "alpha", "user": approverAlpha.Username, "github_id": float64(101),
		"login": "octo", "generation": float64(4),
		"changes": []any{map[string]any{"field": projectsettings.FieldMaxRevisions, "item": "", "from": "3",
			"to": "5"}}}
	for k, v := range want {
		if !equalJSON(t, got[k], v) {
			t.Errorf("audit %s = %v, want %v", k, got[k], v)
		}
	}
	if settingsWrites(t, "changed") != changed+1 {
		t.Error("the change was not counted")
	}

	mr = 15
	req = f.request()
	req.Settings.Limits.MaxRevisions = &mr
	if code, _ := f.save(approverAlpha, req); code != http.StatusUnprocessableEntity {
		t.Fatalf("refused save = %d", code)
	}
	lines = f.logs.lines(t, "project settings refused")
	if len(lines) != 1 || lines[0]["reason"] != projectsettings.ResultRefused || lines[0]["user"] !=
		approverAlpha.Username || lines[0]["project"] != "alpha" {
		t.Errorf("refusal lines = %v", lines)
	}
	if settingsWrites(t, projectsettings.ResultRefused) != refused+1 {
		t.Error("the refusal was not counted")
	}
}

// settingsMetrics installs, once per test binary, a ManualReader-backed
// global MeterProvider the package's instruments report to.
var settingsMetrics = sync.OnceValue(func() *sdkmetric.ManualReader {
	r := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r)))
	return r
})

// settingsWrites is maul.status.project_settings.writes for one result.
func settingsWrites(t *testing.T, result string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := settingsMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "maul.status.project_settings.writes" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("result")); ok && v.AsString() == result {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// A save of a Project the caller may not see, one that does not exist and
// one the page is not for answer as the read does, and write nothing; a
// save without a session is a 401.
func TestProjectSettingsSaveInvisibleIsMissing(t *testing.T) {
	f := newSaveFixture(t, ProjectSettingsOptions{Policy: savePolicy(), Projects: []string{"alpha"}})
	raw, err := json.Marshal(f.request())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		id   *auth.Identity
		path string
	}{
		"invisible":   {nobodyCaller, settingsPath},
		"missing":     {approverAlpha, "/api/projects/nosuch/settings"},
		"not listed":  {approverAlpha, "/api/projects/beta/settings"},
		"not a name":  {approverAlpha, "/api/projects/Not_A_Name/settings"},
		"to a viewer": {viewerBeta, settingsPath},
	} {
		if code, body := f.postTo(tc.id, tc.path, "application/json", string(raw), "same-origin"); code !=
			http.StatusNotFound {
			t.Errorf("%s: POST = %d %s, want 404", name, code, body)
		}
	}
	if code, _ := f.postTo(nil, settingsPath, "application/json", string(raw), "same-origin"); code !=
		http.StatusUnauthorized {
		t.Errorf("no session: POST = %d, want 401", code)
	}
	if len(f.patches) != 0 {
		t.Errorf("%d patches", len(f.patches))
	}
}

// Through a real API server: a save bumps the generation and the answer
// carries the new one, the edit lands under the page's field manager, and
// a cross-field rule the page does not check first (a per-intent cost
// under a stage's) comes back as a refusal of the fields that moved.
func TestProjectSettingsSaveThroughARealAPIServer(t *testing.T) {
	admin, s := startOwnershipEnv(t)
	p := settingsProject()
	p.ManagedFields, p.Status, p.ResourceVersion = nil, v1alpha1.ProjectStatus{}, ""
	p.Spec.IntentRepository = "https://github.com/acme/intents"
	p.Spec.Limits.Plan.MaxCostMicroUSD = 3_000_000
	if err := admin.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	st := settingsProject().Status
	p.Status = st
	if err := admin.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	g := fixtureGranter()
	g.set(approverAlpha.Username, "alpha", authz.TierIntents)
	// The admin client, not startOwnershipEnv's: that one's user agent is
	// maul-status-server, which the API server would record as the manager
	// even if the patch named none.
	s.client = admin
	s = s.WithTranscripts(admin, nil).WithIntents(IntentsOptions{Projects: g}).
		WithProjectSettings(ProjectSettingsOptions{Policy: savePolicy()})
	f := &saveFixture{t: t, s: s, c: admin, g: g, logs: &syncBuffer{}}
	s.log = slog.New(slog.NewJSONHandler(f.logs, nil))

	req := f.request()
	mr := int32(5)
	req.Settings.Limits.MaxRevisions = &mr
	code, res := f.save(approverAlpha, req)
	stored := f.project()
	if code != http.StatusOK || res.Generation != req.Generation+1 || stored.Generation != res.Generation {
		t.Fatalf("save = %d %+v; generation sent %d, stored %d", code, res, req.Generation, stored.Generation)
	}
	owned := false
	for _, m := range stored.ManagedFields {
		if m.Manager == projectsettings.FieldManager && m.FieldsV1 != nil &&
			strings.Contains(m.FieldsV1.GetRawString(), `"f:maxRevisions"`) {
			owned = true
		}
	}
	if !owned {
		t.Errorf("maxRevisions is not %s's: %+v", projectsettings.FieldManager, stored.ManagedFields)
	}

	req = f.request()
	req.Settings.Limits.MaxCostMicroUSD = 2_000_000
	code, res = f.save(approverAlpha, req)
	want := []projectsettings.ProjectSettingsRefusal{{Field: projectsettings.FieldMaxCost,
		Reason: projectsettings.RefusedInvalid}}
	if code != http.StatusUnprocessableEntity || res.Reason != projectsettings.ResultRefused ||
		!equalJSON(t, res.Refusals, want) {
		t.Errorf("a per-intent cost under the plan stage's = %d %+v, want 422 %+v", code, res, want)
	}
	// The rule is declared on spec.limits, and that is the path the API
	// server names: the refusal above is the mapping of exactly that.
	lines := f.logs.lines(t, "project settings refused")
	if len(lines) != 1 || lines[0]["reason"] != SettingsAPIInvalid ||
		!strings.HasPrefix(mustJSON(t, lines[0]["causes"]), `["spec.limits: `) {
		t.Errorf("refusal lines = %v, want one naming the API server's cause on spec.limits", lines)
	}
	if got := f.project(); got.Spec.Limits.MaxCostMicroUSD != 10_000_000 || got.Generation != stored.Generation {
		t.Errorf("refused save wrote: cost %d generation %d", got.Spec.Limits.MaxCostMicroUSD, got.Generation)
	}
}
