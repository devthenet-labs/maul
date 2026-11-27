// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
)

// replanArgs turn replans in review on.
var replanArgs = []string{"--intent-replan-in-review"}

// approvePlan approves the intent's current plan by the approve label, once
// GitHub dates the label after the plan's posting, and waits for review.
func (e *intentEnv) approvePlan(t *testing.T, number int, name string, revision int32) *v1alpha1.Intent {
	t.Helper()
	in := e.waitPlan(t, name, revision)
	afterSecond(in.Status.Plan.PostedAt.Time)
	e.gh.LabelIssue(number, approveLabel, approver)
	return e.waitPhase(t, name, v1alpha1.IntentInReview)
}

// waitPlan waits for plan revision revision to wait for approval.
func (e *intentEnv) waitPlan(t *testing.T, name string, revision int32) *v1alpha1.Intent {
	t.Helper()
	var in v1alpha1.Intent
	eventually(t, "the plan to wait for approval", func() bool {
		if err := e.cl.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name},
			&in); err != nil {
			return false
		}
		return in.Status.Phase == v1alpha1.IntentAwaitingApproval && in.Status.Plan != nil &&
			in.Status.Plan.Revision == revision && in.Status.Plan.PostedAt != nil
	})
	return &in
}

// ownOn are maul's comments on pull request (or issue) number headed by
// marker.
func (e *intentEnv) ownOn(number int, marker string) []fakegithub.Comment {
	return e.own(number, marker)
}

// TestIntentReplanInReview drives replans in review through the shipped
// binaries: the pull request open, an approver's comment on it, then
// /maul replan with a note. The old pull request is closed with one
// comment, the issue gets one reply, and the plan Job of the next revision
// is handed the old plan, the pull request's link and the review comment,
// with the note in its request. The new plan's approval builds a new pull
// request on maul-intent/<intent>-p2. Then the trigger label re-applied
// replans again, onto -p3, and that pull request's merge closes the issue.
func TestIntentReplanInReview(t *testing.T) {
	e := startIntents(t, replanArgs...)
	number, name := e.fileIntent(t)
	in := e.approvePlan(t, number, name, 1)
	first := in.Status.PullRequests[0]
	e.gh.CommentAs(int(first.Number), "The VERSION file must hold the semver, not a date.", approver)

	afterSecond(in.Status.PhaseTimes[len(in.Status.PhaseTimes)-1].At.Time)
	cmd := e.gh.CommentAs(number, "/maul replan read the version from the module instead", approver)
	key := "comment-" + strconv.FormatInt(cmd, 10)
	e.waitPlan(t, name, 2)
	checkClosedByReplan(t, e, name, first, key, "maul-intent/"+name)
	if got := e.own(number, notice(name, key)); len(got) != 1 || !strings.Contains(got[0].Body, "replaces plan r1") {
		t.Errorf("replies to the replan = %+v, want one", got)
	}
	plan2 := jobs.NameFor(v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 2, "", 1), intentRunKind, 1)
	job := e.kubelet.waitRun(t, "the second plan job to run", func(r agentRun) bool { return r.Job.Name == plan2 })
	for _, want := range []string{"The previous plan (r1)", first.URL, "closed",
		"The VERSION file must hold the semver, not a date."} {
		if !strings.Contains(string(job.Investigation), want) {
			t.Errorf("the replan's plan context does not hold %q:\n%s", want, job.Investigation)
		}
	}
	if !strings.Contains(string(job.Issue), "read the version from the module instead") {
		t.Errorf("the replan's note is not in the plan's request:\n%s", job.Issue)
	}

	in = e.approvePlan(t, number, name, 2)
	second := in.Status.PullRequests[0]
	checkNewBranch(t, e, in, second, first, "maul-intent/"+name+"-p2")

	// The trigger label re-applied is a replan too.
	afterSecond(in.Status.PhaseTimes[len(in.Status.PhaseTimes)-1].At.Time)
	e.gh.UnlabelIssue(number, triggerLabel, approver)
	e.gh.LabelIssue(number, triggerLabel, approver)
	label := e.lastLabeled(t, number, triggerLabel)
	e.waitPlan(t, name, 3)
	checkClosedByReplan(t, e, name, second, "event-"+strconv.FormatInt(label.ID, 10), "maul-intent/"+name+"-p2")
	in = e.approvePlan(t, number, name, 3)
	third := in.Status.PullRequests[0]
	checkNewBranch(t, e, in, third, second, "maul-intent/"+name+"-p3")

	e.gh.MergePull(int(third.Number), "maul-intent/"+name+"-p3", "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	e.waitPhase(t, name, v1alpha1.IntentMerged)
	if issue := e.issue(t, number); issue.State != "closed" || issue.StateReason != "completed" {
		t.Errorf("intent issue = %s (%s), want closed as completed", issue.State, issue.StateReason)
	}
	for _, pr := range []v1alpha1.IntentPullRequest{first, second} {
		if got, _ := e.gh.Pull(int(pr.Number)); got.State != "closed" || got.Merged {
			t.Errorf("replaced pull request #%d = %s merged %v, want closed unmerged", pr.Number, got.State,
				got.Merged)
		}
	}
}

// checkClosedByReplan: pr is closed unmerged with one comment of the replan
// keyed key, naming its branch left as it is.
func checkClosedByReplan(t *testing.T, e *intentEnv, name string, pr v1alpha1.IntentPullRequest, key,
	branch string) {
	t.Helper()
	eventually(t, "the replaced pull request to be closed", func() bool {
		got, ok := e.gh.Pull(int(pr.Number))
		return ok && got.State == "closed"
	})
	if got, _ := e.gh.Pull(int(pr.Number)); got.Merged {
		t.Errorf("pull request #%d merged, want closed unmerged", pr.Number)
	}
	comments := e.ownOn(int(pr.Number), notice(name, templates.ReplanClosedKey(key)))
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "`"+branch+"` is left as it is") {
		t.Errorf("pull request #%d's replan comments = %+v, want one naming %s", pr.Number, comments, branch)
	}
	if e.gh.RepoBranchHead(intentOwner, appRepo, branch) == "" {
		t.Errorf("the branch %s was deleted, want it left", branch)
	}
}

// checkNewBranch: the new plan's pull request is a new one, opened from its
// own branch.
func checkNewBranch(t *testing.T, e *intentEnv, in *v1alpha1.Intent, pr, old v1alpha1.IntentPullRequest,
	branch string) {
	t.Helper()
	if pr.Number == old.Number || in.Status.Branch != branch {
		t.Fatalf("new pull request #%d (old #%d), branch %q, want a new one on %s", pr.Number, old.Number,
			in.Status.Branch, branch)
	}
	if got, ok := e.gh.Pull(int(pr.Number)); !ok || got.Head != branch || got.State != "open" {
		t.Errorf("new pull request = %+v, want open from %s", got, branch)
	}
}
