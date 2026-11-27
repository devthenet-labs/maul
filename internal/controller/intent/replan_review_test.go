// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// replanEnv is an env whose controller runs with --intent-replan-in-review.
func replanEnv(t *testing.T, p *v1alpha1.Project) *env {
	t.Helper()
	e := newEnv(t, p)
	e.intent.Settings.ReplanInReview = true
	return e
}

// notices are maul's comments on the issue keyed key.
func (e *env) notices(name, key string) []string {
	e.t.Helper()
	var out []string
	for _, c := range e.gh.ownComments() {
		if markerOf(c.Body) == templates.NoticeMarker(testNS, name, key) {
			out = append(out, c.Body)
		}
	}
	return out
}

// prNotices are maul's comments on pull request n keyed key.
func (e *env) prNotices(name string, n int64, key string) []string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []string
	for _, c := range e.gh.prComments[n] {
		if markerOf(c.Body) == templates.NoticeMarker(testNS, name, key) {
			out = append(out, c.Body)
		}
	}
	return out
}

// planContextOf is the investigation.md the plan run of input revision rev
// was handed: the context file of a replan.
func (e *env) planContextOf(rev int32) string {
	e.t.Helper()
	for _, s := range e.specsOf("plan") {
		if strings.Contains(s.Owner, "-plan-r"+itoa(int64(rev))+"-") {
			return s.InvestigationMarkdown
		}
	}
	e.t.Fatalf("no plan Job of revision %d", rev)
	return ""
}

// TestReplanInReview: an approver's /maul replan with a note, in review,
// once a review round on the pull request ran: the pull request gets one
// comment and is closed; the issue one reply; the intent plans again from
// the issue at the next input revision, attributed to the replanner, its
// planner handed the old plan, the pull request's link and state, the
// approver's review and the note; nothing of review is left on the
// intent. The new plan needs a new approval (the approve label was taken
// away before it was posted), is built on a branch of its own, opened as a
// new pull request, and its merge closes the issue.
func TestReplanInReview(t *testing.T) {
	e := replanEnv(t, testProject())
	name := e.reviewing()
	e.reviewOn(1, 801, "The version must come from the build info, not a constant.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	before := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if before.Status.Revisions != 1 || !e.gh.hasLabel("maul:approved") {
		t.Fatalf("revisions %d, approve label %v: want one round and the label left from the approval",
			before.Status.Revisions, e.gh.hasLabel("maul:approved"))
	}
	e.clock.Advance(time.Minute)
	id := e.gh.comment(approver, "/maul replan use the build info package")
	in := e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	checkReplanned(t, e, name, id, in)
	checkReplanInReviewContext(t, e)
	checkNewPlanBuilt(t, e, name)
}

// checkReplanned: the old pull request got one comment and was closed, the
// issue one reply and stays open, and nothing of review is left; the new
// revision's runs are attributed to the replan.
func checkReplanned(t *testing.T, e *env, name string, id int64, in *v1alpha1.Intent) {
	t.Helper()
	key := "comment-" + itoa(id)
	if got := e.gh.closedPRs; !slices.Equal(got, []int64{1}) {
		t.Errorf("closed pull requests = %v, want #1 once", got)
	}
	if got := e.prNotices(name, 1, templates.ReplanClosedKey(key)); len(got) != 1 ||
		!strings.Contains(got[0], "`"+legacyBranch(name)+"` is left as it is") {
		t.Errorf("pull request comments = %q, want one naming the branch left", got)
	}
	if got := e.notices(name, key); len(got) != 1 || !strings.Contains(got[0], "replaces plan r1") {
		t.Errorf("replies to the replan = %q, want one", got)
	}
	if e.gh.state() != "open" {
		t.Fatal("the issue was closed: maul's own close read as a human's")
	}
	st := in.Status
	if st.Input.Revision != 2 || st.PlanBranchesFrom != 2 || st.Replan != nil || len(st.PullRequests) != 0 ||
		st.Branch != "" || st.Revisions != 0 || st.RoundNoticesThrough != st.Rounds {
		t.Errorf("status after the replan = input r%d, branches from %d, replan %+v, %d pull requests, branch %q, "+
			"revisions %d, notices through %d of %d rounds", st.Input.Revision, st.PlanBranchesFrom, st.Replan,
			len(st.PullRequests), st.Branch, st.Revisions, st.RoundNoticesThrough, st.Rounds)
	}
	if tr := st.Input.Trigger; tr == nil || tr.Source != v1alpha1.IntentActionCommand || tr.EventID != id {
		t.Errorf("the new plan's runs are attributed to %+v, want the replan", tr)
	}
	if e.gh.hasLabel("maul:approved") {
		t.Error("the old approval's label is still on the issue with the new plan posted")
	}
}

// checkReplanInReviewContext: the new plan run is handed the old plan, the pull
// request's link and state, and the approver's review; the note is in its
// request.
func checkReplanInReviewContext(t *testing.T, e *env) {
	t.Helper()
	context := e.planContextOf(2)
	for _, want := range []string{"The previous plan (r1)", "acme/app#1", "closed",
		"The version must come from the build info, not a constant."} {
		if !strings.Contains(context, want) {
			t.Errorf("the planner's context does not hold %q:\n%s", want, context)
		}
	}
	if issue := e.specsOf("plan")[1].IssueMarkdown; !strings.Contains(issue, "use the build info package") {
		t.Errorf("the replan's note is not in the request:\n%s", issue)
	}
}

// checkNewPlanBuilt: the new plan builds only on a new approval, on a
// branch of its own, as a new pull request whose merge closes the issue.
func checkNewPlanBuilt(t *testing.T, e *env, name string) {
	t.Helper()
	e.settleActions(name)
	if got := e.get(name).Status.Phase; got != v1alpha1.IntentAwaitingApproval {
		t.Fatalf("phase %s with no new approval, want AwaitingApproval", got)
	}
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	branch := legacyBranch(name) + "-p2"
	if in.Status.Branch != branch || e.gh.branches[branch] == "" || len(in.Status.PullRequests) != 1 ||
		in.Status.PullRequests[0].Number == 1 || in.Status.Approval.PlanRevision != 2 {
		t.Fatalf("new build: branch %q (%q on GitHub), pull requests %+v, approval %+v", in.Status.Branch,
			e.gh.branches[branch], in.Status.PullRequests, in.Status.Approval)
	}
	e.gh.closePRn(in.Status.PullRequests[0].Number, true)
	e.drive(name, v1alpha1.IntentMerged, repoImage)
	if e.gh.state() != "closed" {
		t.Error("the new pull request's merge did not close the issue")
	}
}

// TestReplanInReviewByLabel: the trigger label re-applied in review is the
// same replan, answered with one reply naming the label.
func TestReplanInReviewByLabel(t *testing.T) {
	e := replanEnv(t, testProject())
	name := e.reviewing()
	e.clock.Advance(time.Minute)
	ev := e.gh.label(1, "maul:target", approver)
	in := e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	key := "event-" + itoa(ev)
	if got := e.notices(name, key); len(got) != 1 || !strings.Contains(got[0], "Adding the `maul:target` label") {
		t.Errorf("replies to the label = %q, want one", got)
	}
	if len(e.gh.closedPRs) != 1 || in.Status.Input.Trigger.Source != v1alpha1.IntentActionLabel ||
		in.Status.Input.Trigger.EventID != ev {
		t.Errorf("closed %v, trigger %+v; want #1 closed and the label the trigger", e.gh.closedPRs,
			in.Status.Input.Trigger)
	}
}

// TestReplanInReviewStopsTheRound: a replan made while a round runs is
// taken at once; the round pushes nothing, and its pull request is closed
// only once the round has settled.
func TestReplanInReviewStopsTheRound(t *testing.T) {
	e := replanEnv(t, testProject())
	name := e.reviewing()
	release := e.runningRound(name)
	commits, head := len(e.gh.commits), e.gh.branches[legacyBranch(name)]
	id := e.gh.comment(approver, "/maul replan")
	e.settleActions(name)
	if rp := e.get(name).Status.Replan; rp == nil || rp.Trigger.EventID != id {
		t.Fatalf("replan = %+v, want the command recorded while the round runs", rp)
	}
	release()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if run := e.reviseRun(name, 1); run.Status.Outcome != OutcomeAborted {
		t.Errorf("round = %s %q, want aborted", run.Status.Outcome, run.Status.Detail)
	}
	if len(e.gh.commits) != commits || e.gh.branches[legacyBranch(name)] != head {
		t.Error("the stopped round pushed")
	}
	if len(e.gh.closedPRs) != 1 {
		t.Errorf("closed %v, want #1", e.gh.closedPRs)
	}
}

// TestReplanInReviewFromBlocked: an intent blocked in review (here on its
// revision limit) replans; the block's condition is resolved.
func TestReplanInReviewFromBlocked(t *testing.T) {
	p := testProject()
	p.Spec.Limits.MaxRevisions = new(int32)
	e := replanEnv(t, p)
	name := e.reviewing()
	e.reviewOn(1, 801, "Rename it.")
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentBlocked, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul replan")
	in := e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionRevisionLimitReached) {
		t.Error("the revision limit still blocks the new plan")
	}
}

// TestReplanInReviewRefusals: each replan maul cannot take in review is
// answered once and consumed; nothing is closed and the intent stays.
func TestReplanInReviewRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env, name string)
		want  string
	}{
		{"at the cost ceiling", func(e *env, name string) {
			proj := e.getProject()
			proj.Spec.Limits.MaxCostMicroUSD = 1
			if err := e.c.Update(t.Context(), proj); err != nil {
				t.Fatal(err)
			}
		}, "at its cost ceiling"},
		{"no plan revision left", func(e *env, name string) {
			e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Input.Revision = v1alpha1.MaxIntentRound })
		}, "no plan revision left"},
		{"a pull request merged on GitHub", func(e *env, name string) {
			e.gh.mu.Lock()
			e.gh.prs[1].pr.Merged = true
			e.gh.mu.Unlock()
		}, "once a pull request of this intent has merged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := replanEnv(t, testProject())
			name := e.reviewing()
			tc.setup(e, name)
			e.clock.Advance(time.Minute)
			id := e.gh.comment(approver, "/maul replan")
			e.settleActions(name)
			e.settleActions(name)
			in := e.get(name)
			if got := e.notices(name, "comment-"+itoa(id)); len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Errorf("replies = %q, want one saying %q", got, tc.want)
			}
			if in.Status.Replan != nil || len(e.gh.closedPRs) != 0 || in.Status.Input.Trigger.EventID == id {
				t.Errorf("replan %+v, closed %v: the refused replan was taken", in.Status.Replan, e.gh.closedPRs)
			}
			if lt := in.Status.LastTrigger; lt == nil || lt.EventID != id {
				t.Errorf("last trigger = %+v, want the refused replan consumed", lt)
			}
		})
	}
}

// TestReplanInReviewSurvivesFailures (crash injection): a GitHub call
// failing at each step of the replan leaves it recorded and resumed by the
// next pass, which repeats only what is idempotent: one comment on the pull
// request, one close, one reply, the issue never closed, and the intent
// plans again.
func TestReplanInReviewSurvivesFailures(t *testing.T) {
	for _, step := range []string{"GetPullRequest", "CreatePullRequestComment", "ClosePullRequest",
		"CreateCommitStatus", "CreateIssueComment", "GetIssue"} {
		t.Run(step, func(t *testing.T) {
			e := replanEnv(t, testProject())
			name := e.reviewing()
			e.setStatus(name, func(in *v1alpha1.Intent) {
				// maul posted a maul/acceptance on the head: the replan
				// withdraws it.
				in.Status.PullRequests[0].AcceptanceStatusID = 77
				in.Status.PullRequests[0].AcceptanceHead = in.Status.PullRequests[0].HeadSHA
			})
			e.clock.Advance(time.Minute)
			id := e.gh.comment(approver, "/maul replan")
			e.settleActions(name)
			e.gh.failNext(step, errTransient)
			if step == "CreateIssueComment" {
				// The reply, not the plan comment, is the call to fail.
				e.gh.failNext(step, errTransient)
			}
			// The pass meeting the failure fails; the next ones resume.
			for range 6 {
				_ = e.reconcileIntent(name)
				e.runRuns()
				e.clock.Advance(time.Minute)
			}
			in := e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
			key := "comment-" + itoa(id)
			if len(e.notices(name, key)) != 1 || len(e.prNotices(name, 1, templates.ReplanClosedKey(key))) != 1 ||
				!slices.Equal(e.gh.closedPRs, []int64{1}) || e.gh.state() != "open" {
				t.Errorf("replies %d, pull request comments %d, closes %v, issue %s",
					len(e.notices(name, key)), len(e.prNotices(name, 1, templates.ReplanClosedKey(key))),
					e.gh.closedPRs, e.gh.state())
			}
			if in.Status.Replan != nil || in.Status.Input.Revision != 2 {
				t.Errorf("replan %+v, input r%d", in.Status.Replan, in.Status.Input.Revision)
			}
			got := e.acceptanceStatuses(e.gh.prs[1].pr.HeadSHA)
			superseded := 0
			for _, s := range got {
				if s.Description == "superseded by a replan" {
					superseded++
				}
			}
			if len(got) == 0 || got[0].State != "error" || got[0].Description != "superseded by a replan" ||
				superseded != 1 {
				t.Errorf("maul/acceptance at the old head = %+v, want one final error, the latest", got)
			}
		})
	}
}

// TestReplanInReviewAnswersOnce (critique 4): a label replan with a command
// after it in the same poll: the command is answered exactly once, after
// the replan, as the phase the intent is then in admits it; and the replan
// command's own reply is never posted twice, even as later polls list it.
func TestReplanInReviewAnswersOnce(t *testing.T) {
	e := replanEnv(t, testProject())
	name := e.reviewing()
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:target", approver)
	e.clock.Advance(time.Second)
	cmd := e.gh.comment(approver, "/maul approve")
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	for range 3 {
		e.settleActions(name)
		e.clock.Advance(time.Minute)
	}
	if got := e.notices(name, "comment-"+itoa(cmd)); len(got) != 1 {
		t.Errorf("replies to the /maul approve = %q, want one", got)
	}
	if e.get(name).Status.Phase != v1alpha1.IntentAwaitingApproval {
		t.Errorf("phase %s: a command made before the new plan was posted approved it", e.get(name).Status.Phase)
	}
}

// TestReplanInReviewFlagOffUnchanged: with the flag off the replan in
// review is answered as before, not available, and nothing else happens.
func TestReplanInReviewFlagOffUnchanged(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.reviewing()
	e.clock.Advance(time.Minute)
	id := e.gh.comment(approver, "/maul replan")
	e.settleActions(name)
	in := e.get(name)
	got := e.notices(name, "comment-"+itoa(id))
	if len(got) != 1 || !strings.Contains(got[0], "does nothing while this intent is `InReview`") ||
		strings.Contains(got[0], "replan`:") {
		t.Errorf("replies = %q, want today's not-available answer", got)
	}
	if in.Status.Replan != nil || in.Status.PlanBranchesFrom != 0 || len(e.gh.closedPRs) != 0 {
		t.Errorf("replan %+v, branches from %d, closed %v", in.Status.Replan, in.Status.PlanBranchesFrom,
			e.gh.closedPRs)
	}
}

// TestReplanOfferedInReview: with the flag on, the commands the issue
// admits in review (InReview, Revising, Blocked from either) include replan,
// before cancel; off, or in another phase, they are as before.
func TestReplanOfferedInReview(t *testing.T) {
	for _, on := range []bool{false, true} {
		e := newEnv(t, testProject())
		e.intent.Settings.ReplanInReview = on
		name := e.reviewing()
		p := e.prVerifyPass(name)
		for _, phase := range []v1alpha1.IntentPhase{v1alpha1.IntentInReview, v1alpha1.IntentRevising} {
			got := p.availableVerbs(phase)
			want := len(got) >= 2 && got[len(got)-2] == "replan" && got[len(got)-1] == "cancel"
			if want != on {
				t.Errorf("flag %v, %s: commands %v", on, phase, got)
			}
		}
		if got := p.availableVerbs(v1alpha1.IntentBuilding); slices.Contains(got, "replan") {
			t.Errorf("flag %v, Building: commands %v offer replan", on, got)
		}
		e.setStatus(name, func(in *v1alpha1.Intent) {
			if err := v1alpha1.SetIntentPhase(in, v1alpha1.IntentBlocked, e.clock.Now()); err != nil {
				t.Fatal(err)
			}
		})
		p = e.prVerifyPass(name)
		if got := p.availableVerbs(v1alpha1.IntentBlocked); slices.Contains(got, "replan") != on {
			t.Errorf("flag %v, Blocked from review: commands %v", on, got)
		}
	}
}
