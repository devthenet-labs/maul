// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// The outcome twins of verify_round_test.go (S4 PR 1): the acceptance-fix
// round's guards and the human's precedence over it, for a failure the
// agent evaluator reproduced. Each test is named after the criteria test it
// twins; the helpers are in verify_outcomes_test.go.

// TestOutcomeFixGuardsHalt twins TestVerifyFixGuardsHalt.
func TestOutcomeFixGuardsHalt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project func(*v1alpha1.Project)
		reason  v1alpha1.AutoFixReason
		state   v1alpha1.IntentVerificationState
	}{
		{"max verify fixes", func(p *v1alpha1.Project) { p.Spec.Limits.MaxVerifyFixes = new(int32(0)) },
			v1alpha1.AutoFixMaxVerifyFixes, v1alpha1.VerificationExhausted},
		{"cost ceiling", func(p *v1alpha1.Project) { p.Spec.Limits.MaxCostMicroUSD = 1 },
			v1alpha1.AutoFixCostCeiling, v1alpha1.VerificationFailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name, _ := failingEvaluation(t)
			proj := e.getProject()
			tc.project(proj)
			if err := e.c.Update(context.Background(), proj); err != nil {
				t.Fatal(err)
			}
			e.review(name, 2)
			in := e.get(name)
			v := in.Status.Verification
			if in.Status.Phase != v1alpha1.IntentInReview || len(e.verifyRuns(name)) != 0 ||
				v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != tc.reason || v.State != tc.state {
				t.Fatalf("phase %s, verify runs %d, verification %+v", in.Status.Phase, len(e.verifyRuns(name)), v)
			}
		})
	}
}

// TestOutcomeFixHumanPushHalts twins TestVerifyFixHumanPushHalts.
func TestOutcomeFixHumanPushHalts(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.pushHead(e.prNumber(name), strings.Repeat("e", 40))
	e.review(name, 3)
	v := e.verification(name)
	if len(e.verifyRuns(name)) != 0 || v.AutoFix.Reason != v1alpha1.AutoFixHumanPush {
		t.Fatalf("verify runs %d, verification %+v", len(e.verifyRuns(name)), v)
	}
}

// TestOutcomeHumanWinsOverAVerifyFix twins TestHumanWinsOverAVerifyFix: a
// review due on the pull request when a round is judged failing: the
// human's round starts, and automatic fixes halt.
func TestOutcomeHumanWinsOverAVerifyFix(t *testing.T) {
	e, name, version, health := outcomeEnv(t)
	e.reviewOn(e.prNumber(name), 1501, "Return JSON, please.")
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	e.review(name, 3)
	in := e.get(name)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if in.Status.Phase != v1alpha1.IntentRevising || len(runs) != 1 ||
		runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("phase %s, runs %+v; want the review's round", in.Status.Phase, runs)
	}
	if a := in.Status.Verification.AutoFix; a.State != v1alpha1.AutoFixHalted || a.Reason != v1alpha1.AutoFixHumanReview {
		t.Fatalf("auto-fix = %+v", a)
	}
}

// TestOutcomeHumanPendingHoldsAVerifyFix twins TestHumanPendingHoldsAVerifyFix.
func TestOutcomeHumanPendingHoldsAVerifyFix(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.reviewOn(e.prNumber(name), 1601, "Hold on, the route is wrong.")
	e.review(name, 1)
	if in := e.get(name); in.Status.Phase != v1alpha1.IntentInReview ||
		len(e.runsOf(name, v1alpha1.IntentStageRevise)) != 0 {
		t.Fatalf("phase %s, runs %d; want nothing started inside the quiet window", in.Status.Phase,
			len(e.runsOf(name, v1alpha1.IntentStageRevise)))
	}
	e.review(name, 2)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("runs = %+v, want the review's round", runs)
	}
}

// TestOutcomeHumanCancelsAPendingVerifyFix twins
// TestHumanCancelsAPendingVerifyFix.
func TestOutcomeHumanCancelsAPendingVerifyFix(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 1)
	runs := e.verifyRuns(name)
	if len(runs) != 1 || e.get(name).Status.Phase != v1alpha1.IntentRevising {
		t.Fatalf("verify runs %d, want one leased", len(runs))
	}
	jobsBefore := len(e.jobs.order)
	n := e.prNumber(name)
	e.reviewOn(n, 1701, "Not like that.")
	e.review(name, 1)
	v := e.verification(name)
	if v.CancelRun != runs[0].Name || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixHumanReview {
		t.Fatalf("verification = %+v, want the run cancelled", v)
	}
	e.readyRepositories(repoImage)
	e.runRuns()
	run := e.verifyRuns(name)[0]
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeSupersededByHuman ||
		run.Status.JobRef != nil || len(e.jobs.order) != jobsBefore {
		t.Fatalf("verify run = %s %s job %+v, jobs %v (were %d)", run.Status.Phase, run.Status.Outcome,
			run.Status.JobRef, e.jobs.order, jobsBefore)
	}
	e.review(name, 1)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.VerifyFixes != 0 || in.Status.Revisions != 0 ||
		in.Status.Verification.CancelRun != "" || len(e.verifyRuns(name)) != 1 {
		t.Fatalf("after the cancel: phase %s, fixes %d, revisions %d, verification %+v, verify runs %d",
			in.Status.Phase, in.Status.VerifyFixes, in.Status.Revisions, in.Status.Verification,
			len(e.verifyRuns(name)))
	}
	if !strings.Contains(e.everyBody(), "cancelled for your review") {
		t.Error("the pull request was not told the automatic fix was cancelled")
	}
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	if r := e.reviseRun(name, in.Status.Rounds); r == nil || r.Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("round %d = %+v, want the review's", in.Status.Rounds, r)
	}
}

// TestOutcomeHumanWaitsForARunningVerifyFix twins
// TestHumanWaitsForARunningVerifyFix.
func TestOutcomeHumanWaitsForARunningVerifyFix(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 1)
	fix := e.verifyRuns(name)[0]
	hold := true
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		if hold && spec.Finding == fix.Name {
			return jobs.Status{}, true
		}
		return jobs.Status{}, false
	}
	for range 4 {
		e.mustIntent(name)
		e.readyRepositories(repoImage)
		e.runRuns()
	}
	if r := e.verifyRuns(name)[0]; r.Status.JobRef == nil {
		t.Fatalf("the verify fix did not launch: %s %s %q", r.Status.Phase, r.Status.Outcome, r.Status.Detail)
	}
	e.reviewOn(e.prNumber(name), 1801, "Also rename the handler.")
	e.review(name, 1)
	v := e.verification(name)
	if v.CancelRun != "" || v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixHumanReview {
		t.Fatalf("verification = %+v, want halted and nothing cancelled", v)
	}
	hold = false
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	fix = e.verifyRuns(name)[0]
	if fix.Status.Phase != v1alpha1.RunComplete || fix.Status.PushedCommit == "" || in.Status.VerifyFixes != 1 {
		t.Fatalf("the running fix did not push: %s %s", fix.Status.Phase, fix.Status.Outcome)
	}
	e.clock.Advance(2 * time.Minute)
	in = e.drive(name, v1alpha1.IntentRevising, repoImage)
	human := e.reviseRun(name, in.Status.Rounds)
	if human == nil || human.Spec.Trigger != v1alpha1.IntentRunTriggerReview {
		t.Fatalf("round %d = %+v, want the review's", in.Status.Rounds, human)
	}
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	handoff := e.runInput(*e.reviseRun(name, human.Spec.Round)).Data[keyInvestigation]
	if !strings.Contains(handoff, "An automatic acceptance fix landed after your review") ||
		!strings.Contains(handoff, fix.Status.PushedCommit) {
		t.Errorf("the human's round was not told of the fix:\n%s", handoff)
	}
}

// TestOutcomeVerifyCommandOnAPullRequestIsSettled twins
// TestVerifyCommandOnAPullRequestIsSettled.
func TestOutcomeVerifyCommandOnAPullRequestIsSettled(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.review(name, 2)
	n := e.prNumber(name)
	id := e.prCommand(n, approver, "/maul verify")
	e.review(name, 1)
	if r := e.verification(name).Requested; r == nil || r.CommentID != id {
		t.Fatalf("requested = %+v", r)
	}
	if !strings.Contains(e.everyBody(), "comment-"+itoa(id)) {
		t.Error("the command was not answered on the pull request")
	}
}

// TestOutcomeVerifyLaunchRefusal twins TestVerifyLaunchRefusal over an
// outcomes acceptance record: launched at its plan revision, aborted at
// another, superseded or stopped when cancelled for a human.
func TestOutcomeVerifyLaunchRefusal(t *testing.T) {
	proj := testProject()
	proj.Spec.Preview = &appPreview
	run := reviseRunOf(v1alpha1.IntentRunTriggerVerify)
	run.Name = "fix-1"
	run.Spec.Repository.URL = appRepoURL
	in := func(mutate func(*v1alpha1.Intent)) *v1alpha1.Intent {
		i := &v1alpha1.Intent{Status: v1alpha1.IntentStatus{
			Acceptance: &v1alpha1.IntentAcceptance{PlanRevision: 1, OutcomesDigest: "sha256:o",
				Outcomes: []v1alpha1.AcceptanceOutcome{{ID: "version-shown", Outcome: "the version is shown"}}},
			Verification: &v1alpha1.IntentVerification{}}}
		if mutate != nil {
			mutate(i)
		}
		return i
	}
	for _, tc := range []struct {
		name string
		in   *v1alpha1.Intent
		want string
	}{
		{"launch", in(nil), ""},
		{"cancelled", in(func(i *v1alpha1.Intent) { i.Status.Verification.CancelRun = "fix-1" }),
			OutcomeSupersededByHuman},
		{"stopped", in(func(i *v1alpha1.Intent) {
			i.Status.Verification.CancelRun = "fix-1"
			i.Status.Verification.AutoFix.Reason = v1alpha1.AutoFixHumanStop
		}), OutcomeStoppedByHuman},
		{"other plan revision", in(func(i *v1alpha1.Intent) { i.Status.Acceptance.PlanRevision = 2 }),
			OutcomeAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if res := verifyRunRefusal(tc.in, proj, run); res != nil {
				got = res.outcome
			}
			if got != tc.want {
				t.Errorf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOutcomeVerifyWorkPending twins TestVerifyWorkPending over the
// evaluator's states: a round in flight or judged unconfirmed starts
// nothing; a reproduced failure with fixes armed, or a request, may.
func TestOutcomeVerifyWorkPending(t *testing.T) {
	ref := &v1alpha1.ObjectReference{Name: "v", UID: "u"}
	armed, halted := v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed}, v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted}
	for _, tc := range []struct {
		name string
		v    *v1alpha1.IntentVerification
		want bool
	}{
		{"evaluating", &v1alpha1.IntentVerification{State: v1alpha1.VerificationEvaluating, Ref: ref, AutoFix: armed},
			false},
		{"unconfirmed", &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed, Ref: ref,
			AutoFix: armed}, false},
		{"unconfirmed, a request", &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed, Ref: ref,
			Requested: &v1alpha1.VerifyRequest{CommentID: 1}}, true},
		{"failing, armed", &v1alpha1.IntentVerification{State: v1alpha1.VerificationFailing, Ref: ref,
			AutoFix: armed}, true},
		{"failing, halted", &v1alpha1.IntentVerification{State: v1alpha1.VerificationFailing, Ref: ref,
			AutoFix: halted}, false},
	} {
		if got := verifyWorkPending(tc.v); got != tc.want {
			t.Errorf("%s: verifyWorkPending = %v, want %v", tc.name, got, tc.want)
		}
	}
}
