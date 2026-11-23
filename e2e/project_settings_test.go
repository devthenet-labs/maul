// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/e2e/fakeoidc"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/rbactest"
)

// carol is an account with write access to the intent repository that is on
// no approver list until an approver adds her from the settings page.
var carol = fakegithub.Actor{Login: "carol", ID: 1003, Type: "User"}

// settingsBuildTurns is the build stage's turn limit the approver sets from
// the page, below intent-controller's own ceiling.
const settingsBuildTurns int32 = 37

// TestProjectSettingsEndToEnd drives the status page's Project settings
// through the shipped binaries: status-server with the page on (approver
// list edits allowed) and intent-controller on the envtest API server, the
// fake GitHub answering who may write, and people signed in through the
// fakeoidc authorization code flow, each with their GitHub id.
//
// It walks the design's story: an approver with write access lowers the
// build stage's turns, and the next build run is granted exactly that; a
// signed-in account that is no approver, and an approver GitHub gives no
// write access, are refused with nothing written; a model the controller
// does not admit is refused and the Project stays Ready; a page served
// before another save is refused as stale; a login the approver adds may not
// save until intent-controller has resolved it against GitHub, and may once
// it has; a login removed is refused at once; and raising maxRevisions from
// the page lifts an intent blocked on RevisionLimitReached, whose waiting
// /maul revise then runs.
func TestProjectSettingsEndToEnd(t *testing.T) {
	s := startSettingsPage(t)
	before := s.lowerBuildTurns(t)
	s.refusesOthers(t)
	s.refusesStaleAndUnadmitted(t, before)
	s.addsAnApprover(t)
	s.removesAnApprover(t)
	name, pr, pushed := s.buildsOnTheGrant(t)
	s.raisesTheRevisionLimit(t, name, pr, pushed)
}

// settingsPage is the stack TestProjectSettingsEndToEnd drives: the intent
// stack, status-server with the settings page on, and a signed-in browser
// per person.
type settingsPage struct {
	e *intentEnv
	// page is the settings API of the Project.
	page string
	// octo is the approver's browser; rdr, mal and car reader's, mallory's
	// and carol's.
	octo, rdr, mal, car *http.Client
}

// startSettingsPage starts the intent stack with a Project of two approvers
// (one who may write) and no revision rounds, status-server with the page on
// and approver list edits allowed, and signs everyone in.
func startSettingsPage(t *testing.T) *settingsPage {
	t.Helper()
	cl, gh, _ := startSourceStack(t, appRepo)
	gh.SetRole(carol.Login, "write")
	gh.SetUserID(carol.Login, carol.ID)
	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories:     []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL}},
		// No revision round at all, until the page raises it.
		Limits: v1alpha1.ProjectLimits{MaxRevisions: new(int32(0))},
	})
	eventually(t, "the project to publish its approvers and admits", func() bool {
		p := e.project(t)
		return p.Status.Approvers != nil && p.Status.Approvers.ResolvedAt != nil && p.Status.Admits != nil
	})
	if ceiling := e.project(t).Status.Admits.Stages.Build.MaxTurns; ceiling <= settingsBuildTurns {
		t.Fatalf("intent-controller's build turn ceiling is %d; the test lowers to %d, under it", ceiling,
			settingsBuildTurns)
	}

	oidc, err := fakeoidc.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oidc.Close)
	people := []fakegithub.Actor{approver, reader, mallory, carol}
	for _, a := range people {
		oidc.AddUser(fakeoidc.User{Login: a.Login, Name: a.Login, GitHubID: a.ID})
	}
	grantProjectViewers(t, cl, people...)
	pol := projectsettings.DefaultPolicy()
	pol.Allow.Approvers = true
	policy, err := json.Marshal(pol)
	if err != nil {
		t.Fatal(err)
	}
	listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	base := "http://" + listen
	cl.controller(t, "status-server", "--listen-addr", listen,
		"--auth-config", writeAccountAuthConfig(t, oidc.URL, base+"/oauth2/callback"),
		"--agent-namespace", agentsNS, "--intents-enabled",
		"--project-settings-enabled", "--project-settings-policy", string(policy))
	return &settingsPage{e: e, page: base + "/api/projects/" + projectName + "/settings",
		octo: signIn(t, base, oidc, approver.Login), rdr: signIn(t, base, oidc, reader.Login),
		mal: signIn(t, base, oidc, mallory.Login), car: signIn(t, base, oidc, carol.Login)}
}

// lowerBuildTurns: the approver, who may write, lowers the build stage's
// turns. It returns the page as served before the save.
func (s *settingsPage) lowerBuildTurns(t *testing.T) projectsettings.ProjectSettingsView {
	t.Helper()
	before := settingsOf(t, s.octo, s.page)
	if !before.CanEdit || before.Reason != "" {
		t.Fatalf("the approver's page: canEdit %v (%q), want an approver who may save", before.CanEdit, before.Reason)
	}
	code, res := saveSettings(t, s.octo, s.page, before, nil, func(d *projectsettings.ProjectSettings) {
		d.Stages.Build.MaxTurns = settingsBuildTurns
	})
	if code != http.StatusOK || res.Reason != "" || len(res.Changes) != 1 ||
		res.Changes[0].Field != "limits.build.maxTurns" {
		t.Fatalf("lower the build turns = %d %+v, want 200 with that one change", code, res)
	}
	if got := s.e.project(t).Spec.Limits.Build.MaxTurns; got != settingsBuildTurns {
		t.Fatalf("spec.limits.build.maxTurns = %d after the save, want %d", got, settingsBuildTurns)
	}
	eventually(t, "intent-controller to observe the saved generation", func() bool {
		return settingsOf(t, s.octo, s.page).ObservedGeneration >= res.Generation
	})
	return before
}

// refusesOthers: signed in and seeing the Project, but no approver; an
// approver GitHub gives no write access: both 403, nothing written.
func (s *settingsPage) refusesOthers(t *testing.T) {
	t.Helper()
	saved := s.e.project(t).Generation
	for _, who := range []struct {
		c      *http.Client
		login  string
		reason string
	}{
		{s.mal, mallory.Login, projectsettings.ReasonNotApprover},
		{s.rdr, reader.Login, projectsettings.ReasonNoWriteAccess},
	} {
		v := settingsOf(t, who.c, s.page)
		if v.CanEdit || v.Reason != who.reason {
			t.Errorf("%s's page: canEdit %v (%q), want %q", who.login, v.CanEdit, v.Reason, who.reason)
		}
		code, res := saveSettings(t, who.c, s.page, v, nil, func(d *projectsettings.ProjectSettings) {
			d.Stages.Build.MaxTurns = settingsBuildTurns - 1
		})
		if code != http.StatusForbidden || res.Reason != who.reason || len(res.Changes) != 0 {
			t.Errorf("%s's save = %d %+v, want 403 %s telling nothing of the save", who.login, code, res, who.reason)
		}
	}
	if p := s.e.project(t); p.Generation != saved || p.Spec.Limits.Build.MaxTurns != settingsBuildTurns {
		t.Errorf("a refused save wrote: generation %d (was %d), build turns %d", p.Generation, saved,
			p.Spec.Limits.Build.MaxTurns)
	}
}

// refusesStaleAndUnadmitted: a page served before the approver's own save
// is stale (409); a model intent-controller does not admit is 422 naming
// it, nothing is written, and the Project stays Ready.
func (s *settingsPage) refusesStaleAndUnadmitted(t *testing.T, before projectsettings.ProjectSettingsView) {
	t.Helper()
	saved := s.e.project(t).Generation
	code, res := saveSettings(t, s.octo, s.page, before, nil, func(d *projectsettings.ProjectSettings) {
		d.Stages.Plan.MaxTurns = 20
	})
	if code != http.StatusConflict || res.Reason != projectsettings.ResultGenerationChanged {
		t.Errorf("a save from the stale page = %d %+v, want 409 %s", code, res, projectsettings.ResultGenerationChanged)
	}
	code, res = saveSettings(t, s.octo, s.page, settingsOf(t, s.octo, s.page), nil,
		func(d *projectsettings.ProjectSettings) { d.Models.Build = "claude-e2e-not-admitted" })
	if code != http.StatusUnprocessableEntity || res.Reason != projectsettings.ResultRefused ||
		!slices.ContainsFunc(res.Refusals, func(r projectsettings.ProjectSettingsRefusal) bool {
			return r.Field == "models.build" && r.Reason == projectsettings.RefusedModelNotAdmitted
		}) {
		t.Errorf("an unadmitted model = %d %+v, want 422 refusing models.build as not admitted", code, res)
	}
	// This stack runs intent-controller without --intent-build-subagents,
	// so the Project's admits allow no delegation: the switch is locked
	// while off, and a save turning it on anyway is refused.
	view := settingsOf(t, s.octo, s.page)
	if i := slices.IndexFunc(view.Fields, func(f projectsettings.ProjectSettingsField) bool {
		return f.ID == projectsettings.FieldSubagents
	}); i < 0 || view.Fields[i].Locked != projectsettings.LockedNotAdmitted || view.Options.Subagents {
		t.Errorf("the subagents switch = %+v, options %+v; want it locked as not admitted", view.Fields, view.Options)
	}
	code, res = saveSettings(t, s.octo, s.page, view, nil,
		func(d *projectsettings.ProjectSettings) { d.Delegation.Subagents = true })
	if code != http.StatusUnprocessableEntity || res.Reason != projectsettings.ResultRefused ||
		!slices.ContainsFunc(res.Refusals, func(r projectsettings.ProjectSettingsRefusal) bool {
			return r.Field == projectsettings.FieldSubagents && r.Reason == projectsettings.RefusedDelegationNotAdmitted
		}) {
		t.Errorf("subagents turned on = %d %+v, want 422 refusing delegation.subagents as not admitted", code, res)
	}
	consistently(t, "the project to stay Ready and unwritten after the refused saves", func() bool {
		p := s.e.project(t)
		return p.Generation == saved && p.Spec.Limits.Plan.MaxTurns == 0 &&
			(p.Spec.Models == nil || p.Spec.Models.Build == "") && p.Spec.Delegation == nil &&
			meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady)
	})
}

// setActiveIntents is an edit of limits.maxActiveIntents.
func setActiveIntents(n int32) func(*projectsettings.ProjectSettings) {
	return func(d *projectsettings.ProjectSettings) { d.Limits.MaxActiveIntents = n }
}

// addsAnApprover: the approver adds carol, with intent-controller down so
// nothing resolves her yet: a weakening change, confirmed, then saved.
// Carol, signed in with write access on GitHub, may not save until the
// controller has asked GitHub who she is, and may once it has.
func (s *settingsPage) addsAnApprover(t *testing.T) {
	t.Helper()
	s.e.stopIntents()
	addCarol := func(d *projectsettings.ProjectSettings) { d.Approvers = append(d.Approvers, carol.Login) }
	s.confirmedSave(t, "add carol", projectsettings.ConfirmApproverAdd, addCarol)
	if logins := s.e.project(t).Spec.Approvers.Logins; !slices.Equal(logins,
		[]string{approver.Login, reader.Login, carol.Login}) {
		t.Fatalf("approvers = %v after adding carol", logins)
	}
	cv := settingsOf(t, s.car, s.page)
	if cv.CanEdit || cv.Reason != projectsettings.ReasonApproversUnresolved {
		t.Errorf("carol's page before resolution: canEdit %v (%q), want %q", cv.CanEdit, cv.Reason,
			projectsettings.ReasonApproversUnresolved)
	}
	if code, res := saveSettings(t, s.car, s.page, cv, nil, setActiveIntents(3)); code != http.StatusForbidden ||
		res.Reason != projectsettings.ReasonApproversUnresolved {
		t.Errorf("carol's save before resolution = %d %+v, want 403 %s", code, res,
			projectsettings.ReasonApproversUnresolved)
	}

	s.e.restartIntents(t)
	within(t, 90*time.Second, "intent-controller to resolve carol as a writer", func() bool {
		st := s.e.project(t).Status.Approvers
		return st != nil && slices.Contains(st.Identities,
			v1alpha1.ProjectApproverIdentity{Login: carol.Login, ID: carol.ID, Write: true})
	})
	cv = settingsOf(t, s.car, s.page)
	if !cv.CanEdit {
		t.Fatalf("carol's page once resolved: canEdit false (%q)", cv.Reason)
	}
	if code, res := saveSettings(t, s.car, s.page, cv, nil, setActiveIntents(3)); code != http.StatusOK ||
		len(res.Changes) != 1 {
		t.Fatalf("carol's save once resolved = %d %+v, want 200 with one change", code, res)
	}
	if got := s.e.project(t).Spec.Limits.MaxActiveIntents; got != 3 {
		t.Errorf("maxActiveIntents = %d after carol's save, want 3", got)
	}
}

// removesAnApprover: the approver removes carol, and her next save is
// refused at once: from the live list, while intent-controller (down again,
// so nothing races the save) still has her resolved as a writer.
func (s *settingsPage) removesAnApprover(t *testing.T) {
	t.Helper()
	s.e.stopIntents()
	s.confirmedSave(t, "remove carol", projectsettings.ConfirmApproverRemove, func(d *projectsettings.ProjectSettings) {
		d.Approvers = slices.DeleteFunc(d.Approvers, func(l string) bool { return l == carol.Login })
	})
	if code, res := saveSettings(t, s.car, s.page, settingsOf(t, s.car, s.page), nil, setActiveIntents(4)); code !=
		http.StatusForbidden || res.Reason != projectsettings.ReasonNotApprover {
		t.Errorf("carol's save once removed = %d %+v, want 403 %s", code, res, projectsettings.ReasonNotApprover)
	}
	if got := s.e.project(t).Spec.Limits.MaxActiveIntents; got != 3 {
		t.Errorf("maxActiveIntents = %d after carol's refused save, want 3", got)
	}
	if st := s.e.project(t).Status.Approvers; st == nil || !slices.Contains(st.Identities,
		v1alpha1.ProjectApproverIdentity{Login: carol.Login, ID: carol.ID, Write: true}) {
		t.Errorf("status.approvers = %+v, want carol still resolved: the refusal must come from the live list", st)
	}
	s.e.restartIntents(t)
}

// confirmedSave saves the approver's edit as the page does a weakening one:
// refused with one confirmation of kind, then sent again confirming it.
func (s *settingsPage) confirmedSave(t *testing.T, what, kind string, edit func(*projectsettings.ProjectSettings)) {
	t.Helper()
	v := settingsOf(t, s.octo, s.page)
	code, res := saveSettings(t, s.octo, s.page, v, nil, edit)
	if code != http.StatusConflict || res.Reason != projectsettings.ResultConfirmRequired || len(res.Confirm) != 1 ||
		res.Confirm[0].Kind != kind {
		t.Fatalf("%s unconfirmed = %d %+v, want 409 asking to confirm it (%s)", what, code, res, kind)
	}
	if code, res = saveSettings(t, s.octo, s.page, v, []string{res.Confirm[0].Key}, edit); code != http.StatusOK {
		t.Fatalf("%s confirmed = %d %+v, want 200", what, code, res)
	}
}

// buildsOnTheGrant files and approves an intent: its build run is granted
// the page's turn limit, and its Job carries it. It returns the Intent's
// name, its pull request's number and the pushed commit.
func (s *settingsPage) buildsOnTheGrant(t *testing.T) (string, int, string) {
	t.Helper()
	e := s.e
	number, name := e.fileIntent(t)
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	buildRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageBuild, 1, appRepo, 1)
	buildJob := e.kubelet.waitRun(t, "the build job to run", phaseIs("build"))
	if g := e.run(t, buildRun).Spec.Grant; g.MaxTurns != settingsBuildTurns {
		t.Errorf("build run grant = %+v, want maxTurns %d from the page", g, settingsBuildTurns)
	}
	if got := buildJob.Env["MAUL_GRANTED_MAX_TURNS"]; got != strconv.Itoa(int(settingsBuildTurns)) {
		t.Errorf("build Job MAUL_GRANTED_MAX_TURNS = %q, want %d", got, settingsBuildTurns)
	}
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	return name, int(in.Status.PullRequests[0].Number), e.run(t, buildRun).Status.PushedCommit
}

// raisesTheRevisionLimit: with no revision round permitted, the approver's
// /maul revise blocks the intent on RevisionLimitReached; the approver
// raises maxRevisions from the page, the block lifts, and the waiting
// command's round runs.
func (s *settingsPage) raisesTheRevisionLimit(t *testing.T, name string, pr int, pushed string) {
	t.Helper()
	e := s.e
	e.gh.SetComparePatch(fakegithub.BaseSHA, pushed, "diff --git a/VERSION b/VERSION\n+0.1.0\n")
	e.gh.CommentAs(pr, "/maul revise Please test the VERSION contents.", approver)
	eventually(t, "the intent to block on its revision limit", func() bool {
		cur := e.intent(t, name)
		return cur.Status.Phase == v1alpha1.IntentBlocked &&
			meta.IsStatusConditionTrue(cur.Status.Conditions, v1alpha1.ConditionRevisionLimitReached)
	})
	reviseRun := v1alpha1.IntentRunName(name, v1alpha1.IntentStageRevise, 1, appRepo, 1)
	if _, ok := e.runs(t, name)[reviseRun]; ok {
		t.Fatalf("a revise run was created past the limit: %s", e.describeRuns(t, name))
	}

	code, res := saveSettings(t, s.octo, s.page, settingsOf(t, s.octo, s.page), nil,
		func(d *projectsettings.ProjectSettings) { d.Limits.MaxRevisions = new(int32(1)) })
	if code != http.StatusOK || len(res.Changes) != 1 || res.Changes[0].Field != "limits.maxRevisions" {
		t.Fatalf("raise maxRevisions = %d %+v, want 200 with that one change", code, res)
	}
	eventually(t, "the revision limit block to lift", func() bool {
		cur := e.intent(t, name)
		c := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionRevisionLimitReached)
		return cur.Status.Phase != v1alpha1.IntentBlocked && c != nil && c.Status == metav1.ConditionFalse
	})
	runJobName := jobs.NameFor(reviseRun, intentRunKind, 1)
	e.kubelet.waitRun(t, "the waiting revision's Job to run", func(r agentRun) bool {
		return r.Job.Name == runJobName && phaseIs("build")(r)
	})
	within(t, 60*time.Second, "the revision round to complete", func() bool {
		cur := e.intent(t, name)
		return cur.Status.Phase == v1alpha1.IntentInReview && cur.Status.Revisions == 1
	})
	if run := e.run(t, reviseRun); run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" {
		t.Errorf("revise run = %+v, want Complete and pushed", run.Status)
	}
}

// grantProjectViewers lets each person see the Project's intents views,
// in the documented grammar, which the settings page is served beside: a
// person who cannot see the Project gets no page at all (404), not a
// refusal.
func grantProjectViewers(t *testing.T, cl *cluster, people ...fakegithub.Actor) {
	t.Helper()
	ctx := context.Background()
	const role = "shop-viewers"
	if err := cl.client.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{"maul.run"}, Resources: []string{"projects/intents"},
			ResourceNames: []string{projectName}, Verbs: []string{"get"}}},
	}); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role},
	}
	reviews := make([]authorizationv1.SubjectAccessReviewSpec, 0, len(people))
	for _, a := range people {
		user := "github:" + a.Login
		binding.Subjects = append(binding.Subjects,
			rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user})
		reviews = append(reviews, rbactest.Review(user, nil, authorizationv1.ResourceAttributes{Namespace: namespace,
			Verb: "get", Group: "maul.run", Resource: "projects", Subresource: "intents", Name: projectName}))
	}
	if err := cl.client.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	rbactest.WaitAllowed(t, cl.client, reviews...)
}

// settingsOf reads the settings page's view as the signed-in client c.
func settingsOf(t *testing.T, c *http.Client, page string) projectsettings.ProjectSettingsView {
	t.Helper()
	var v projectsettings.ProjectSettingsView
	getJSON(t, c, page, &v)
	return v
}

// saveSettings posts what the page posts: the whole document the view was
// served, changed by edit, with the view's generation and schema and the
// confirmations given. It returns the status and the decoded answer.
func saveSettings(t *testing.T, c *http.Client, page string, v projectsettings.ProjectSettingsView,
	confirm []string, edit func(*projectsettings.ProjectSettings)) (int, projectsettings.ProjectSettingsResult) {
	t.Helper()
	settings := v.Settings
	settings.Approvers = slices.Clone(settings.Approvers)
	edit(&settings)
	body, err := json.Marshal(projectsettings.ProjectSettingsRequest{Generation: v.Generation, Schema: v.Schema,
		Settings: settings, Confirm: confirm})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := accountPost(t, c, page, string(body))
	var res projectsettings.ProjectSettingsResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("save %s = %d, not a result: %s", page, code, raw)
	}
	return code, res
}
