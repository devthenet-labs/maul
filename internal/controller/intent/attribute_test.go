// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/attribution"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// reviewer is a second approver, so a review round can be told from the
// build's approver.
const reviewer = "carol"

// attributing turns --attribution-mode attribute on for every reconciler of
// the env.
func (e *env) attributing() {
	e.project.Settings.Attribution = attribution.ModeAttribute
	e.intent.Settings.Attribution = attribution.ModeAttribute
	e.runs.Settings.Attribution = attribution.ModeAttribute
}

// discovered is the Intent discovery makes for issue 1, triggered by
// requester: unlike newIntent, through the issue events, so its requestedBy
// carries the applier's GitHub id.
func (e *env) discovered(requester string) string {
	e.t.Helper()
	e.gh.openIssue(1, "Add a version endpoint", "Please add GET /version.", requester)
	e.reconcileProject()
	e.clock.Advance(time.Second)
	return v1alpha1.IntentName("target", 1)
}

func person(login string, reason v1alpha1.AttributionReason) *v1alpha1.Attribution {
	return &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: login, ID: actorOf(login).ID}, Reason: reason}
}

func annotationOf(login string) string {
	return "github-id:" + strconv.FormatInt(actorOf(login).ID, 10)
}

// launchedAttribution is what every Job launched for phase was attributed
// to, in launch order.
func (e *env) launchedAttribution(phase string) []string {
	var out []string
	for _, s := range e.jobs.launched() {
		if s.Phase == phase {
			out = append(out, s.AttributedTo)
		}
	}
	return out
}

// TestAttributionTable is the design §2 table for intents under mode
// attribute: the plan is the approver who triggered it (requester), the
// build is the plan's approver, a review round the newest reviewer it
// consumed, and every Job carries its own run's stamp.
func TestAttributionTable(t *testing.T) {
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
	e := newEnv(t, p)
	e.gh.perms[reviewer] = ghclient.PermissionWrite
	e.attributing()
	name := e.discovered(approver)
	if got := e.get(name).Spec.RequestedBy.UserID; got != actorOf(approver).ID {
		t.Fatalf("requestedBy.userID = %d, want the applier's", got)
	}
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plan := e.runsOf(name, v1alpha1.IntentStagePlan)
	if len(plan) != 1 || !reflect.DeepEqual(plan[0].Spec.AttributedTo, person(approver, v1alpha1.AttributionRequester)) {
		t.Fatalf("plan run attributed to %+v, want the requester", plan[0].Spec.AttributedTo)
	}
	if got := e.launchedAttribution("plan"); !reflect.DeepEqual(got, []string{annotationOf(approver)}) {
		t.Errorf("plan Jobs attributed to %v, want the requester's id", got)
	}

	// The reviewer approves the plan: the build is theirs, not the
	// requester's.
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", reviewer)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.Approval == nil || in.Status.Approval.ByID != actorOf(reviewer).ID {
		t.Fatalf("approval = %+v, want the approver's GitHub id recorded", in.Status.Approval)
	}
	build := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(build) != 1 || !reflect.DeepEqual(build[0].Spec.AttributedTo, person(reviewer, v1alpha1.AttributionApprover)) {
		t.Fatalf("build run attributed to %+v, want the approver", build[0].Spec.AttributedTo)
	}
	if got := e.launchedAttribution("build"); !reflect.DeepEqual(got, []string{annotationOf(reviewer)}) {
		t.Errorf("build Jobs attributed to %v, want the approver's id", got)
	}

	// Two approvers request changes: the round is the newest reviewer's.
	pr := in.Status.PullRequests[0].Number
	now := e.clock.Now()
	e.gh.reviews[pr] = []ghclient.Review{
		{ID: 501, NodeID: "review-501", Author: actorOf(reviewer), State: "CHANGES_REQUESTED",
			Body: "Add a test.", SubmittedAt: now},
		{ID: 502, NodeID: "review-502", Author: actorOf(approver), State: "CHANGES_REQUESTED",
			Body: "And a header.", SubmittedAt: now.Add(time.Second)},
	}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	revise := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(revise) != 1 || !reflect.DeepEqual(revise[0].Spec.AttributedTo, person(approver, v1alpha1.AttributionReviewer)) {
		t.Fatalf("review round attributed to %+v, want the newest reviewer", revise[0].Spec.AttributedTo)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if got := e.launchedAttribution("build"); len(got) != 2 || got[1] != annotationOf(approver) {
		t.Errorf("build-phase Jobs attributed to %v, want the review round's the reviewer's", got)
	}
}

// TestAttributionCommandAndChecks: a /patchy revise round is its
// commenter's, and a check-fix round the build's approver's.
func TestAttributionCommandAndChecks(t *testing.T) {
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
	p.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, p)
	e.gh.perms[reviewer] = ghclient.PermissionWrite
	e.attributing()
	name := e.discovered(approver)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)

	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		Output: ghclient.CheckOutput{Title: "Go failed", Summary: "failure"}}}
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	checks := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(checks) != 1 || checks[0].Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		!reflect.DeepEqual(checks[0].Spec.AttributedTo, person(approver, v1alpha1.AttributionApprover)) {
		t.Fatalf("check-fix round = %+v, want the build's approver", checks)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.checks[in.Status.PullRequests[0].HeadSHA] = nil

	pr := in.Status.PullRequests[0]
	e.gh.prComments[pr.Number] = []*ghclient.Comment{{ID: 940, NodeID: "comment-940",
		UserLogin: reviewer, UserID: actorOf(reviewer).ID, UserType: "User",
		Body:      "/patchy revise Please check the response header.",
		CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
	e.clock.Advance(time.Second)
	e.driveUntil(name, func(in *v1alpha1.Intent) bool {
		return len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 2
	})
	command := e.runsOf(name, v1alpha1.IntentStageRevise)[1]
	if command.Spec.Trigger != v1alpha1.IntentRunTriggerCommand ||
		!reflect.DeepEqual(command.Spec.AttributedTo, person(reviewer, v1alpha1.AttributionCommenter)) {
		t.Errorf("command round = %+v, want the commenter's", command.Spec)
	}
}

// TestAttributionUnregisteredAndOff: an approver with no GitHub id on
// record (an Intent from before ids were kept) makes the run the org's,
// reason approver-unregistered, its Job "org"; under mode off no run is
// stamped and no Job annotated.
func TestAttributionUnregisteredAndOff(t *testing.T) {
	e := newEnv(t, testProject())
	e.attributing()
	name := e.newIntent(approver) // requestedBy without a userID
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	want := &v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}
	if got := e.runsOf(name, v1alpha1.IntentStagePlan)[0].Spec.AttributedTo; !reflect.DeepEqual(got, want) {
		t.Errorf("plan by an unregistered requester attributed to %+v, want %+v", got, want)
	}
	if got := e.launchedAttribution("plan"); !reflect.DeepEqual(got, []string{"org"}) {
		t.Errorf("plan Jobs attributed to %v, want org", got)
	}

	off := newEnv(t, testProject())
	name = off.discovered(approver)
	off.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	off.clock.Advance(time.Minute)
	off.gh.label(1, "patchy:approved", approver)
	off.drive(name, v1alpha1.IntentInReview, repoImage)
	for _, run := range off.intentRuns(name) {
		if run.Spec.AttributedTo != nil {
			t.Errorf("run %s attributed under mode off: %+v", run.Name, run.Spec.AttributedTo)
		}
	}
	for _, s := range off.jobs.launched() {
		if s.AttributedTo != "" {
			t.Errorf("a %s Job attributed under mode off: %q", s.Phase, s.AttributedTo)
		}
	}
}

// TestAttributePlanByNonApprover: a plan whose newest trigger came from
// someone who is not (or no longer) an approver is the org's.
func TestAttributePlanByNonApprover(t *testing.T) {
	proj := testProject()
	p := &pass{proj: proj, in: &v1alpha1.Intent{Spec: v1alpha1.IntentSpec{
		RequestedBy: v1alpha1.IntentRequest{Login: "mallory", UserID: 9, EventID: 1},
	}}}
	if got := p.attributeRun(v1alpha1.IntentStagePlan); !reflect.DeepEqual(got, attribution.Org()) {
		t.Errorf("plan by a non-approver attributed to %+v, want the org", got)
	}
	p.in.Status.LastTrigger = &v1alpha1.IntentAction{Source: v1alpha1.IntentActionCommand, EventID: 5,
		Login: approver, UserID: 11}
	if got := p.attributeRun(v1alpha1.IntentStagePlan); !reflect.DeepEqual(got, &v1alpha1.Attribution{
		User: &v1alpha1.UserRef{Login: approver, ID: 11}, Reason: v1alpha1.AttributionRequester}) {
		t.Errorf("a replan by an approver attributed to %+v, want them", got)
	}
	if got := p.attributeRun(v1alpha1.IntentStageBuild); !reflect.DeepEqual(got, attribution.Org()) {
		t.Errorf("a build with no approval attributed to %+v, want the org", got)
	}
}
