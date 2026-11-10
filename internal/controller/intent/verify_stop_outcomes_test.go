// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// The outcome twins of verify_stop_test.go (S4): /maul verify stop on an
// intent whose approved plan states outcomes, under the agent evaluator.
// Each test is named after the criteria test it twins.

// armed sets intent name's automatic fixes armed.
func (e *env) armed(name string) {
	e.t.Helper()
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed}
	})
}

// TestOutcomeVerifyStopHalts twins TestVerifyStopHalts.
func TestOutcomeVerifyStopHalts(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.armed(name)
	id := e.gh.comment(approver, "/maul verify stop")
	e.settleActions(name)
	v := e.verification(name)
	a := v.AutoFix
	if a.State != v1alpha1.AutoFixHalted || a.Reason != v1alpha1.AutoFixHumanStop || a.Since == nil ||
		a.Stop == nil || a.Stop.By != approver || a.Stop.ByID != actorOf(approver).ID || a.Stop.CommentID != id ||
		a.Stop.At.IsZero() {
		t.Fatalf("auto-fix = %+v (stop %+v), want halted by the approver's stop", a, a.Stop)
	}
	if v.Requested != nil || v.Reset.Generation != 0 || v.CancelRun != "" {
		t.Fatalf("stop started something: requested %+v reset %+v cancel %q", v.Requested, v.Reset, v.CancelRun)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "`/maul verify stop`") ||
		!strings.Contains(body, "no longer fixes") || !strings.Contains(body, "`/maul verify fix`") {
		t.Errorf("reply = %s", body)
	}
	if in := e.get(name); in.Status.Commands == nil || !containsID(in.Status.Commands.Applied, id) {
		t.Errorf("the stop is not recorded as applied: %+v", in.Status.Commands)
	}
	e.checkRearmedAfterStop(name, id)
}

// TestOutcomeVerifyStopReplyAfterCrash twins TestVerifyStopReplyAfterCrash.
func TestOutcomeVerifyStopReplyAfterCrash(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.armed(name)
	id := e.gh.comment(approver, "/maul verify stop")
	e.gh.failNext("CreateIssueComment", errTransient)
	if err := e.reconcileIntent(name); err == nil {
		t.Fatal("the failed reply returned no error")
	}
	since := e.verification(name).AutoFix.Since
	e.clock.Advance(time.Minute)
	e.settleActions(name)
	a := e.verification(name).AutoFix
	if a.Reason != v1alpha1.AutoFixHumanStop || !a.Since.Equal(since) {
		t.Fatalf("auto-fix = %+v, want the first stop's, unchanged", a)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "no longer fixes") {
		t.Errorf("reply = %s", body)
	}
}

// TestOutcomeVerifyStopCancelsAPendingFix twins
// TestVerifyStopCancelsAPendingFix: the fix is an outcome round's.
func TestOutcomeVerifyStopCancelsAPendingFix(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 1)
	runs := e.verifyRuns(name)
	if len(runs) != 1 || e.get(name).Status.Phase != v1alpha1.IntentRevising {
		t.Fatalf("verify runs %d, want one leased", len(runs))
	}
	id := e.gh.comment(approver, "/maul verify stop")
	e.settleActions(name)
	v := e.verification(name)
	if v.CancelRun != runs[0].Name || v.AutoFix.Reason != v1alpha1.AutoFixHumanStop {
		t.Fatalf("verification = %+v, want the pending fix cancelled for the stop", v)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "cancelled") {
		t.Errorf("reply = %s", body)
	}
	jobsBefore := len(e.jobs.order)
	e.readyRepositories(repoImage)
	e.runRuns()
	run := e.verifyRuns(name)[0]
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeStoppedByHuman ||
		run.Status.JobRef != nil || len(e.jobs.order) != jobsBefore {
		t.Fatalf("verify run = %s %s job %+v", run.Status.Phase, run.Status.Outcome, run.Status.JobRef)
	}
	e.review(name, 3)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || in.Status.VerifyFixes != 0 ||
		in.Status.Verification.CancelRun != "" || len(e.verifyRuns(name)) != 1 {
		t.Fatalf("after the stop: phase %s, fixes %d, verification %+v, verify runs %d", in.Status.Phase,
			in.Status.VerifyFixes, in.Status.Verification, len(e.verifyRuns(name)))
	}
	if got := e.prVerifyPass(name).verifyFixRounds(); got != 0 {
		t.Errorf("verifyFixRounds = %d, want the cancelled fix uncounted", got)
	}
	if !strings.Contains(e.everyBody(), "stopped by an approver") {
		t.Error("the pull request was not told the automatic fix was stopped")
	}
}

// holdFix holds intent name's first verify run's Job running until the
// returned release is called, and drives the run until it has launched.
func (e *env) holdFix(name string) (v1alpha1.IntentRun, func()) {
	e.t.Helper()
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
		e.t.Fatalf("the verify fix did not launch: %s %s", r.Status.Phase, r.Status.Outcome)
	}
	return fix, func() { hold = false }
}

// TestOutcomeVerifyStopLetsARunningFixFinish twins
// TestVerifyStopLetsARunningFixFinish: the fix is an outcome round's.
func TestOutcomeVerifyStopLetsARunningFixFinish(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 1)
	_, release := e.holdFix(name)
	id := e.gh.comment(approver, "/maul verify stop")
	e.settleActions(name)
	v := e.verification(name)
	if v.CancelRun != "" || v.AutoFix.Reason != v1alpha1.AutoFixHumanStop {
		t.Fatalf("verification = %+v, want halted and nothing cancelled", v)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "already running finishes") {
		t.Errorf("reply = %s", body)
	}
	release()
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	fix := e.verifyRuns(name)[0]
	if fix.Status.Phase != v1alpha1.RunComplete || fix.Status.PushedCommit == "" || in.Status.VerifyFixes != 1 {
		t.Fatalf("the running fix did not push: %s %s", fix.Status.Phase, fix.Status.Outcome)
	}
	if a := in.Status.Verification.AutoFix; a.State != v1alpha1.AutoFixHalted || a.Reason != v1alpha1.AutoFixHumanStop {
		t.Errorf("auto-fix = %+v, want still stopped", a)
	}
}

// TestOutcomeVerifyStopOnThePullRequest twins TestVerifyStopOnThePullRequest.
func TestOutcomeVerifyStopOnThePullRequest(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.armed(name)
	pr := e.get(name).Status.PullRequests[0]
	stranger := e.prCommand(pr.Number, "mallory", "/maul verify stop")
	edited := e.prCommand(pr.Number, approver, "/maul verify stop")
	e.gh.edited[edited] = true
	e.clock.Advance(time.Second)
	e.settlePR(name)
	if a := e.verification(name).AutoFix; a.State != v1alpha1.AutoFixArmed {
		t.Fatalf("an edited or a stranger's stop applied: %+v", a)
	}
	if r := e.prReplies(pr.Number, edited); len(r) != 1 || !strings.Contains(r[0], "was edited") {
		t.Errorf("replies to the edited stop = %q", r)
	}
	if r := e.prReplies(pr.Number, stranger); len(r) != 0 {
		t.Errorf("a stranger's stop was answered: %q", r)
	}
	stop := e.prCommand(pr.Number, approver, "/maul verify stop")
	e.clock.Advance(time.Second)
	e.settlePR(name)
	e.settlePR(name)
	if a := e.verification(name).AutoFix; a.Reason != v1alpha1.AutoFixHumanStop || a.Stop == nil ||
		a.Stop.CommentID != stop {
		t.Fatalf("auto-fix = %+v, want stopped by the pull request's command", a)
	}
	if r := e.prReplies(pr.Number, stop); len(r) != 1 || !strings.Contains(r[0], "no longer fixes") {
		t.Errorf("replies to the stop = %q", r)
	}
}
