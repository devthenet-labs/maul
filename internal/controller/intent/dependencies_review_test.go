// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// The review's regressions: each test pins one behaviour the first cut of
// intent dependencies got wrong.

// limitedEnv is an env whose Project runs at most one active intent,
// with one of them (issue 5) already planning.
func limitedEnv(t *testing.T) (*env, string) {
	t.Helper()
	p := testProject()
	p.Spec.Limits.MaxActiveIntents = 1
	e := newEnv(t, p)
	active := e.newIntentFor(5, approver)
	e.mustIntent(active)
	e.mustIntent(active)
	if ph := e.get(active).Status.Phase; ph != v1alpha1.IntentPlanning {
		t.Fatalf("the active intent is %s, want Planning", ph)
	}
	return e, active
}

// endIntent closes the Intent, as its own reconciler would on an ending,
// and returns the nudge its ending gives.
func (e *env) endIntent(name string) []ctrl.Request {
	e.t.Helper()
	cur := e.get(name)
	if err := v1alpha1.SetIntentPhase(cur, v1alpha1.IntentClosed, e.clock.Now()); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.Status().Update(context.Background(), cur); err != nil {
		e.t.Fatal(err)
	}
	return e.intent.nudgeWaiting(context.Background(), e.c, cur)
}

// relabel records actor removing the trigger label from issue 1 and
// applying it again.
func (e *env) relabel(actor string) int64 {
	e.gh.mu.Lock()
	e.gh.removeLabel(1, "maul:target", actor)
	e.gh.mu.Unlock()
	return e.gh.label(1, "maul:target", actor)
}

func (e *env) projectCondition() *metav1.Condition {
	e.t.Helper()
	e.reconcileProject()
	var p v1alpha1.Project
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &p); err != nil {
		e.t.Fatal(err)
	}
	return meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionDependenciesUnavailable)
}

func (e *env) setPermission(login, perm string) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.perms[login] = perm
}

// TestDependencyReleaseKeepsRequesterAttribution: a trigger label applied
// again while the intent waits is consumed as a replan that is not
// available; the plan the release starts is still attributed to the trigger
// that created the intent, never to the re-labeller.
func TestDependencyReleaseKeepsRequesterAttribution(t *testing.T) {
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, "alice")
	e := newEnv(t, p)
	e.setPermission("alice", ghclient.PermissionWrite)
	e.plainIssue(2)
	name := e.newIntentFor(1, "alice")
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)

	relabelled := e.relabel(approver)
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)
	if lt := e.get(name).Status.LastTrigger; lt == nil || lt.EventID != relabelled {
		t.Fatalf("lastTrigger = %+v, want the re-label consumed", lt)
	}

	e.gh.closeAs(2, "completed")
	e.waitPoll(name)
	e.wantStarted(name, v1alpha1.ReasonBlockersClosed)
	in := e.get(name)
	if tr := in.Status.Input.Trigger; tr == nil || tr.EventID != in.Spec.RequestedBy.EventID || tr.Login != "alice" {
		t.Fatalf("the plan is attributed to %+v, want the trigger that created the intent", tr)
	}
}

// TestDependenciesOffKeepsTheLimit: turning dependencies off (rollback)
// starts the intents that waited only as the Project has slots for them,
// oldest first; discovery counts them meanwhile.
func TestDependenciesOffKeepsTheLimit(t *testing.T) {
	e, active := limitedEnv(t)
	e.plainIssue(2)
	older := e.newIntent(approver)
	younger := e.newIntentFor(3, approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.gh.blockBy(3, fakeBlocker{number: 2})
	for _, n := range []string{older, younger} {
		e.mustIntent(n)
		e.mustIntent(n)
		e.wantWaiting(n, v1alpha1.ReasonOpenBlockers)
	}

	e.intent.Settings.IgnoreDependencies = true
	e.project.Settings.IgnoreDependencies = true
	for _, n := range []string{younger, older} {
		e.clock.Advance(time.Minute)
		e.mustIntent(n)
		if ph := e.get(n).Status.Phase; ph != v1alpha1.IntentPending {
			t.Fatalf("%s is %s with dependencies off and no slot, want Pending", n, ph)
		}
	}
	var list v1alpha1.IntentList
	if err := e.c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if got := activeIntents(&list, "target", e.project.Settings.IgnoreDependencies); got != 3 {
		t.Fatalf("activeIntents = %d with dependencies off, want 3: discovery must count the waiters", got)
	}

	reqs := e.endIntent(active)
	if len(reqs) != 2 {
		t.Fatalf("the ending nudged %v, want both waiters", reqs)
	}
	e.mustIntent(younger)
	if ph := e.get(younger).Status.Phase; ph != v1alpha1.IntentPending {
		t.Fatalf("the younger waiter is %s, want Pending behind the older", ph)
	}
	e.mustIntent(older)
	if ph := e.get(older).Status.Phase; ph != v1alpha1.IntentPlanning {
		t.Fatalf("the older waiter is %s, want Planning", ph)
	}
	e.clock.Advance(time.Minute)
	e.mustIntent(younger)
	if ph := e.get(younger).Status.Phase; ph != v1alpha1.IntentPending {
		t.Fatalf("the younger waiter is %s past the limit, want Pending", ph)
	}
}

// TestDependencyQueuedGiveUpKeepsItsPlace: an intent that gave up on a
// failing read while the Project had no slot stays queued for one while the
// read keeps failing, and starts as DependenciesUnanswered once one frees.
func TestDependencyQueuedGiveUpKeepsItsPlace(t *testing.T) {
	e, active := limitedEnv(t)
	name := e.newIntent(approver)
	e.gh.blockedByErr = ghError(http.StatusBadGateway, "bad gateway")
	e.mustIntent(name)
	e.mustIntent(name)
	for range 6 {
		e.waitPoll(name)
	}
	e.wantWaiting(name, v1alpha1.ReasonNoActiveSlot)
	if got := e.get(name).Status.Dependencies.ReleaseReason; got != v1alpha1.ReasonDependenciesUnanswered {
		t.Fatalf("releaseReason = %q, want %s", got, v1alpha1.ReasonDependenciesUnanswered)
	}
	for range 3 {
		e.waitPoll(name)
		e.wantWaiting(name, v1alpha1.ReasonNoActiveSlot)
	}
	e.endIntent(active)
	e.mustIntent(name)
	e.wantStarted(name, v1alpha1.ReasonDependenciesUnanswered)
}

// TestDependencyQueuedReleaseSurvivesAFailingRead: a released intent
// queued for a slot is not sent back to CheckingDependencies by a read that
// decides nothing.
func TestDependencyQueuedReleaseSurvivesAFailingRead(t *testing.T) {
	e, active := limitedEnv(t)
	e.plainIssue(3)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 3})
	e.mustIntent(name)
	e.mustIntent(name)
	e.gh.closeAs(3, "completed")
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonNoActiveSlot)
	e.gh.blockedByErr = ghError(http.StatusBadGateway, "bad gateway")
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonNoActiveSlot)
	e.endIntent(active)
	e.mustIntent(name)
	e.wantStarted(name, v1alpha1.ReasonBlockersClosed)
}

// TestDependencyQueuedDegradedReadNotRelabelled: a queued intent whose
// release read GitHub refuses starts with the refusal's reason, and the
// Project says the dependencies were not read.
func TestDependencyQueuedDegradedReadNotRelabelled(t *testing.T) {
	e, active := limitedEnv(t)
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.gh.closeAs(2, "completed")
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonNoActiveSlot)
	e.gh.blockedByErr = ghError(http.StatusForbidden, "no")
	e.endIntent(active)
	e.mustIntent(name)
	e.wantStarted(name, v1alpha1.ReasonDependenciesForbidden)
	if c := e.projectCondition(); c == nil || c.Status != metav1.ConditionTrue ||
		c.Reason != v1alpha1.ReasonDependenciesForbidden {
		t.Fatalf("Project condition = %+v, want True DependenciesForbidden", c)
	}
}

// TestDependencyLateCheckAfterRefusalReadsNothing: once an intent started
// because GitHub refused the read, its later polls never ask again for the
// blockers they cannot read.
func TestDependencyLateCheckAfterRefusalReadsNothing(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.gh.blockedByErr = ghError(http.StatusForbidden, "no")
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantStarted(name, v1alpha1.ReasonDependenciesForbidden)
	calls := e.gh.calls["ListBlockedBy"]
	for range 4 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	if got := e.gh.calls["ListBlockedBy"] - calls; got != 0 {
		t.Fatalf("%d blocker reads after a refused one started the intent, want none", got)
	}
}

// TestDependencyLateCheckAfterUnansweredStart: blockers maul could not read
// before the start are no news once GitHub answers again: they were not
// added late, so no notice says they were.
func TestDependencyLateCheckAfterUnansweredStart(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.gh.blockedByErr = ghError(http.StatusBadGateway, "bad gateway")
	e.mustIntent(name)
	e.mustIntent(name)
	for range 6 {
		e.waitPoll(name)
	}
	e.wantStarted(name, v1alpha1.ReasonDependenciesUnanswered)
	e.gh.blockedByErr = nil
	for range 3 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	if n := len(e.gh.withMarker(lateNoticeKey)); n != 0 || e.get(name).Status.Dependencies.LateNotice != nil {
		t.Fatalf("a blocker present from the start was noticed as added late (%d notices)", n)
	}
}

// TestDependencyLateCheckReadsOnlyOnChange: a started intent whose issue
// still shows a blocker it knows of reads the blockers once, then only when
// the issue's summary changes.
func TestDependencyLateCheckReadsOnlyOnChange(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	e.plainIssue(3)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.gh.comment(approver, "/maul start")
	e.waitPoll(name)
	e.wantStarted(name, v1alpha1.ReasonStartedByApprover)
	calls := e.gh.calls["ListBlockedBy"]
	for range 4 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	if got := e.gh.calls["ListBlockedBy"] - calls; got > 1 {
		t.Fatalf("%d blocker reads over four polls of an unchanged summary, want at most 1", got)
	}
	e.gh.blockBy(1, fakeBlocker{number: 3})
	for range 2 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	if n := len(e.gh.withMarker(lateNoticeKey)); n != 1 {
		t.Fatalf("%d late notices after a blocker was added, want 1", n)
	}
}

// TestDependencyCycleIgnoresAStartedPeer: a peer an approver started (now
// waiting only for a slot) breaks the cycle, so the other intent is not told
// it is in one.
func TestDependencyCycleIgnoresAStartedPeer(t *testing.T) {
	e, _ := limitedEnv(t)
	a := e.newIntent(approver)
	b := e.newIntentFor(3, approver)
	e.gh.blockBy(1, fakeBlocker{number: 3})
	e.gh.blockBy(3, fakeBlocker{number: 1})
	for _, n := range []string{a, b} {
		e.mustIntent(n)
		e.mustIntent(n)
	}
	e.waitPoll(a)
	e.wantWaiting(a, v1alpha1.ReasonDependencyCycle)
	e.wantWaiting(b, v1alpha1.ReasonDependencyCycle)

	e.gh.comment(approver, "/maul start")
	e.waitPoll(a)
	e.wantWaiting(a, v1alpha1.ReasonNoActiveSlot)
	e.waitPoll(b)
	e.wantWaiting(b, v1alpha1.ReasonOpenBlockers)
}

// TestDependencyUnreadableBoundedByTotal: a summary that counts a closed
// blocker as blocking (lag, or every blocker) holds nothing once every
// blocker GitHub counts is listed and closed as completed.
func TestDependencyUnreadableBoundedByTotal(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	e.gh.summaryCountsClosed = true
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.gh.closeAs(2, "completed")
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantStarted(name, v1alpha1.ReasonBlockersClosed)
}

// TestDependencyNudgeOnlyTheConcerned: an ending nudges the waiters it
// blocked and those queued for a slot, never every Pending intent.
func TestDependencyNudgeOnlyTheConcerned(t *testing.T) {
	e, active := limitedEnv(t)
	e.plainIssue(2)
	blocked := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 5})
	other := e.newIntentFor(3, approver)
	e.gh.blockBy(3, fakeBlocker{number: 2})
	queued := e.newIntentFor(4, approver)
	for _, n := range []string{blocked, other, queued} {
		e.mustIntent(n)
		e.mustIntent(n)
	}
	e.wantWaiting(blocked, v1alpha1.ReasonOpenBlockers)
	e.wantWaiting(other, v1alpha1.ReasonOpenBlockers)
	e.wantWaiting(queued, v1alpha1.ReasonNoActiveSlot)
	reqs := e.endIntent(active)
	got := make([]string, 0, len(reqs))
	for _, r := range reqs {
		got = append(got, r.Name)
	}
	slices.Sort(got)
	want := []string{blocked, queued}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the ending nudged %v, want %v", got, want)
	}
}

// TestDependencyReleaseRechecksAuthority: an approver who lost write
// access while the intent waited cannot start it by the trigger accepted
// weeks before: the release decides the trigger again and refuses it.
func TestDependencyReleaseRechecksAuthority(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)
	e.setPermission(approver, ghclient.PermissionRead)
	e.gh.closeAs(2, "completed")
	e.waitPoll(name)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentClosed {
		t.Fatalf("phase = %s, want Closed: the trigger's authority was not decided again", in.Status.Phase)
	}
	if runs := e.intentRuns(name); len(runs) != 0 {
		t.Fatalf("a refused release launched %d runs", len(runs))
	}
}

// TestDependencyUnansweredWordsTheBound: the issue and the Project word the
// give-up bound the controller runs with.
func TestDependencyUnansweredWordsTheBound(t *testing.T) {
	e := newEnv(t, testProject())
	e.intent.Settings.DependencyGiveUp = 5 * time.Minute
	e.project.Settings.DependencyGiveUp = 5 * time.Minute
	name := e.newIntent(approver)
	e.gh.blockedByErr = ghError(http.StatusBadGateway, "bad gateway")
	e.mustIntent(name)
	e.mustIntent(name)
	e.waitPoll(name)
	e.wantStarted(name, v1alpha1.ReasonDependenciesUnanswered)
	e.settle(name)
	if body := e.statusOf(1); !strings.Contains(body, "GitHub did not answer for 5 minutes") {
		t.Fatalf("status comment:\n%s", body)
	}
	if c := e.projectCondition(); c == nil || !strings.Contains(c.Message, "GitHub did not answer for 5 minutes") {
		t.Fatalf("Project condition = %+v", c)
	}
}
