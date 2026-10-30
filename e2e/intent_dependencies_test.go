// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// Intent dependencies end to end: GitHub's "blocked by" relationships on
// the fake, read by the shipped intent-controller. An intent blocked by an
// open issue waits in Pending, holding no slot and launching nothing, until
// every blocker is closed as completed; /maul start overrides; a dependencies
// API that is not there starts it as before. intentArgs poll every second,
// so a waiting intent polls every five.

// startLimited is startIntents for a Project of at most limit active
// intents.
func startLimited(t *testing.T, limit int32, extra ...string) *intentEnv {
	t.Helper()
	cl, gh, registry := startSourceStack(t, appRepo)
	e := withIntentsFor(t, cl, gh, v1alpha1.ProjectSpec{
		IntentRepository: intentRepoURL,
		Approvers:        v1alpha1.ProjectApprovers{Logins: []string{approver.Login, reader.Login}},
		Repositories:     []v1alpha1.ProjectRepository{{Name: appRepo, URL: appRepoURL}},
		Limits:           v1alpha1.ProjectLimits{MaxActiveIntents: limit},
	}, extra...)
	e.registry = registry
	return e
}

// plainIssue opens an issue in owner/repo that is no intent: no trigger.
func (e *intentEnv) plainIssue(repo, title string) int {
	return e.gh.OpenIssue(intentOwner, repo, title, "A blocker.", nil, approver)
}

// fileBlocked files an intent blocked by blockers, the relationships set
// before the trigger is applied, as a person does who marks the dependency
// and then asks for the work.
func (e *intentEnv) fileBlocked(t *testing.T, title string, blockers ...int) (int, string) {
	t.Helper()
	number := e.gh.OpenIssue(intentOwner, intentRepo, title, "Build on the blockers' work.", nil, approver)
	for _, b := range blockers {
		e.gh.AddBlockedBy(number, b)
	}
	e.gh.LabelIssue(number, triggerLabel, approver)
	return number, v1alpha1.IntentName(projectName, int64(number))
}

// waitWaiting waits for the Intent to wait in Pending with reason, and
// checks it launched nothing.
func (e *intentEnv) waitWaiting(t *testing.T, name, reason string) *v1alpha1.Intent {
	t.Helper()
	in := e.waitFor(t, name, "waiting with "+reason, func(in *v1alpha1.Intent) bool {
		c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
		return in.Status.Phase == v1alpha1.IntentPending && c != nil && c.Status == metav1.ConditionTrue &&
			c.Reason == reason
	})
	if runs := e.runs(t, name); len(runs) != 0 {
		t.Fatalf("waiting intent %s has runs: %s", name, e.describeRuns(t, name))
	}
	return in
}

// waitReleased waits for the Intent's wait to have ended with reason, and
// for its plan to be posted.
func (e *intentEnv) waitReleased(t *testing.T, name, reason string) *v1alpha1.Intent {
	t.Helper()
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reason {
		t.Fatalf("intent %s released with %+v, want %s", name, c, reason)
	}
	return in
}

// waitStatusSays waits for the intent's one status comment to contain
// every one of want, and returns it.
func (e *intentEnv) waitStatusSays(t *testing.T, number int, name string, want ...string) string {
	t.Helper()
	var body string
	eventually(t, fmt.Sprintf("issue #%d's status comment to say %q", number, want), func() bool {
		st := e.own(number, templates.IntentStatusMarker(namespace, name))
		if len(st) != 1 {
			return false
		}
		body = st[0].Body
		for _, w := range want {
			if !strings.Contains(body, w) {
				return false
			}
		}
		return true
	})
	return body
}

// driveToMerge approves the posted plan, waits for the pull request and
// merges it, and returns the merged Intent.
func (e *intentEnv) driveToMerge(t *testing.T, number int, name string) *v1alpha1.Intent {
	t.Helper()
	in := e.waitPhase(t, name, v1alpha1.IntentAwaitingApproval)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	pr := in.Status.PullRequests[0]
	e.gh.MergePull(int(pr.Number), v1alpha1.IntentBranchPrefix+name, "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	return e.waitPhase(t, name, v1alpha1.IntentMerged)
}

// planRunCreated is when the Intent's first plan run was created.
func (e *intentEnv) planRunCreated(t *testing.T, name string) time.Time {
	t.Helper()
	run := e.run(t, v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1))
	return run.CreationTimestamp.Time
}

// projectCondition waits for the Project's DependenciesUnavailable
// condition to read status and reason.
func (e *intentEnv) projectCondition(t *testing.T, status metav1.ConditionStatus, reason string) {
	t.Helper()
	eventually(t, fmt.Sprintf("the project's DependenciesUnavailable to be %s %s", status, reason), func() bool {
		c := meta.FindStatusCondition(e.project(t).Status.Conditions, v1alpha1.ConditionDependenciesUnavailable)
		return c != nil && c.Status == status && c.Reason == reason
	})
}

// TestIntentDependenciesWaitRelease: an intent blocked by an open issue
// waits visibly and plans once the blocker is closed as completed, within
// one waiting poll.
func TestIntentDependenciesWaitRelease(t *testing.T) {
	e := startIntents(t)

	// 1. Wait: D is blocked by X, an open issue that is no intent.
	x := e.plainIssue(intentRepo, "Blocker X")
	d, dName := e.fileBlocked(t, "D", x)
	in := e.waitWaiting(t, dName, v1alpha1.ReasonOpenBlockers)
	if deps := in.Status.Dependencies; deps == nil || len(deps.Blockers) != 1 || deps.Blockers[0].Number != int64(x) {
		t.Fatalf("dependencies = %+v, want #%d recorded", in.Status.Dependencies, x)
	}
	e.waitStatusSays(t, d, dName, "waiting for the issues this one is blocked by",
		fmt.Sprintf("[#%d](%s/issues/%d): open", x, intentRepoURL, x),
		"`/maul start`, `/maul cancel`")

	// 2. Release: X closed as completed; D plans within one waiting poll.
	e.gh.CloseIssueAs(x, "completed", approver)
	e.waitReleased(t, dName, v1alpha1.ReasonBlockersClosed)
	e.waitStatusSays(t, d, dName, fmt.Sprintf("**Dependencies:** waited for [#%d](", x))
	e.projectCondition(t, metav1.ConditionFalse, v1alpha1.ReasonDependenciesRead)
}

// chainPollInterval is the issue poll of the chain test: its waiting poll
// (five of them, 40s) is far longer than the release bound it asserts, so
// only the end-of-intent nudge can meet that bound.
const chainPollInterval = 8 * time.Second

// TestIntentDependenciesChain: a chain of three intents plans in order,
// each released by the merge of the one before: its issue closed as
// completed, and the end of an intent nudging the waiters it blocked within
// seconds, long before their slow waiting poll would.
func TestIntentDependenciesChain(t *testing.T) {
	e := startIntents(t, "--intent-poll-interval", chainPollInterval.String())

	// 3. The chain: C blocked by B, B blocked by A, all intents.
	a, aName := e.fileBlocked(t, "A")
	b, bName := e.fileBlocked(t, "B", a)
	_, cName := e.fileBlocked(t, "C", b)
	e.waitWaiting(t, bName, v1alpha1.ReasonOpenBlockers)
	e.waitWaiting(t, cName, v1alpha1.ReasonOpenBlockers)

	aDone := e.driveToMerge(t, a, aName)
	e.waitReleased(t, bName, v1alpha1.ReasonBlockersClosed)
	e.waitWaiting(t, cName, v1alpha1.ReasonOpenBlockers)
	bDone := e.driveToMerge(t, b, bName)
	e.waitReleased(t, cName, v1alpha1.ReasonBlockersClosed)

	// The order: each plan was made only after its blocker merged, and the
	// merge's nudge released it at once: well inside the 40s waiting poll,
	// which alone would almost never meet the bound.
	for _, step := range []struct {
		blocker *v1alpha1.Intent
		waiter  string
	}{{aDone, bName}, {bDone, cName}} {
		merged, planned := step.blocker.Status.CompletedAt.Time, e.planRunCreated(t, step.waiter)
		if planned.Before(merged.Truncate(time.Second)) {
			t.Errorf("%s planned at %s, before %s merged at %s", step.waiter, planned, step.blocker.Name, merged)
		}
		if gap := planned.Sub(merged); gap > 5*time.Second {
			t.Errorf("%s planned %s after %s merged: the end of an intent did not nudge it", step.waiter, gap,
				step.blocker.Name)
		}
	}
}

// TestIntentDependenciesStartNotPlannedAndCycle: an approver's /maul start
// starts a waiting intent and is recorded; a blocker closed as not planned
// keeps its waiter waiting; two intents blocking each other both wait and
// both say so, and a start on one releases the other once it merges.
func TestIntentDependenciesStartNotPlannedAndCycle(t *testing.T) {
	e := startLimited(t, 4)

	// 4. /maul start: a non-approver's is refused, an approver's starts it.
	x := e.plainIssue(intentRepo, "Blocker X")
	s, sName := e.fileBlocked(t, "S", x)
	e.waitWaiting(t, sName, v1alpha1.ReasonOpenBlockers)
	refused := e.gh.CommentAs(s, "/maul start", mallory)
	eventually(t, "mallory's /maul start refused", func() bool {
		return len(e.own(s, notice(sName, fmt.Sprintf("comment-%d", refused)))) == 1
	})
	e.waitWaiting(t, sName, v1alpha1.ReasonOpenBlockers)
	started := e.gh.CommentAs(s, "/maul start", approver)
	in := e.waitReleased(t, sName, v1alpha1.ReasonStartedByApprover)
	if sb := in.Status.Dependencies.StartedBy; sb == nil || sb.EventID != started || sb.Login != approver.Login {
		t.Fatalf("startedBy = %+v, want comment %d by %s", sb, started, approver.Login)
	}
	if tr := in.Status.Input.Trigger; tr == nil || tr.EventID != in.Spec.RequestedBy.EventID {
		t.Errorf("the plan is attributed to %+v, want the trigger that created the intent", tr)
	}
	e.waitStatusSays(t, s, sName, "**Dependencies:** started by `octocat` with `/maul start` while [#")
	consistently(t, "the start to be answered once", func() bool {
		return len(e.own(s, notice(sName, fmt.Sprintf("comment-%d", started)))) == 1
	})

	// 5. Not planned: the blocker never landed, so the waiter keeps waiting.
	y := e.plainIssue(intentRepo, "Blocker Y")
	n, nName := e.fileBlocked(t, "N", y)
	e.waitWaiting(t, nName, v1alpha1.ReasonOpenBlockers)
	e.gh.CloseIssueAs(y, "not_planned", approver)
	e.waitWaiting(t, nName, v1alpha1.ReasonBlockerNotCompleted)
	e.waitStatusSays(t, n, nName, "closed as not planned, so what it was for never landed",
		"was closed without being completed")

	// 8. A cycle: P blocked by Q, Q blocked by P.
	p := e.gh.OpenIssue(intentOwner, intentRepo, "P", "One half.", nil, approver)
	q := e.gh.OpenIssue(intentOwner, intentRepo, "Q", "The other half.", nil, approver)
	e.gh.AddBlockedBy(p, q)
	e.gh.AddBlockedBy(q, p)
	e.gh.LabelIssue(p, triggerLabel, approver)
	e.gh.LabelIssue(q, triggerLabel, approver)
	pName, qName := v1alpha1.IntentName(projectName, int64(p)), v1alpha1.IntentName(projectName, int64(q))
	e.waitWaiting(t, pName, v1alpha1.ReasonDependencyCycle)
	e.waitWaiting(t, qName, v1alpha1.ReasonDependencyCycle)
	e.waitStatusSays(t, p, pName, fmt.Sprintf("**Dependency cycle:** [#%d](", q))
	e.waitStatusSays(t, q, qName, fmt.Sprintf("**Dependency cycle:** [#%d](", p))
	e.gh.CommentAs(p, "/maul start", approver)
	e.waitReleased(t, pName, v1alpha1.ReasonStartedByApprover)
	e.waitWaiting(t, qName, v1alpha1.ReasonOpenBlockers)
	e.driveToMerge(t, p, pName)
	e.waitReleased(t, qName, v1alpha1.ReasonBlockersClosed)
}

// TestIntentDependenciesCrossRepositoryAndUnavailable: a blocker the token
// cannot see waits, unnamed; one GitHub shows in a Project repository is
// named, one elsewhere only counted; a GitHub without the feature starts the
// intent at once, and a failing one starts it at the give-up bound, the
// Project saying which.
func TestIntentDependenciesCrossRepositoryAndUnavailable(t *testing.T) {
	e := startLimited(t, 4, "--intent-dependency-give-up", "12s")

	// 6. Cross-repository blockers: one in the Project's application
	//    repository, one in a repository the Project does not list.
	inApp := e.plainIssue(appRepo, "An app issue")
	secret := e.plainIssue("secret-plans", "A private matter")
	x, xName := e.fileBlocked(t, "X", inApp, secret)
	e.waitWaiting(t, xName, v1alpha1.ReasonBlockerUnreadable)
	body := e.waitStatusSays(t, x, xName, "2 issues maul cannot read")
	if strings.Contains(body, "secret-plans") {
		t.Fatalf("the status comment names a repository outside the Project:\n%s", body)
	}
	e.gh.SetCrossRepositoryBlockersVisible(true)
	e.waitWaiting(t, xName, v1alpha1.ReasonOpenBlockers)
	body = e.waitStatusSays(t, x, xName,
		fmt.Sprintf("[%s/%s#%d](%s/issues/%d): open", intentOwner, appRepo, inApp, appRepoURL, inApp),
		"1 issue in another repository: open")
	if strings.Contains(body, "secret-plans") || strings.Contains(body, fmt.Sprintf("#%d", secret)) {
		t.Fatalf("the status comment names the other repository's blocker:\n%s", body)
	}

	// 7a. A GitHub without issue dependencies: the intent starts at once,
	//     and so does the one still waiting, and the Project says why.
	e.gh.SetDependenciesUnsupported(true)
	u, uName := e.fileBlocked(t, "U")
	e.waitReleased(t, uName, v1alpha1.ReasonDependenciesUnsupported)
	e.waitStatusSays(t, u, uName, "this GitHub does not offer issue dependencies")
	e.projectCondition(t, metav1.ConditionTrue, v1alpha1.ReasonDependenciesUnsupported)
	e.waitReleased(t, xName, v1alpha1.ReasonDependenciesUnsupported)

	// 7b. A failing one: the intent waits, then starts at the bound.
	e.gh.SetDependenciesUnsupported(false)
	e.gh.SetDependenciesFailing(true)
	blocker := e.plainIssue(intentRepo, "Blocker")
	f, fName := e.fileBlocked(t, "F", blocker)
	e.waitWaiting(t, fName, v1alpha1.ReasonCheckingDependencies)
	e.waitStatusSays(t, f, fName, "could not read this issue's dependencies from GitHub just now")
	e.waitReleased(t, fName, v1alpha1.ReasonDependenciesUnanswered)
	e.projectCondition(t, metav1.ConditionTrue, v1alpha1.ReasonDependenciesUnanswered)
}

// TestIntentDependenciesThrottled403Waits: a 403 that only its Retry-After
// header marks as throttling is no permission refusal: a blocked intent
// keeps waiting (and the Project reports no DependenciesForbidden), then
// waits on its blocker once GitHub answers again.
func TestIntentDependenciesThrottled403Waits(t *testing.T) {
	e := startIntents(t)
	e.gh.SetDependenciesThrottled(true)
	x := e.plainIssue(intentRepo, "Blocker X")
	_, dName := e.fileBlocked(t, "T", x)
	e.waitWaiting(t, dName, v1alpha1.ReasonCheckingDependencies)
	if c := meta.FindStatusCondition(e.project(t).Status.Conditions, v1alpha1.ConditionDependenciesUnavailable); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonDependenciesForbidden {
		t.Fatalf("a throttled read was reported as %+v", c)
	}
	e.gh.SetDependenciesThrottled(false)
	e.waitWaiting(t, dName, v1alpha1.ReasonOpenBlockers)
}

// TestIntentDependenciesLateSlotAndOff: a released waiter holds back for
// the Project's one active slot until the active intent merges; a blocker
// added after the start gets one notice and stops nothing; and with
// --intent-dependencies=false a blocked intent plans at once.
func TestIntentDependenciesLateSlotAndOff(t *testing.T) {
	e := startLimited(t, 1)

	// 11. The slot: B, blocked, holds none while it waits, so A is
	//     discovered and takes the Project's one slot.
	x := e.plainIssue(intentRepo, "Blocker X")
	b, bName := e.fileBlocked(t, "B", x)
	e.waitWaiting(t, bName, v1alpha1.ReasonOpenBlockers)
	a, aName := e.fileBlocked(t, "A")
	e.waitPhase(t, aName, v1alpha1.IntentAwaitingApproval)
	eventually(t, "the project to count A alone active", func() bool { return e.project(t).Status.ActiveIntents == 1 })
	e.gh.CloseIssueAs(x, "completed", approver)
	e.waitWaiting(t, bName, v1alpha1.ReasonNoActiveSlot)
	e.waitStatusSays(t, b, bName, "ready to start, and waiting for an active slot")

	// 10. A blocker added to A after it started: one notice, A goes on.
	late := e.plainIssue(intentRepo, "A late blocker")
	e.gh.AddBlockedBy(a, late)
	lateNotice := notice(aName, "dependencies-late")
	eventually(t, "the late-blocker notice", func() bool { return len(e.own(a, lateNotice)) == 1 })
	consistently(t, "one late notice, and A still awaiting approval", func() bool {
		return len(e.own(a, lateNotice)) == 1 &&
			e.intent(t, aName).Status.Phase == v1alpha1.IntentAwaitingApproval
	})
	e.waitStatusSays(t, a, aName, fmt.Sprintf("**Blocked by, added after planning started:** [#%d](", late))

	e.driveToMerge(t, a, aName)
	e.waitReleased(t, bName, v1alpha1.ReasonBlockersClosed)

	// 9. Dependencies off: B's slot freed by a cancel, C plans at once
	//    although its blocker is open, and nothing is read.
	e.restartIntents(t, "--intent-dependencies=false")
	e.projectCondition(t, metav1.ConditionTrue, v1alpha1.ReasonDependenciesOff)
	e.gh.CommentAs(b, "/maul cancel", approver)
	e.waitFor(t, bName, "closed", func(in *v1alpha1.Intent) bool { return in.Status.Phase == v1alpha1.IntentClosed })
	reads := e.gh.BlockedByReads()
	z := e.plainIssue(intentRepo, "Blocker Z")
	_, cName := e.fileBlocked(t, "C", z)
	in := e.waitPhase(t, cName, v1alpha1.IntentAwaitingApproval)
	if in.Status.Dependencies != nil ||
		meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionWaitingOnDependencies) != nil {
		t.Errorf("dependency state written with dependencies off: %+v", in.Status)
	}
	if got := e.gh.BlockedByReads(); got != reads {
		t.Errorf("%d blocked-by reads with dependencies off", got-reads)
	}
}
