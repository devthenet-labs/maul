// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/jobs"
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

// discovered is the Intent discovery makes for issue 1, triggered by the
// approver: unlike newIntent, through the issue events, so its requestedBy
// carries the applier's GitHub id.
func (e *env) discovered() string {
	e.t.Helper()
	e.gh.openIssue(1, "Add a version endpoint", "Please add GET /version.", approver)
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
	for _, s := range e.jobs.launchedInOrder() {
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
	name := e.discovered()
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
	e.gh.label(1, "maul:approved", reviewer)
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
	e.mustIntent(name)
	if body := e.statusBody(); !strings.Contains(body, "**Attributed to:** `@carol` (`approver`), for the latest run") {
		t.Errorf("the status comment does not name the build's approver:\n%s", body)
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
	if want := person(approver, v1alpha1.AttributionReviewer); len(revise) != 1 ||
		!reflect.DeepEqual(revise[0].Spec.AttributedTo, want) {
		t.Fatalf("review round attributed to %+v, want the newest reviewer", revise[0].Spec.AttributedTo)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	if got := e.launchedAttribution("build"); len(got) != 2 || got[1] != annotationOf(approver) {
		t.Errorf("build-phase Jobs attributed to %v, want the review round's the reviewer's", got)
	}
}

// TestAttributionCommandAndChecks: a /maul revise round is its
// commenter's, and a check-fix round the build's approver's.
func TestAttributionCommandAndChecks(t *testing.T) {
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
	p.Spec.Checks.Fix = []string{"test"}
	e := newEnv(t, p)
	e.gh.perms[reviewer] = ghclient.PermissionWrite
	e.attributing()
	name := e.discovered()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "maul:approved", approver)
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
		Body:      "/maul revise Please check the response header.",
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
	name = off.discovered()
	off.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	off.clock.Advance(time.Minute)
	off.gh.label(1, "maul:approved", approver)
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
	if body := off.statusBody(); strings.Contains(body, "Attributed to") {
		t.Errorf("the status comment names an attribution under mode off:\n%s", body)
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

// TestAttributePlanRetryKeepsItsTrigger is the regression for a plan retry
// attributed to whichever trigger action was consumed last: an action
// consumed while the round's first attempt runs (a replan refused for its
// actor, or one an approver makes while the intent is not awaiting approval)
// starts no plan, so the round's next attempt is still the requester's.
func TestAttributePlanRetryKeepsItsTrigger(t *testing.T) {
	for _, tc := range []struct {
		name, by string
		label    bool // the trigger label re-applied, else /maul replan
	}{
		{"a trigger label refused for a non-approver", "mallory", true},
		{"a replan unavailable to another approver", reviewer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProject()
			p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
			e := newEnv(t, p)
			e.gh.perms[reviewer] = ghclient.PermissionWrite
			e.attributing()
			name := e.discovered()
			first := v1alpha1.IntentRunName(name, v1alpha1.IntentStagePlan, 1, "", 1)
			job := jobs.NameFor(first, KindIntent, 1)
			e.jobs.status[job] = jobs.Status{} // held running
			e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
				if spec.Phase == "plan" && spec.Finding == first {
					return planOutput(envelope.Stage{Outcome: envelope.OutcomeRuntimeError,
						Detail: "the CLI crashed"}, "")
				}
				return defaultOutput(spec)
			}
			e.passUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStagePlan)) == 1 })

			e.clock.Advance(time.Minute)
			if tc.label {
				e.gh.removeTrigger()
				e.gh.label(1, "maul:target", tc.by)
			} else {
				e.gh.comment(tc.by, "/maul replan")
			}
			e.settleActions(name)
			in := e.get(name)
			if in.Status.Phase != v1alpha1.IntentPlanning || in.Status.LastTrigger == nil ||
				in.Status.LastTrigger.Login != tc.by {
				t.Fatalf("phase %s lastTrigger %+v, want Planning with the replan consumed", in.Status.Phase,
					in.Status.LastTrigger)
			}

			e.jobs.mu.Lock()
			delete(e.jobs.status, job)
			e.jobs.mu.Unlock()
			e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
			runs := e.runsOf(name, v1alpha1.IntentStagePlan)
			if len(runs) != 2 {
				t.Fatalf("plan runs = %d, want the failed attempt and its retry", len(runs))
			}
			want := person(approver, v1alpha1.AttributionRequester)
			for _, run := range runs {
				if !reflect.DeepEqual(run.Spec.AttributedTo, want) {
					t.Errorf("plan run %s attributed to %+v, want the requester %+v", run.Name,
						run.Spec.AttributedTo, want)
				}
			}
		})
	}
}

// TestReviseLeaseAdoptedWhateverItsStamp: a revise round's lease created by
// a pass under another attribution (the mode changed between the create and
// the pass that finds it, or the stamp's person did) is the same round, so
// it is adopted with its own stamp, never refused as another's.
func TestReviseLeaseAdoptedWhateverItsStamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp *v1alpha1.Attribution // the existing lease's
	}{
		{"leased before attribution was on", nil},
		{"leased under another attribution", attribution.Org()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			e.attributing()
			name := e.discovered()
			e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
			e.clock.Advance(time.Minute)
			e.gh.label(1, "maul:approved", approver)
			in := e.drive(name, v1alpha1.IntentInReview, repoImage)

			wc, ok := e.c.(client.WithWatch)
			if !ok {
				t.Fatal("the env's client does not watch")
			}
			leased := false
			c := interceptor.NewClient(wc, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.CreateOption) error {
					run, ok := obj.(*v1alpha1.IntentRun)
					if !ok || run.Spec.Stage != v1alpha1.IntentStageRevise || leased {
						return c.Create(ctx, obj, opts...)
					}
					leased = true
					other := run.DeepCopy()
					other.Spec.AttributedTo = tc.stamp.DeepCopy()
					if err := c.Create(ctx, other, opts...); err != nil {
						return err
					}
					return kerrors.NewAlreadyExists(schema.GroupResource{Resource: "intentruns"}, run.Name)
				},
			})
			e.intent.Client = c

			number := in.Status.PullRequests[0].Number
			e.gh.reviews[number] = []ghclient.Review{{ID: 901, NodeID: "review-901", Author: actorOf(approver),
				State: "CHANGES_REQUESTED", Body: "Please add a test.", SubmittedAt: e.clock.Now()}}
			e.clock.Advance(3 * time.Minute)
			e.drive(name, v1alpha1.IntentRevising, repoImage)
			e.drive(name, v1alpha1.IntentInReview, repoImage)
			runs := e.runsOf(name, v1alpha1.IntentStageRevise)
			if !leased || len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunComplete {
				t.Fatalf("leased %v, revise runs %+v; want the one existing lease adopted and run", leased, runs)
			}
			if !reflect.DeepEqual(runs[0].Spec.AttributedTo, tc.stamp) {
				t.Errorf("the adopted round is attributed to %+v, want its own stamp %+v",
					runs[0].Spec.AttributedTo, tc.stamp)
			}
		})
	}
}
