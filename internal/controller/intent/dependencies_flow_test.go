// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// settle runs a few passes, a minute apart, so the run and the status
// comment catch up with a decision.
func (e *env) settle(name string) {
	e.t.Helper()
	for range 3 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
		e.runRuns()
	}
}

// plainIssue files issue n with no trigger: a blocker that is no intent.
func (e *env) plainIssue(n int64) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.issues[n] = &fakeIssue{number: n, title: "Blocker " + itoa(n), state: "open"}
}

// waitCondition is the Intent's WaitingOnDependencies condition, or nil.
func (e *env) waitCondition(name string) *metav1.Condition {
	return meta.FindStatusCondition(e.get(name).Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
}

// statusOf is the body of maul's status comment on issue n, "" without one.
func (e *env) statusOf(n int64) string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	for _, c := range e.gh.issues[n].comments {
		if c.UserLogin == e.gh.bot && strings.Contains(markerOf(c.Body), "maul:intent ") {
			return c.Body
		}
	}
	return ""
}

// waitPoll advances past a waiting intent's poll and reconciles it.
func (e *env) waitPoll(name string) {
	e.t.Helper()
	e.clock.Advance(waitingPollFactor * time.Minute)
	e.mustIntent(name)
}

func (e *env) wantWaiting(name, reason string) {
	e.t.Helper()
	in := e.get(name)
	c := e.waitCondition(name)
	if in.Status.Phase != v1alpha1.IntentPending || c == nil || c.Status != metav1.ConditionTrue || c.Reason != reason {
		e.t.Fatalf("intent %s: phase %s, condition %+v; want Pending waiting with %s", name, in.Status.Phase, c, reason)
	}
	if runs := e.intentRuns(name); len(runs) != 0 {
		e.t.Fatalf("a waiting intent has %d runs", len(runs))
	}
}

func (e *env) wantStarted(name, reason string) {
	e.t.Helper()
	in := e.get(name)
	c := e.waitCondition(name)
	if in.Status.Phase != v1alpha1.IntentPlanning || c == nil || c.Status != metav1.ConditionFalse || c.Reason != reason {
		e.t.Fatalf("intent %s: phase %s, condition %+v; want Planning released with %s", name, in.Status.Phase, c,
			reason)
	}
}

// TestDependencyWaitAndRelease: an intent blocked by an open issue waits in
// Pending, with no run, its status comment naming the blocker; once the
// blocker is closed as completed it plans on its next waiting poll.
func TestDependencyWaitAndRelease(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name) // Pending
	e.mustIntent(name) // decides: waits
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)
	e.mustIntent(name) // status comment
	body := e.statusOf(1)
	for _, want := range []string{"waiting for the issues this one is blocked by", "[#2](" + intentRepoURL +
		"/issues/2): open", "`/maul start`, `/maul cancel`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("status comment lacks %q:\n%s", want, body)
		}
	}
	// No poll due: nothing more is read.
	calls := e.gh.calls["ListBlockedBy"]
	e.clock.Advance(time.Minute)
	e.mustIntent(name)
	if got := e.gh.calls["ListBlockedBy"]; got != calls {
		t.Fatalf("a waiting intent read its blockers %d times before its waiting poll", got-calls)
	}

	e.gh.closeAs(2, "completed")
	e.waitPoll(name)
	e.wantStarted(name, v1alpha1.ReasonBlockersClosed)
	e.settle(name)
	if body := e.statusOf(1); !strings.Contains(body, "**Dependencies:** waited for [#2](") {
		t.Fatalf("released status comment lacks the history line:\n%s", body)
	}
}

// TestDependencyNotCompletedKeepsWaiting: a blocker closed as not planned
// never landed, so the intent keeps waiting and says why.
func TestDependencyNotCompletedKeepsWaiting(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.gh.closeAs(2, "not_planned")
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonBlockerNotCompleted)
	e.mustIntent(name)
	if body := e.statusOf(1); !strings.Contains(body, "closed as not planned") {
		t.Fatalf("status comment does not say the blocker never landed:\n%s", body)
	}
}

// TestDependencyUnreadableWaits: a blocker GitHub counts but does not show
// (another repository the token may not read) holds the start, unnamed.
func TestDependencyUnreadableWaits(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{repo: "secret-org/private", number: 7, state: "open", hidden: true})
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonBlockerUnreadable)
	e.mustIntent(name)
	body := e.statusOf(1)
	if !strings.Contains(body, "1 issue maul cannot read") || strings.Contains(body, "secret-org") {
		t.Fatalf("status comment:\n%s", body)
	}
}

// TestDependencyCrossRepositoryNaming: a readable blocker in one of the
// Project's repositories is named; one elsewhere is counted, never named.
func TestDependencyCrossRepositoryNaming(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{repo: "acme/app", number: 4, state: "open"})
	e.gh.blockBy(1, fakeBlocker{repo: "other/lib", number: 5, state: "open"})
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)
	e.mustIntent(name)
	body := e.statusOf(1)
	if !strings.Contains(body, "[acme/app#4](https://github.com/acme/app/issues/4): open") ||
		!strings.Contains(body, "1 issue in another repository: open") || strings.Contains(body, "other/lib") {
		t.Fatalf("status comment:\n%s", body)
	}
}

// TestDependencyUnsupportedStarts: a GitHub without issue dependencies (404)
// starts the intent at once, and its Project says dependencies are not
// honoured.
func TestDependencyUnsupportedStarts(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
	}{
		{http.StatusNotFound, v1alpha1.ReasonDependenciesUnsupported},
		{http.StatusGone, v1alpha1.ReasonDependenciesUnsupported},
		{http.StatusForbidden, v1alpha1.ReasonDependenciesForbidden},
	} {
		t.Run(tc.reason+itoa(int64(tc.status)), func(t *testing.T) {
			e := newEnv(t, testProject())
			name := e.newIntent(approver)
			e.gh.blockedByErr = ghError(tc.status, "no")
			e.mustIntent(name)
			e.mustIntent(name)
			e.wantStarted(name, tc.reason)
			e.settle(name)
			e.reconcileProject()
			var p v1alpha1.Project
			if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &p); err != nil {
				t.Fatal(err)
			}
			c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionDependenciesUnavailable)
			if c == nil || c.Status != metav1.ConditionTrue || c.Reason != tc.reason || !strings.Contains(c.Message, name) {
				t.Fatalf("Project condition = %+v", c)
			}
			if body := e.statusOf(1); !strings.Contains(body, "maul could not read this issue's dependencies") {
				t.Fatalf("status comment:\n%s", body)
			}
		})
	}
}

// TestDependencyTransientGivesUpAcrossRestart: a read GitHub keeps failing
// waits, then starts at the bound, which is timed from status so a
// restarted controller does not begin it again.
func TestDependencyTransientGivesUpAcrossRestart(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.newIntent(approver)
	e.gh.blockedByErr = ghError(http.StatusBadGateway, "bad gateway")
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonCheckingDependencies)
	first := e.get(name).Status.Dependencies.UnavailableSince
	if first == nil {
		t.Fatal("no failing streak recorded")
	}
	for range 4 {
		e.waitPoll(name)
		e.wantWaiting(name, v1alpha1.ReasonCheckingDependencies)
	}
	// A restart: a fresh reconciler, nothing in memory.
	e.intent = &IntentReconciler{Client: e.c, APIReader: e.c, GitHub: e.gh, Settings: e.intent.Settings,
		Images: e.intent.Images, Nudger: e.nudger, Now: e.clock.Now}
	e.mustIntent(name)
	if got := e.get(name).Status.Dependencies.UnavailableSince; got == nil || !got.Equal(first) {
		t.Fatalf("the restart moved the streak's start to %v, want %v", got, first)
	}
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonCheckingDependencies)
	e.waitPoll(name) // 30 minutes since the first failure
	e.wantStarted(name, v1alpha1.ReasonDependenciesUnanswered)
}

// TestDependencyStartCommand: an approver's /maul start starts a waiting
// intent and is recorded; a non-approver's is refused; an edited comment is
// not taken as its author's; the reply is posted once.
func TestDependencyStartCommand(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)

	refused := e.gh.comment("mallory", "/maul start")
	edited := e.gh.comment(approver, "/maul start")
	e.gh.editComment(edited, "/maul start")
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)
	if len(e.gh.withMarker("comment-"+itoa(refused))) != 1 || len(e.gh.withMarker("comment-"+itoa(edited))) != 1 {
		t.Fatalf("the refused and the edited start were not each answered once: %d, %d",
			len(e.gh.withMarker("comment-"+itoa(refused))), len(e.gh.withMarker("comment-"+itoa(edited))))
	}

	id := e.gh.comment(approver, "/maul start")
	e.waitPoll(name)
	in := e.get(name)
	if sb := in.Status.Dependencies.StartedBy; sb == nil || sb.EventID != id || sb.Login != approver {
		t.Fatalf("startedBy = %+v, want comment %d by %s", sb, id, approver)
	}
	e.wantStarted(name, v1alpha1.ReasonStartedByApprover)
	if in.Status.Input.Trigger == nil || in.Status.Input.Trigger.EventID != in.Spec.RequestedBy.EventID {
		t.Fatalf("the plan is attributed to %+v, want the trigger that created the intent", in.Status.Input.Trigger)
	}
	for range 3 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	if n := len(e.gh.withMarker("comment-" + itoa(id))); n != 1 {
		t.Fatalf("%d replies to the start, want 1", n)
	}
	if body := e.statusOf(1); !strings.Contains(body, "started by `peter` with `/maul start` while [#2](") {
		t.Fatalf("status comment:\n%s", body)
	}

	// Planning now: a second start is not available.
	again := e.gh.comment(approver, "/maul start")
	e.clock.Advance(time.Minute)
	e.mustIntent(name)
	got := e.gh.withMarker("comment-" + itoa(again))
	if len(got) != 1 || !strings.Contains(got[0].Body, "Planning") {
		t.Fatalf("a start while planning was answered %+v", got)
	}
}

// TestDependencyCancelWhileWaiting: /maul cancel and a human close end a
// waiting intent as they end any Pending one.
func TestDependencyCancelWhileWaiting(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.gh.comment(approver, "/maul cancel")
	e.waitPoll(name)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentClosed {
		t.Fatalf("phase = %s, want Closed", in.Status.Phase)
	}
}

// TestDependencyRefusedTriggerNeverWaits: a non-approver's trigger closes
// the intent at once, whatever it is blocked by, and reads no blockers.
func TestDependencyRefusedTriggerNeverWaits(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(2)
	name := e.newIntent("mallory")
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentClosed || in.Status.Dependencies != nil {
		t.Fatalf("phase %s, dependencies %+v; want Closed and none", in.Status.Phase, in.Status.Dependencies)
	}
	if n := e.gh.calls["ListBlockedBy"]; n != 0 {
		t.Fatalf("a refused trigger read its blockers %d times", n)
	}
}

// TestDependencySlot: a released waiter starts only once the Project has a
// slot for it, and an intent ending nudges it at once.
func TestDependencySlot(t *testing.T) {
	p := testProject()
	p.Spec.Limits.MaxActiveIntents = 1
	e := newEnv(t, p)
	e.plainIssue(2)
	active := e.newIntentFor(5, approver)
	e.mustIntent(active)
	e.mustIntent(active)
	if ph := e.get(active).Status.Phase; ph != v1alpha1.IntentPlanning {
		t.Fatalf("the first intent is %s, want Planning", ph)
	}
	name := e.newIntent(approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.mustIntent(name)
	e.mustIntent(name)
	e.wantWaiting(name, v1alpha1.ReasonOpenBlockers)
	var list v1alpha1.IntentList
	if err := e.c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if got := activeIntents(&list, "target"); got != 1 {
		t.Fatalf("activeIntents = %d with one waiting on blockers, want 1", got)
	}

	e.gh.closeAs(2, "completed")
	e.waitPoll(name)
	e.wantWaiting(name, v1alpha1.ReasonNoActiveSlot)
	if err := e.c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if got := activeIntents(&list, "target"); got != 2 {
		t.Fatalf("activeIntents = %d with one queued for a slot, want 2", got)
	}
	e.mustIntent(name)
	if body := e.statusOf(1); !strings.Contains(body, "ready to start, and waiting for an active slot") {
		t.Fatalf("status comment:\n%s", body)
	}

	// The active intent ends: the waiter is nudged and starts on the pass
	// it is given, without waiting for its poll.
	cur := e.get(active)
	if err := v1alpha1.SetIntentPhase(cur, v1alpha1.IntentClosed, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Status().Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reqs := e.intent.nudgeWaiting(context.Background(), e.c, cur)
	if len(reqs) != 1 || reqs[0].Name != name {
		t.Fatalf("nudged %v, want %s", reqs, name)
	}
	e.mustIntent(name)
	e.wantStarted(name, v1alpha1.ReasonBlockersClosed)
}

// TestDependencyLateBlocker: a blocker added after the start never stops
// the intent; it gets one notice, recorded, and no second.
func TestDependencyLateBlocker(t *testing.T) {
	e := newEnv(t, testProject())
	e.plainIssue(3)
	name := e.awaiting()
	e.gh.blockBy(1, fakeBlocker{number: 3})
	for range 3 {
		e.clock.Advance(time.Minute)
		e.mustIntent(name)
	}
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentAwaitingApproval {
		t.Fatalf("phase = %s, want AwaitingApproval still", in.Status.Phase)
	}
	if in.Status.Dependencies.LateNotice == nil || len(in.Status.Dependencies.Late) != 1 {
		t.Fatalf("dependencies = %+v, want the late notice recorded", in.Status.Dependencies)
	}
	if n := len(e.gh.withMarker(lateNoticeKey)); n != 1 {
		t.Fatalf("%d late notices, want 1", n)
	}
	if body := e.statusOf(1); !strings.Contains(body, "**Blocked by, added after planning started:** [#3](") {
		t.Fatalf("status comment:\n%s", body)
	}
}

// TestDependenciesOff: with --intent-dependencies=false nothing is read,
// a blocked intent plans at once, the comment is exactly as before, and the
// Project says dependencies are off.
func TestDependenciesOff(t *testing.T) {
	run := func(ignore bool) (*env, string) {
		e := newEnv(t, testProject())
		e.intent.Settings.IgnoreDependencies = ignore
		e.project.Settings.IgnoreDependencies = ignore
		e.plainIssue(2)
		name := e.newIntent(approver)
		if ignore {
			e.gh.blockBy(1, fakeBlocker{number: 2})
		}
		e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
		return e, name
	}
	off, name := run(true)
	if n := off.gh.calls["ListBlockedBy"]; n != 0 {
		t.Fatalf("ListBlockedBy called %d times with dependencies off", n)
	}
	in := off.get(name)
	if in.Status.Dependencies != nil || off.waitCondition(name) != nil {
		t.Fatalf("dependency state written with the flag off: %+v", in.Status)
	}
	on, _ := run(false)
	if a, b := off.statusOf(1), on.statusOf(1); a != b {
		t.Fatalf("status comments differ:\noff:\n%s\non:\n%s", a, b)
	}
	off.reconcileProject()
	var p v1alpha1.Project
	if err := off.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &p); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionDependenciesUnavailable); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != v1alpha1.ReasonDependenciesOff {
		t.Fatalf("Project condition = %+v, want DependenciesOff", c)
	}
	on.reconcileProject()
	if err := on.c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "target"}, &p); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionDependenciesUnavailable); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonDependenciesRead {
		t.Fatalf("Project condition = %+v, want DependenciesRead", c)
	}
}

// TestDependencyCycle: two intents blocking each other both wait, and both
// say so once each has read the other's record.
func TestDependencyCycle(t *testing.T) {
	e := newEnv(t, testProject())
	a := e.newIntent(approver)
	b := e.newIntentFor(2, approver)
	e.gh.blockBy(1, fakeBlocker{number: 2})
	e.gh.blockBy(2, fakeBlocker{number: 1})
	for _, n := range []string{a, b} {
		e.mustIntent(n)
		e.mustIntent(n)
	}
	e.wantWaiting(a, v1alpha1.ReasonOpenBlockers) // b had no record yet
	e.wantWaiting(b, v1alpha1.ReasonDependencyCycle)
	e.waitPoll(a)
	e.wantWaiting(a, v1alpha1.ReasonDependencyCycle)
	e.mustIntent(a)
	e.mustIntent(b)
	if body := e.statusOf(1); !strings.Contains(body, "**Dependency cycle:** [#2](") {
		t.Fatalf("issue 1's comment:\n%s", body)
	}
	if body := e.statusOf(2); !strings.Contains(body, "**Dependency cycle:** [#1](") {
		t.Fatalf("issue 2's comment:\n%s", body)
	}
}
