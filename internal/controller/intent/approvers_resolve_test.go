// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// approversProject is the test Project with three approvers: one with
// write access, one GitHub knows without it, and one GitHub knows nothing
// of.
func approversProject() *v1alpha1.Project {
	p := testProject()
	p.Spec.Approvers.Logins = []string{approver, "Reader", "ghost"}
	return p
}

// TestApproversResolved: each login of the list is recorded as the spec
// spells it, with the numeric id GitHub resolved it to and whether that
// account may write to the intent repository; a login GitHub knows no
// account for is id 0. The time is the pass's and ApproversResolved is
// True.
func TestApproversResolved(t *testing.T) {
	e := newEnv(t, approversProject())
	e.gh.perms["ghost"] = "404"
	e.gh.ids[approver] = 225740
	e.reconcileProject()

	p := e.getProject()
	if p.Status.Approvers == nil {
		t.Fatal("status.approvers is not published")
	}
	want := []v1alpha1.ProjectApproverIdentity{
		{Login: approver, ID: 225740, Write: true},
		{Login: "Reader", ID: actorOf("reader").ID},
		{Login: "ghost"},
	}
	if !slices.Equal(p.Status.Approvers.Identities, want) {
		t.Errorf("identities = %+v, want %+v", p.Status.Approvers.Identities, want)
	}
	if at := p.Status.Approvers.ResolvedAt; at == nil || !at.Time.Equal(e.clock.Now().Truncate(time.Second)) {
		t.Errorf("resolvedAt = %v, want %v", at, e.clock.Now())
	}
	c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionApproversResolved)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonApproversResolved {
		t.Errorf("ApproversResolved = %+v, want True %s", c, ReasonApproversResolved)
	}
	if e.gh.calls["Collaborator"] != 3 {
		t.Errorf("%d Collaborator calls, want one per login", e.gh.calls["Collaborator"])
	}
}

// TestApproversResolutionIsPaced: once resolved, a pass makes no GitHub
// call for the approvers until ten minutes have passed or the login set
// changes; then it resolves at once.
func TestApproversResolutionIsPaced(t *testing.T) {
	e := newEnv(t, approversProject())
	e.reconcileProject()
	calls := func() int { return e.gh.calls["Collaborator"] }
	if calls() != 3 {
		t.Fatalf("%d calls on the first pass, want 3", calls())
	}
	for _, step := range []time.Duration{0, time.Minute, 8 * time.Minute} {
		e.clock.Advance(step)
		e.reconcileProject()
		if calls() != 3 {
			t.Fatalf("%d calls after %v more, want no new call before ten minutes", calls(), step)
		}
	}
	e.clock.Advance(time.Minute)
	e.reconcileProject()
	if calls() != 6 {
		t.Fatalf("%d calls at ten minutes, want the list resolved again", calls())
	}

	p := e.getProject()
	p.Spec.Approvers.Logins = []string{approver, "Reader", "ghost", "carol"}
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.reconcileProject()
	if calls() != 10 {
		t.Fatalf("%d calls after a login was added, want the whole list resolved at once", calls())
	}
	got := e.getProject().Status.Approvers.Identities
	if len(got) != 4 || got[3].Login != "carol" {
		t.Errorf("identities = %+v, want carol resolved", got)
	}
}

// TestApproversResolvedAtOnceWhenALoginIsSwapped: a login replaced by
// another, so the list keeps its length, is a changed login set: the list
// is resolved again on the next pass, well before ten minutes, and the old
// login's identity (here one with write access) is gone at once. Without
// this an identity resolved for a removed login would keep matching for up
// to ten minutes under the new login's name.
func TestApproversResolvedAtOnceWhenALoginIsSwapped(t *testing.T) {
	e := newEnv(t, approversProject())
	e.reconcileProject()
	calls := func() int { return e.gh.calls["Collaborator"] }
	if calls() != 3 {
		t.Fatalf("%d calls on the first pass, want 3", calls())
	}

	e.clock.Advance(time.Minute)
	p := e.getProject()
	p.Spec.Approvers.Logins = []string{"carol", "Reader", "ghost"}
	p.Generation++
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.gh.perms["carol"] = "read"
	e.reconcileProject()
	if calls() != 6 {
		t.Fatalf("%d calls after %s was swapped for carol, want the whole list resolved at once", calls(), approver)
	}
	got := e.getProject().Status.Approvers
	want := []v1alpha1.ProjectApproverIdentity{
		{Login: "carol", ID: actorOf("carol").ID},
		{Login: "Reader", ID: actorOf("reader").ID},
		{Login: "ghost", ID: actorOf("ghost").ID},
	}
	if !slices.Equal(got.Identities, want) {
		t.Errorf("identities = %+v, want %+v", got.Identities, want)
	}
	if at := got.ResolvedAt; at == nil || !at.Time.Equal(e.clock.Now().Truncate(time.Second)) {
		t.Errorf("resolvedAt = %v, want this pass's %v", at, e.clock.Now())
	}
}

// TestApproversResolutionFailureKeepsTheOld: when GitHub cannot be read the
// identities and resolvedAt stay exactly as they were, so they age, and
// ApproversResolved is False; Ready is untouched. A failed resolution is
// retried once per poll interval, never on every pass.
func TestApproversResolutionFailureKeepsTheOld(t *testing.T) {
	e := newEnv(t, approversProject())
	e.reconcileProject()
	before := e.getProject().Status.Approvers.DeepCopy()

	e.clock.Advance(revalidateEvery)
	e.gh.failNext("Collaborator", nil, errTransient)
	e.reconcileProject()
	p := e.getProject()
	if !equalApprovers(p.Status.Approvers, before) {
		t.Errorf("status.approvers = %+v after a failure, want it kept as %+v", p.Status.Approvers, before)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionApproversResolved)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonApproversUnreadable {
		t.Errorf("ApproversResolved = %+v, want False %s", c, ReasonApproversUnreadable)
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady) {
		t.Errorf("Ready = %+v: approvers are never part of it", meta.FindStatusCondition(p.Status.Conditions,
			v1alpha1.ConditionReady))
	}

	calls := e.gh.calls["Collaborator"]
	e.reconcileProject()
	if e.gh.calls["Collaborator"] != calls {
		t.Errorf("a failed resolution was retried on the next pass")
	}
	e.clock.Advance(testSettings().PollInterval)
	e.reconcileProject()
	p = e.getProject()
	if e.gh.calls["Collaborator"] != calls+3 {
		t.Errorf("%d calls a poll interval after the failure, want a retry", e.gh.calls["Collaborator"]-calls)
	}
	if at := p.Status.Approvers.ResolvedAt; at == nil || !at.Time.Equal(e.clock.Now().Truncate(time.Second)) {
		t.Errorf("resolvedAt = %v after the retry, want now", at)
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionApproversResolved) {
		t.Errorf("ApproversResolved still False after a good retry")
	}
}

// TestApproversResolvedWhileNotReady: a Project that is not Ready (here:
// two repositories without --intent-multi-repo) still has its approvers
// resolved, so the settings page can fix what keeps it from Ready.
func TestApproversResolvedWhileNotReady(t *testing.T) {
	p := approversProject()
	p.Spec.Repositories = append(p.Spec.Repositories, v1alpha1.ProjectRepository{Name: "two",
		URL: "https://github.com/acme/two"})
	e := newEnv(t, p)
	e.reconcileProject()
	got := e.getProject()
	if meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
		t.Fatal("the Project is Ready; the test needs one that is not")
	}
	if got.Status.Approvers == nil || len(got.Status.Approvers.Identities) != 3 {
		t.Errorf("status.approvers = %+v, want resolved while not Ready", got.Status.Approvers)
	}
}

// TestResolveApproversHoldsTheBounds: what is published never breaks the
// status schema (a refused status write would lose Ready too): an empty or
// over-long login gets no identity and no call, and an answer naming
// another account than the login asked for is no account.
func TestResolveApproversHoldsTheBounds(t *testing.T) {
	long := strings.Repeat("a", 65)
	gh := &renamingGitHub{fakeGitHub: newFakeGitHub(&fakeClock{})}
	got, err := resolveApprovers(context.Background(), gh, intentRepoURL, []string{"", long, approver, "renamed"})
	if err != nil {
		t.Fatal(err)
	}
	want := []v1alpha1.ProjectApproverIdentity{
		{Login: approver, ID: actorOf(approver).ID, Write: true},
		{Login: "renamed"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("identities = %+v, want %+v", got, want)
	}
	if gh.calls["Collaborator"] != 2 {
		t.Errorf("%d calls, want one per valid login", gh.calls["Collaborator"])
	}
}

// renamingGitHub answers "renamed" with another account's login, as a
// lookup that resolved to someone else would.
type renamingGitHub struct{ *fakeGitHub }

func (g *renamingGitHub) Collaborator(ctx context.Context, repoURL, login string) (ghclient.Collaborator, error) {
	c, err := g.fakeGitHub.Collaborator(ctx, repoURL, login)
	if login == "renamed" {
		c.Login, c.Permission = "someone-else", ghclient.PermissionAdmin
	}
	return c, err
}

func equalApprovers(a, b *v1alpha1.ProjectApproversStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return slices.Equal(a.Identities, b.Identities) && a.ResolvedAt.Equal(b.ResolvedAt)
}

// TestApproversUnreadableSurvivesAFailedPass: in an outage the pass whose
// resolution fails may fail discovery too and write no status at all; the
// next pass, paced not to ask GitHub for the approvers again, still says
// they are unresolved.
func TestApproversUnreadableSurvivesAFailedPass(t *testing.T) {
	e := newEnv(t, approversProject())
	e.reconcileProject()
	e.clock.Advance(revalidateEvery)
	e.gh.failNext("Collaborator", errTransient)
	e.gh.failNext("ListIssues", errTransient)
	if _, err := e.project.Reconcile(context.Background(), req("target")); err == nil {
		t.Fatal("the pass succeeded; the test needs discovery to fail with the resolution")
	}
	calls := e.gh.calls["Collaborator"]
	e.reconcileProject()
	if e.gh.calls["Collaborator"] != calls {
		t.Errorf("the approvers were asked again on the next pass")
	}
	c := meta.FindStatusCondition(e.getProject().Status.Conditions, v1alpha1.ConditionApproversResolved)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonApproversUnreadable {
		t.Errorf("ApproversResolved = %+v, want False %s", c, ReasonApproversUnreadable)
	}
}
