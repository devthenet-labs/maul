// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/kube"
)

// botLogin is the login fakeTracker posts as: the projection's own identity.
const botLogin = "maul[bot]"

// fakeTracker records every tracking-system write.
type fakeTracker struct {
	nextNumber    int
	issues        map[int]*ghclient.Issue
	comments      []string // posted comment bodies, in call order
	issueComments map[int][]*ghclient.Comment
	nextCommentID int64
	commentEdits  int
	lists         int // ListComments calls
	// listLag models GitHub's eventually consistent list endpoint: a comment
	// posted while it is set is missing from ListComments.
	listLag   bool
	unlisted  map[int64]bool
	assigned  []string
	closed    []int
	dismissed []int
	bodyEdits int

	// failAssign and failClose fail that many calls with a transient 502.
	failAssign, failClose int
	// onPost, when set, is called as each issue or comment is posted.
	onPost func()
	// creates counts Create calls; createErrs fail its first calls, in order.
	creates    int
	createErrs []error
	alerts     map[int]*ghclient.Alert // GetAlert's answers
	// pulls are GetPullRequest's answers; pullErrs fail its first calls,
	// in order; pullReads records each repo#number asked for.
	pulls     map[int]*ghclient.PullRequest
	pullErrs  []error
	pullReads []string

	// onIssueRead, when set, is called once, after the next GetIssue has
	// read its answer and before it returns it: what changes meanwhile.
	onIssueRead func()

	// perms are CollaboratorPermission's answers by login: a login with none
	// reads "read", as every account does on a public repository, and one
	// in missing is no account (404). permErrs fail its first calls, in
	// order; permReads records each login asked about.
	perms     map[string]string
	missing   map[string]bool
	permErrs  []error
	permReads []string
	// reactions are each comment's reactions, by comment id; reactErrs fail
	// CreateIssueCommentReaction's first calls, in order.
	reactions map[int64][]string
	reactErrs []error
	// commentErrs fail CreateComment's first calls, in order. postedErrs
	// fail the calls after those the way a timeout does: the comment is
	// posted, and the call still fails.
	commentErrs, postedErrs []error
	// recentLists records the since of each ListIssueComments call; clock
	// stamps each posted comment (testClock when nil).
	recentLists []time.Time
	clock       func() time.Time
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{
		nextNumber:    7,
		issues:        map[int]*ghclient.Issue{},
		issueComments: map[int][]*ghclient.Comment{},
		unlisted:      map[int64]bool{},
		perms:         map[string]string{},
		missing:       map[string]bool{},
		reactions:     map[int64][]string{},
	}
}

// notFound is the error GitHub answers a missing issue or comment with.
func notFound(what string) error {
	return fmt.Errorf("ghclient: %s: %w", what,
		&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}})
}

// issuesDisabled is GitHub's answer to opening an issue in a repository
// with Issues turned off.
func issuesDisabled() error {
	return fmt.Errorf("ghclient: create issue in acme/orders: %w", &github.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusGone},
		Message:  "Issues has been disabled in this repository.",
	})
}

// badGateway is a transient GitHub failure.
func badGateway(what string) error {
	return fmt.Errorf("ghclient: %s: %w", what,
		&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}})
}

func (f *fakeTracker) Create(
	_ context.Context, repo ghclient.Repo, req ghclient.IssueRequest,
) (*ghclient.Issue, error) {
	f.creates++
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		return nil, err
	}
	if f.onPost != nil {
		f.onPost()
	}
	n := f.nextNumber
	f.nextNumber++
	is := &ghclient.Issue{
		Repo: repo, Number: n, Title: req.Title, Body: req.Body, State: "open", Labels: req.Labels, Author: botLogin,
	}
	f.issues[n] = is
	return is, nil
}

func (f *fakeTracker) GetIssue(_ context.Context, _ ghclient.Repo, number int) (*ghclient.Issue, error) {
	is, ok := f.issues[number]
	if !ok {
		return nil, notFound(fmt.Sprintf("get issue #%d", number))
	}
	if hook := f.onIssueRead; hook != nil {
		f.onIssueRead = nil
		read := *is
		hook()
		return &read, nil
	}
	return is, nil
}

func (f *fakeTracker) EditBody(_ context.Context, _ ghclient.Repo, number int, body string) error {
	f.bodyEdits++
	f.issues[number].Body = body
	return nil
}

func (f *fakeTracker) AddLabels(_ context.Context, _ ghclient.Repo, number int, add []string) error {
	f.issues[number].Labels = append(f.issues[number].Labels, add...)
	return nil
}

func (f *fakeTracker) RemoveLabel(_ context.Context, _ ghclient.Repo, number int, name string) error {
	out := f.issues[number].Labels[:0]
	for _, l := range f.issues[number].Labels {
		if l != name {
			out = append(out, l)
		}
	}
	f.issues[number].Labels = out
	return nil
}

func (f *fakeTracker) Comment(ctx context.Context, repo ghclient.Repo, number int, body string) error {
	_, err := f.CreateComment(ctx, repo, number, body)
	return err
}

func (f *fakeTracker) CreateComment(_ context.Context, _ ghclient.Repo, number int, body string) (int64, error) {
	if len(f.commentErrs) > 0 {
		err := f.commentErrs[0]
		f.commentErrs = f.commentErrs[1:]
		return 0, err
	}
	if _, ok := f.issues[number]; !ok {
		return 0, notFound(fmt.Sprintf("comment on issue #%d", number))
	}
	if f.onPost != nil {
		f.onPost()
	}
	f.comments = append(f.comments, body)
	f.nextCommentID++
	at := testClock
	if f.clock != nil {
		at = f.clock()
	}
	f.issueComments[number] = append(f.issueComments[number],
		&ghclient.Comment{ID: f.nextCommentID, Body: body, UserLogin: botLogin, CreatedAt: at, UpdatedAt: at})
	if f.listLag {
		f.unlisted[f.nextCommentID] = true
	}
	if len(f.postedErrs) > 0 {
		err := f.postedErrs[0]
		f.postedErrs = f.postedErrs[1:]
		return 0, err
	}
	return f.nextCommentID, nil
}

func (f *fakeTracker) ListComments(_ context.Context, _ ghclient.Repo, number int) ([]*ghclient.Comment, error) {
	f.lists++
	var out []*ghclient.Comment
	for _, c := range f.issueComments[number] {
		if !f.unlisted[c.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

// ListIssueComments is ListComments with GitHub's since filter: only the
// comments updated at or after it.
func (f *fakeTracker) ListIssueComments(
	_ context.Context, _ ghclient.Repo, number int, since time.Time,
) ([]*ghclient.Comment, error) {
	f.recentLists = append(f.recentLists, since)
	var out []*ghclient.Comment
	for _, c := range f.issueComments[number] {
		if !f.unlisted[c.ID] && !c.UpdatedAt.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeTracker) EditComment(_ context.Context, _ ghclient.Repo, commentID int64, body string) error {
	for _, cs := range f.issueComments {
		for _, c := range cs {
			if c.ID == commentID {
				c.Body = body
				f.commentEdits++
				return nil
			}
		}
	}
	return notFound(fmt.Sprintf("edit comment %d", commentID))
}

func (f *fakeTracker) Assign(_ context.Context, _ ghclient.Repo, number int, logins []string) error {
	if f.failAssign > 0 {
		f.failAssign--
		return badGateway(fmt.Sprintf("assign #%d", number))
	}
	f.assigned = append(f.assigned, logins...)
	return nil
}

func (f *fakeTracker) Close(_ context.Context, _ ghclient.Repo, number int) error {
	if f.failClose > 0 {
		f.failClose--
		return badGateway(fmt.Sprintf("close #%d", number))
	}
	f.closed = append(f.closed, number)
	return nil
}

func (f *fakeTracker) DismissAlert(_ context.Context, _ ghclient.Repo, number int, _, _ string) error {
	f.dismissed = append(f.dismissed, number)
	return nil
}

func (f *fakeTracker) GetAlert(_ context.Context, _ ghclient.Repo, number int) (*ghclient.Alert, error) {
	a, ok := f.alerts[number]
	if !ok {
		return nil, fmt.Errorf("ghclient: get alert #%d: %w", number,
			&github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}})
	}
	return a, nil
}

func (f *fakeTracker) GetPullRequest(_ context.Context, repo ghclient.Repo, number int) (*ghclient.PullRequest, error) {
	f.pullReads = append(f.pullReads, fmt.Sprintf("%s#%d", repo, number))
	if len(f.pullErrs) > 0 {
		err := f.pullErrs[0]
		f.pullErrs = f.pullErrs[1:]
		return nil, err
	}
	pr, ok := f.pulls[number]
	if !ok {
		return nil, notFound(fmt.Sprintf("get PR %s#%d", repo, number))
	}
	return pr, nil
}

func (f *fakeTracker) CollaboratorPermission(_ context.Context, repo ghclient.Repo, login string) (string, error) {
	f.permReads = append(f.permReads, login)
	if len(f.permErrs) > 0 {
		err := f.permErrs[0]
		f.permErrs = f.permErrs[1:]
		return "", err
	}
	if f.missing[login] {
		return "", fmt.Errorf("permission of %s on %s: %w: %w", login, repo, ghclient.ErrNoSuchUser,
			notFound("collaborator permission"))
	}
	if p, ok := f.perms[login]; ok {
		return p, nil
	}
	return ghclient.PermissionRead, nil
}

func (f *fakeTracker) CreateIssueCommentReaction(
	_ context.Context, _ ghclient.Repo, commentID int64, content string,
) error {
	if len(f.reactErrs) > 0 {
		err := f.reactErrs[0]
		f.reactErrs = f.reactErrs[1:]
		return err
	}
	if !slices.Contains(f.reactions[commentID], content) {
		f.reactions[commentID] = append(f.reactions[commentID], content)
	}
	return nil
}

// projectable is a Finding ready for projection.
func projectable(phase v1alpha1.Phase) *v1alpha1.Finding {
	return &v1alpha1.Finding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "finding-aa-1", Namespace: "maul",
			Labels: map[string]string{v1alpha1.LabelKeyHash: "aa"},
		},
		Spec: v1alpha1.FindingSpec{
			IntegrationRef: v1alpha1.LocalObjectReference{Name: "gh"},
			TrackingRef:    &v1alpha1.LocalObjectReference{Name: "gh"},
			Source:         "ghas",
			Repository: &v1alpha1.FindingRepository{
				Type: v1alpha1.RepositoryTypeGitHub,
				URL:  "https://github.com/acme/orders",
				Name: "acme/orders",
			},
			Advisories: []string{"CVE-2026-0001"},
			Title:      "Reflected XSS",
			Severity:   v1alpha1.LevelHigh,
			Alerts:     []v1alpha1.Alert{{ID: "42", URL: "https://github.com/acme/orders/security/42"}},
		},
		Status: v1alpha1.FindingStatus{Phase: phase},
	}
}

func newProjector(t *testing.T, tracker *fakeTracker, objs ...client.Object) (*FindingReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(kube.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Finding{}, &v1alpha1.Investigation{}, &v1alpha1.Remediation{}).
		Build()
	r := &FindingReconciler{
		Client:    c,
		Namespace: "maul",
		Now:       func() time.Time { return testClock },
		ClientFor: func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
			return tracker, nil
		},
	}
	return r, c
}

func reconcileFinding(t *testing.T, r *FindingReconciler) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "maul", Name: "finding-aa-1"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestProjectCreatesIssue(t *testing.T) {
	tracker := newFakeTracker()
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))

	reconcileFinding(t, r)

	f := get(t, c, "finding-aa-1")
	if f.Status.Tracking == nil || f.Status.Tracking.IssueNumber != 7 {
		t.Fatalf("tracking = %+v, want issue 7 linked", f.Status.Tracking)
	}
	if !strings.Contains(f.Status.Tracking.URL, "/acme/orders/issues/7") {
		t.Errorf("tracking url = %q", f.Status.Tracking.URL)
	}
	is := tracker.issues[7]
	if !strings.Contains(is.Body, "maul:finding maul/finding-aa-1") {
		t.Errorf("body missing finding marker:\n%s", is.Body)
	}
	if !strings.Contains(is.Title, "CVE-2026-0001") {
		t.Errorf("title = %q", is.Title)
	}
	found := false
	for _, l := range is.Labels {
		if l == "security-finding: opened" {
			found = true
		}
	}
	if !found {
		t.Errorf("labels = %v, want security-finding: opened", is.Labels)
	}
}

func TestProjectRerendersOnPhaseChange(t *testing.T) {
	tracker := newFakeTracker()
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	reconcileFinding(t, r)

	// Same state: no extra writes.
	reconcileFinding(t, r)
	if tracker.bodyEdits != 0 {
		t.Fatalf("bodyEdits = %d after idempotent reconcile, want 0", tracker.bodyEdits)
	}

	f := get(t, c, "finding-aa-1")
	f.Status.Phase = v1alpha1.PhaseEnhanced
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("update phase: %v", err)
	}
	reconcileFinding(t, r)
	if tracker.bodyEdits != 1 {
		t.Errorf("bodyEdits = %d after phase change, want 1", tracker.bodyEdits)
	}
	labels := tracker.issues[7].Labels
	found := false
	for _, l := range labels {
		if l == "security-finding: enhanced" {
			found = true
		}
	}
	if !found {
		t.Errorf("labels = %v, want security-finding: enhanced", labels)
	}
}

func TestProjectReprojectsWhenIssueGone(t *testing.T) {
	tracker := newFakeTracker()
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	reconcileFinding(t, r)

	// The issue vanishes on the forge (deleted there, or an ephemeral
	// tracker was reset) and a phase change forces the projection to touch
	// it: the dead link is dropped, then a fresh issue is projected.
	delete(tracker.issues, 7)
	f := get(t, c, "finding-aa-1")
	f.Status.Phase = v1alpha1.PhaseEnhanced
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("update phase: %v", err)
	}

	reconcileFinding(t, r)
	if f = get(t, c, "finding-aa-1"); f.Status.Tracking != nil {
		t.Fatalf("tracking = %+v after 404, want unlinked", f.Status.Tracking)
	}

	reconcileFinding(t, r)
	f = get(t, c, "finding-aa-1")
	if f.Status.Tracking == nil || f.Status.Tracking.IssueNumber != 8 {
		t.Fatalf("tracking = %+v, want fresh issue 8 linked", f.Status.Tracking)
	}
	if _, ok := tracker.issues[8]; !ok {
		t.Errorf("issue 8 not created; issues = %v", tracker.issues)
	}
}

func TestProjectIssuesDisabledBacksOff(t *testing.T) {
	tracker := newFakeTracker()
	tracker.createErrs = []error{issuesDisabled(), issuesDisabled()}
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	now := testClock
	r.Now = func() time.Time { return now }
	// Count the clients built: each is an installation token minted in
	// production, which a waiting projection must not spend either.
	clients := 0
	r.ClientFor = func(context.Context, *v1alpha1.Integration, ghclient.Repo) (trackerClient, error) {
		clients++
		return tracker, nil
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "maul", Name: "finding-aa-1"}}

	// GitHub refuses the issue for good (410): the projection records why
	// and waits out the re-check, rather than failing into a hot retry.
	res, err := r.Reconcile(t.Context(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v, want the refusal recorded, not retried", err)
	}
	if res.RequeueAfter != DefaultTrackingRecheck {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, DefaultTrackingRecheck)
	}
	f := get(t, c, "finding-aa-1")
	cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionTrackingUnavailable)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonIssuesDisabled {
		t.Fatalf("TrackingUnavailable = %+v, want True/IssuesDisabled", cond)
	}
	if !strings.Contains(cond.Message, "acme/orders") {
		t.Errorf("message = %q, want the repository named", cond.Message)
	}
	if f.Status.Phase != v1alpha1.PhaseOpened {
		t.Errorf("phase = %s, want the pipeline untouched", f.Status.Phase)
	}

	// The finding moves on meanwhile: its own events do not re-ask GitHub
	// before the re-check is due.
	f.Status.Phase = v1alpha1.PhaseEnhanced
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("update phase: %v", err)
	}
	now = testClock.Add(time.Hour)
	if res, err = r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if tracker.creates != 1 || clients != 1 {
		t.Errorf("creates = %d, clients = %d before the re-check; want 1 and 1 (no token minted while waiting)",
			tracker.creates, clients)
	}
	if want := DefaultTrackingRecheck - time.Hour; res.RequeueAfter != want {
		t.Errorf("RequeueAfter = %v, want the rest of the re-check (%v)", res.RequeueAfter, want)
	}

	// Due: asked again, still refused, and the next re-check is paced from
	// this refusal.
	now = testClock.Add(DefaultTrackingRecheck)
	if res, err = r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if tracker.creates != 2 || res.RequeueAfter != DefaultTrackingRecheck {
		t.Errorf("creates = %d, RequeueAfter = %v; want 2 and %v", tracker.creates, res.RequeueAfter, DefaultTrackingRecheck)
	}

	// Issues turned back on: the next re-check opens the issue and clears
	// the condition.
	now = testClock.Add(2 * DefaultTrackingRecheck)
	reconcileFinding(t, r)
	f = get(t, c, "finding-aa-1")
	if f.Status.Tracking == nil || f.Status.Tracking.IssueNumber != 7 {
		t.Fatalf("tracking = %+v, want issue 7 linked", f.Status.Tracking)
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionTrackingUnavailable); cond != nil {
		t.Errorf("TrackingUnavailable = %+v after the issue opened, want removed", cond)
	}
}

// A refusal is for one repository: once the finding's issue belongs in
// another (here the tracking Integration's fallback moved), the next
// reconcile asks GitHub there at once rather than waiting out the re-check.
func TestProjectIssuesDisabledRechecksMovedRepository(t *testing.T) {
	tracker := newFakeTracker()
	tracker.createErrs = []error{issuesDisabled()}
	integ := withFallback()
	r, c := newProjector(t, tracker, integ, projectableCloud(v1alpha1.PhaseEnhanced))
	now := testClock
	r.Now = func() time.Time { return now }
	var asked []string
	r.ClientFor = func(_ context.Context, _ *v1alpha1.Integration, repo ghclient.Repo) (trackerClient, error) {
		asked = append(asked, repo.String())
		return tracker, nil
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "maul", Name: "finding-aa-1"}}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !meta.IsStatusConditionTrue(get(t, c, "finding-aa-1").Status.Conditions,
		v1alpha1.ConditionTrackingUnavailable) {
		t.Fatal("TrackingUnavailable not recorded for the refusal")
	}

	// Same repository an hour on: still waiting, nothing asked.
	now = testClock.Add(time.Hour)
	if res, err := r.Reconcile(t.Context(), req); err != nil || res.RequeueAfter == 0 {
		t.Fatalf("Reconcile = %+v, %v; want a wait for the unchanged repository", res, err)
	}

	// The operator points the fallback at a repository with Issues on.
	var cur v1alpha1.Integration
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(integ), &cur); err != nil {
		t.Fatalf("get integration: %v", err)
	}
	cur.Spec.GitHub.Issues.FallbackRepository = "acme/triage"
	if err := c.Update(t.Context(), &cur); err != nil {
		t.Fatalf("update integration: %v", err)
	}
	reconcileFinding(t, r)

	if want := []string{"acme/security", "acme/triage"}; !slices.Equal(asked, want) {
		t.Errorf("repositories asked = %v, want %v", asked, want)
	}
	f := get(t, c, "finding-aa-1")
	if f.Status.Tracking == nil {
		t.Fatal("tracking not linked in the new repository")
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionTrackingUnavailable); cond != nil {
		t.Errorf("TrackingUnavailable = %+v after the issue opened, want removed", cond)
	}
}

func TestProjectTransientCreateFailureStillRetries(t *testing.T) {
	tracker := newFakeTracker()
	tracker.createErrs = []error{badGateway("create issue")}
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "maul", Name: "finding-aa-1"}}

	if _, err := r.Reconcile(t.Context(), req); err == nil {
		t.Fatal("Reconcile: nil error on a 502, want it returned for the backoff")
	}
	f := get(t, c, "finding-aa-1")
	if cond := meta.FindStatusCondition(f.Status.Conditions, v1alpha1.ConditionTrackingUnavailable); cond != nil {
		t.Errorf("TrackingUnavailable = %+v on a transient failure, want none", cond)
	}
}

func TestProjectEnrichmentCommentOnce(t *testing.T) {
	tracker := newFakeTracker()
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	reconcileFinding(t, r)

	f := get(t, c, "finding-aa-1")
	f.Status.Enrichments = []v1alpha1.Enrichment{{
		Enhancer: "static-context", Markdown: "owned by team-payments", AppliedAt: metav1.NewTime(testClock),
	}}
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("add enrichment: %v", err)
	}
	reconcileFinding(t, r)
	reconcileFinding(t, r)

	n := 0
	for _, cm := range tracker.comments {
		if strings.Contains(cm, "maul:enrichment static-context") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("enrichment comments = %d, want exactly 1", n)
	}
}

func TestProjectEnrichmentStickyEdit(t *testing.T) {
	tracker := newFakeTracker()
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	reconcileFinding(t, r)

	f := get(t, c, "finding-aa-1")
	f.Status.Enrichments = []v1alpha1.Enrichment{{
		Enhancer: "static-context", Markdown: "owned by team-payments", AppliedAt: metav1.NewTime(testClock),
	}}
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("add enrichment: %v", err)
	}
	reconcileFinding(t, r)

	// The markdown moves: the existing comment is edited in place, not
	// re-posted.
	f = get(t, c, "finding-aa-1")
	f.Status.Enrichments[0].Markdown = "owned by team-checkout"
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("update enrichment: %v", err)
	}
	reconcileFinding(t, r)

	n := 0
	for _, cm := range tracker.comments {
		if strings.Contains(cm, "maul:enrichment static-context") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("enrichment comments posted = %d, want exactly 1", n)
	}
	if tracker.commentEdits != 1 {
		t.Errorf("comment edits = %d, want 1", tracker.commentEdits)
	}
	sticky := tracker.issueComments[7][len(tracker.issueComments[7])-1]
	if !strings.Contains(sticky.Body, "team-checkout") {
		t.Errorf("sticky body not updated:\n%s", sticky.Body)
	}
}

func TestProjectEnrichmentAttributesAsLabels(t *testing.T) {
	tracker := newFakeTracker()
	r, c := newProjector(t, tracker, testIntegration(), projectable(v1alpha1.PhaseOpened))
	reconcileFinding(t, r)

	f := get(t, c, "finding-aa-1")
	f.Status.Enrichments = []v1alpha1.Enrichment{{
		Enhancer:   "static-context",
		Attributes: map[string]string{"environment": "prod", "system": "storefront"},
		AppliedAt:  metav1.NewTime(testClock),
	}}
	if err := c.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("add enrichment: %v", err)
	}
	reconcileFinding(t, r)

	got := tracker.issues[7].Labels
	for _, want := range []string{"security-context: environment=prod", "security-context: system=storefront"} {
		if !slices.Contains(got, want) {
			t.Errorf("labels = %v, want %q", got, want)
		}
	}
	// Attributes alone post no comment.
	for _, cm := range tracker.comments {
		if strings.Contains(cm, "maul:enrichment") {
			t.Errorf("unexpected enrichment comment:\n%s", cm)
		}
	}
}

func TestProjectDismissed(t *testing.T) {
	tracker := newFakeTracker()
	fnd := projectable(v1alpha1.PhaseDismissed)
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7, URL: "u", State: "open"}
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
	r, c := newProjector(t, tracker, testIntegration(), fnd)

	reconcileFinding(t, r)

	if len(tracker.dismissed) != 1 || tracker.dismissed[0] != 42 {
		t.Errorf("dismissed = %v, want [42]", tracker.dismissed)
	}
	if len(tracker.closed) != 1 {
		t.Errorf("closed = %v, want the tracking issue", tracker.closed)
	}
	if got := get(t, c, "finding-aa-1").Status.Tracking.State; got != "closed" {
		t.Errorf("tracking state = %q, want closed", got)
	}
}

func TestProjectAwaitingApprovalNotifies(t *testing.T) {
	tracker := newFakeTracker()
	fnd := projectable(v1alpha1.PhaseAwaitingApproval)
	fnd.Status.Tracking = &v1alpha1.TrackingStatus{Integration: "gh", IssueNumber: 7, URL: "u", State: "open"}
	fnd.Status.Owners = []string{"alice", "bob"}
	tracker.issues[7] = &ghclient.Issue{Number: 7, State: "open"}
	r, _ := newProjector(t, tracker, testIntegration(), fnd)

	reconcileFinding(t, r)
	reconcileFinding(t, r) // idempotent

	notices := 0
	for _, cm := range tracker.comments {
		if strings.Contains(cm, "`/maul approve`") {
			notices++
		}
	}
	if notices != 1 {
		t.Errorf("approval notices = %d, want exactly 1", notices)
	}
	if len(tracker.assigned) != 2 {
		t.Errorf("assigned = %v, want alice+bob", tracker.assigned)
	}
}
