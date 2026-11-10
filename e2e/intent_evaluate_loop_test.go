// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// The verify loop's shared machinery end to end on the agent evaluator
// (S4 PR 1): repeated failures, the human's precedence over an automatic
// fix, stop and re-arm across a human push, a fix that finds no defect,
// reset, and an installation without statuses write. Each was proven end
// to end only on slice 1's tester (intent_verify_test.go,
// intent_verify_no_defect_test.go); here every round is an outcome round
// on the slot tool pod, and no tester ever runs.

// checkNoTester asserts the whole run used no slice-1 tester: every round
// was an outcome round.
func (e *evalEnv) checkNoTester(t *testing.T, name string) {
	t.Helper()
	if n := len(e.kubelet.TesterRuns()); n != 0 {
		t.Errorf("%d tester runs, want none on the evaluator", n)
	}
	for _, pv := range e.verifications(t, name) {
		if len(pv.Spec.Criteria) != 0 || len(pv.Spec.Outcomes) == 0 {
			t.Errorf("verification %s carries criteria %d and outcomes %d, want an outcome round", pv.Name,
				len(pv.Spec.Criteria), len(pv.Spec.Outcomes))
		}
	}
}

// startRegression runs the evaluator stack whose checks write their run's
// own record (so a regression of them may start an automatic fix), and
// drives an intent to round 1 passing at its pull request's head. It
// returns the env, the issue number, the Intent's name and the pull
// request.
func startRegression(t *testing.T) (*evalEnv, int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	e := startEvaluate(t, evalOptions{})
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_CHECK_WRITES", "1")
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "round 1 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes confirmed")
	return e, number, name, pr
}

// plantDefect makes the application answer 500 and the scripted evaluator
// fail every outcome on its check: the saved checks fail their replay,
// which the evaluator agrees with.
func (e *evalEnv) plantDefect() {
	e.app.set(http.StatusInternalServerError, "")
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_EVALUATE", "fail")
}

// startConfirmedDefect runs the evaluator stack on a build defective from
// round 1 (the application answers a version other than the one the build
// wrote), the scripted evaluator judging as a careful one does ("red"):
// round 1's defect is only suspected, and an approver's /maul verify fix
// confirms it, which starts the verify-fix round with no new evaluation
// (and so no request round). env names more of the fake agent's switches to
// set. It returns the env, the issue number, the Intent's name and the pull
// request.
func startConfirmedDefect(t *testing.T, env ...string) (*evalEnv, int, string, v1alpha1.IntentPullRequest) {
	t.Helper()
	e := startEvaluate(t, evalOptions{outcomes: oneOutcome, mode: "red"})
	for _, name := range env {
		e.kubelet.setAgentEnv(name, "1")
	}
	e.app.set(http.StatusOK, "0.0.9")
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "round 1's suspected defect", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationUnconfirmed && v.Suspected != nil
	})
	// Only a comment created after maul recorded the suspected defect
	// confirms it, and GitHub keeps whole seconds.
	afterSecond(in.Status.Verification.Suspected.RecordedAt.Time)
	e.requestRound(t, number)
	return e, number, name, pr
}

// waitPushed waits for fix to push and the intent to be back in review at
// its commit, which it returns.
func (e *evalEnv) waitPushed(t *testing.T, name string, fix v1alpha1.IntentRun) string {
	t.Helper()
	in := e.waitFor(t, name, "the fix pushed and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.PushedCommit != "" &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	return in.Status.PullRequests[0].HeadSHA
}

// TestIntentEvaluateRepeatedFailure twins TestIntentVerifyRepeatedFailure:
// round 1 passes; a planted defect fails the requested round 2's replay,
// which the evaluator agrees with, and one verify-fix round starts. It
// pushes, but the defect stays: the automatic round at its head fails
// exactly as before (the same outcomes on the same saved checks). Automatic
// fixes halt (RepeatedFailure), the failing status says so, and no second
// fix starts. (A human's re-arm, /maul verify fix, would start one: it
// counts fixes from its own round, as on slice 1. One more requested round
// is not asked for here: requests are spaced verifyInterval apart, and round
// 2 was one; TestIntentEvaluateFixFindsNoDefect runs a request after a
// halt.)
func TestIntentEvaluateRepeatedFailure(t *testing.T) {
	e, number, name, _ := startRegression(t)
	e.plantDefect()
	e.requestRound(t, number)
	fix := e.waitFixRun(t, name)
	head := e.waitPushed(t, name, fix)
	e.serve(t, name, head)
	in := e.waitEval(t, name, "automatic fixes halted on the same failure", func(v *v1alpha1.IntentVerification) bool {
		return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixRepeatedFailure
	})
	if v := in.Status.Verification; v.Heads[0].Revision != head || v.State != v1alpha1.VerificationFailing ||
		v.Round != 3 || v.Failed != 2 {
		t.Errorf("verification %+v, want round 3 failing both outcomes at the fix's head %s", v, head)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixRepeatedFailure) {
		t.Errorf("VerifyFailing = %+v, want True RepeatedFailure", c)
	}
	e.waitStatus(t, head, "failure", "automatic fixes halted")
	consistently(t, "no second fix round and the intent still in review", func() bool {
		return len(e.verifyRuns(t, name)) == 1 && e.intent(t, name).Status.Phase == v1alpha1.IntentInReview
	})
	e.checkNoTester(t, name)
}

// TestIntentEvaluateFixFindsNoDefect twins TestIntentVerifyFixFindsNoDefect:
// the verify-fix round a confirmed defect started (its Job told it is one)
// concludes the code has no defect. The round
// completes as no_defect with nothing pushed and is not retried; automatic
// fixes halt (FixFoundNoDefect), the failing status says so, and the issue
// shows the agent's reason. An approver's /maul verify, once the
// application answers, runs one more round, which passes.
func TestIntentEvaluateFixFindsNoDefect(t *testing.T) {
	e, number, name, pr := startConfirmedDefect(t, "MAUL_FAKE_FIX_NO_DEFECT")
	fix := e.waitFixRun(t, name)
	in := e.waitFor(t, name, "the fix found no defect and back in review", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return in.Status.Phase == v1alpha1.IntentInReview && run.Status.Phase == v1alpha1.RunComplete
	})
	run := e.run(t, fix.Name)
	if run.Status.Outcome != "no_defect" || run.Status.PushedCommit != "" {
		t.Errorf("fix run = %s %q pushed %q, want no_defect with nothing pushed", run.Status.Outcome,
			run.Status.Detail, run.Status.PushedCommit)
	}
	if head := in.Status.PullRequests[0].HeadSHA; head != pr.HeadSHA {
		t.Errorf("head moved to %s, want %s: nothing was to be pushed", head, pr.HeadSHA)
	}
	v := in.Status.Verification
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixFixFoundNoDefect {
		t.Errorf("auto-fix = %+v, want halted FixFoundNoDefect", v.AutoFix)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixFixFoundNoDefect) {
		t.Errorf("VerifyFailing = %+v, want True FixFoundNoDefect", c)
	}
	if in.Status.VerifyFixes != 1 {
		t.Errorf("verify fixes = %d, want the round counted", in.Status.VerifyFixes)
	}
	e.waitStatus(t, pr.HeadSHA, "failure", "automatic fixes halted")
	eventually(t, "the issue to show the agent's reason", func() bool {
		return e.botSays(int64(number), "found no defect") && e.botSays(int64(number), "the preview was not ready")
	})
	consistently(t, "no retry and no second fix round", func() bool {
		return len(e.verifyRuns(t, name)) == 1 && e.intent(t, name).Status.Phase == v1alpha1.IntentInReview
	})

	e.app.set(http.StatusOK, "0.1.0")
	e.gh.CommentAs(number, "/maul verify", approver)
	e.waitEval(t, name, "the requested round to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == pr.HeadSHA && v.Round == 2
	})
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	e.checkNoTester(t, name)
}

// TestIntentEvaluateHumanCancelsPendingFix twins
// TestIntentVerifyHumanCancelsPendingFix (H5): an approver's /maul revise on
// the pull request arrives while the verify-fix round a reproduced failure
// started still waits for an agent slot (another intent's plan took it
// once the round's evaluator finished). The fix is cancelled before any Job
// exists, superseded by the human, and counts as no fix; automatic fixes
// halt for the human's command, the pull request is told, and the
// approver's round runs next, on their feedback.
func TestIntentEvaluateHumanCancelsPendingFix(t *testing.T) {
	e, number, name, pr := startRegression(t)
	e.plantDefect()
	releaseEval := e.kubelet.hold(t, "evaluate")
	e.requestRound(t, number)
	within(t, roundTimeout, "round 2's evaluator launched and held", func() bool {
		for _, r := range e.stageRuns(t, name, v1alpha1.IntentStageEvaluate) {
			if r.Status.JobRef != nil && e.kubelet.held(jobs.NameFor(r.Name, intentRunKind, 1)) {
				return true
			}
		}
		return false
	})
	// Another intent's plan waits for the slot the evaluator holds, and
	// keeps it once it has it.
	releasePlan := e.kubelet.hold(t, "plan")
	_, other := e.fileIntent(t)
	otherPlan := v1alpha1.IntentRunName(other, v1alpha1.IntentStagePlan, 1, "", 1)
	eventually(t, "the other intent's plan run to wait", func() bool {
		_, ok := e.runs(t, other)[otherPlan]
		return ok
	})
	releaseEval()
	eventually(t, "the other intent's plan to hold the slot", func() bool {
		return e.kubelet.held(jobs.NameFor(otherPlan, intentRunKind, 1))
	})
	fix := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerVerify, "a verify-fix round waiting for a slot",
		func(v1alpha1.IntentRun) bool { return true })
	if run := e.run(t, fix.Name); run.Status.JobRef != nil {
		t.Fatalf("the fix launched (%+v) while the slot was held", run.Status.JobRef)
	}
	e.waitStatus(t, pr.HeadSHA, "failure", "")

	e.gh.CommentAs(int(pr.Number), "/maul revise Please also log the version at startup.", approver)
	eventually(t, "the pending fix cancelled for the human", func() bool {
		run := e.run(t, fix.Name)
		return run.Status.Phase == v1alpha1.RunFailed && run.Status.Outcome == "superseded_by_human"
	})
	if run := e.run(t, fix.Name); run.Status.JobRef != nil {
		t.Errorf("the cancelled fix has a Job %+v", run.Status.JobRef)
	}
	in := e.waitVerification(t, name, "automatic fixes halted for the human's command",
		func(v *v1alpha1.IntentVerification) bool {
			return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanCommand
		})
	if in.Status.VerifyFixes != 0 {
		t.Errorf("verify fixes = %d, want 0: a cancelled fix is no fix", in.Status.VerifyFixes)
	}
	eventually(t, "the pull request told the fix was cancelled", func() bool {
		return e.botSays(pr.Number, "cancelled for your review")
	})

	releasePlan()
	human := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerCommand, "the approver's round to complete",
		func(run v1alpha1.IntentRun) bool { return run.Status.Phase == v1alpha1.RunComplete })
	job := e.kubelet.waitRun(t, "the approver's round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(human.Name, intentRunKind, 1)
	})
	if !strings.Contains(string(job.Investigation), "Please also log the version at startup.") {
		t.Errorf("the approver's round was not handed their feedback:\n%s", job.Investigation)
	}
	in = e.waitPhase(t, name, v1alpha1.IntentInReview)
	if in.Status.VerifyFixes != 0 || in.Status.Revisions != 1 || len(e.verifyRuns(t, name)) != 1 {
		t.Errorf("verify fixes %d revisions %d fix rounds %d, want 0, 1, 1", in.Status.VerifyFixes,
			in.Status.Revisions, len(e.verifyRuns(t, name)))
	}
	e.checkNoTester(t, name)
}

// TestIntentEvaluateHumanDuringRunningFix twins
// TestIntentVerifyHumanDuringRunningFix (H6): an approver's /maul revise
// arrives while the verify-fix round's agent runs. The fix finishes and
// pushes; automatic fixes halt for the human; the approver's round runs
// next, from the fix's commit, told the automatic fix landed after their
// feedback.
func TestIntentEvaluateHumanDuringRunningFix(t *testing.T) {
	e, number, name, pr := startRegression(t)
	release := e.kubelet.hold(t, "build")
	e.plantDefect()
	e.requestRound(t, number)
	fix := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerVerify, "the verify-fix round's agent running",
		func(run v1alpha1.IntentRun) bool {
			return run.Status.JobRef != nil && e.kubelet.held(jobs.NameFor(run.Name, intentRunKind, 1))
		})
	cmd := e.gh.CommentAs(int(pr.Number), "/maul revise Please also log the version at startup.", approver)
	in := e.waitVerification(t, name, "automatic fixes halted for the human's command",
		func(v *v1alpha1.IntentVerification) bool {
			return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanCommand
		})
	if in.Status.Verification.CancelRun != "" {
		t.Errorf("cancelRun = %q: a launched fix is never cancelled", in.Status.Verification.CancelRun)
	}
	for _, c := range e.gh.IssueComments(int(pr.Number)) {
		if c.ID == cmd {
			afterSecond(c.CreatedAt)
		}
	}
	release()
	eventually(t, "the fix to push", func() bool {
		run := e.run(t, fix.Name)
		return run.Status.Phase == v1alpha1.RunComplete && run.Status.PushedCommit != ""
	})
	pushed := e.run(t, fix.Name).Status.PushedCommit

	human := e.runOfTrigger(t, name, v1alpha1.IntentRunTriggerCommand, "the approver's round to run",
		func(run v1alpha1.IntentRun) bool { return run.Status.JobRef != nil })
	job := e.kubelet.waitRun(t, "the approver's round's Job", func(r agentRun) bool {
		return r.Job.Name == jobs.NameFor(human.Name, intentRunKind, 1)
	})
	handoff := string(job.Investigation)
	if !strings.Contains(handoff, "An automatic acceptance fix landed after your review: `"+pushed+"`") ||
		!strings.Contains(handoff, "Please also log the version at startup.") {
		t.Errorf("the approver's round was not told of the fix %s, or not handed their feedback:\n%s", pushed,
			handoff)
	}
	if run := e.run(t, human.Name); run.Status.BaseSHA != pushed {
		t.Errorf("the approver's round starts from %s, want the fix's %s", run.Status.BaseSHA, pushed)
	}
	in = e.waitFor(t, name, "the approver's round done", func(in *v1alpha1.Intent) bool {
		return in.Status.Phase == v1alpha1.IntentInReview && in.Status.Revisions == 1
	})
	if in.Status.VerifyFixes != 1 || len(e.verifyRuns(t, name)) != 1 {
		t.Errorf("verify fixes %d, fix rounds %d, want 1 and 1", in.Status.VerifyFixes, len(e.verifyRuns(t, name)))
	}
	e.checkNoTester(t, name)
}

// stopFixes has an approver's /maul verify stop halt automatic fixes
// (HumanStop) and waits for it to be recorded and answered.
func (e *evalEnv) stopFixes(t *testing.T, number int, name string) {
	t.Helper()
	e.gh.CommentAs(number, "/maul verify stop", approver)
	e.waitVerification(t, name, "automatic fixes stopped", func(v *v1alpha1.IntentVerification) bool {
		return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanStop &&
			v.AutoFix.Stop != nil && v.AutoFix.Stop.By == approver.Login
	})
	eventually(t, "the stop to be answered", func() bool { return e.botSays(int64(number), "no longer fixes") })
}

// TestIntentEvaluateStop twins TestIntentVerifyStop: round 1 passes; an
// approver's /maul verify stop halts automatic fixes (HumanStop) and is
// answered; a planted defect then fails the requested round, a reproduced
// failure that starts no fix while stopped, verification still reporting.
// /maul verify fix re-arms them, and the failure gets its fix round.
func TestIntentEvaluateStop(t *testing.T) {
	e, number, name, pr := startRegression(t)
	e.stopFixes(t, number, name)

	e.plantDefect()
	e.gh.CommentAs(number, "/maul verify", approver)
	e.waitEval(t, name, "the requested round to fail", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationFailing && v.Round == 2
	})
	e.waitStatus(t, pr.HeadSHA, "failure", "automatic fixes halted")
	consistently(t, "no fix round while stopped", func() bool {
		in := e.intent(t, name)
		return len(e.verifyRuns(t, name)) == 0 && in.Status.Phase == v1alpha1.IntentInReview &&
			in.Status.Verification.AutoFix.Reason == v1alpha1.AutoFixHumanStop
	})

	e.gh.CommentAs(number, "/maul verify fix", approver)
	fix := e.waitFixRun(t, name)
	if ref := fix.Spec.Inputs.VerificationRef; ref == nil || ref.Name != e.round(t, name, 2).Name {
		t.Errorf("fix run inputs %+v, want the failing round 2", fix.Spec.Inputs)
	}
	e.checkNoTester(t, name)
}

// TestIntentEvaluateFixAfterAHumanPush twins
// TestIntentVerifyFixAfterAHumanPush (S1 demo bug 2): automatic fixes are
// stopped; the approver pushes to the pull request and re-arms them with
// /maul verify fix a second later, before maul need have seen the push.
// GitHub recorded the push first, so the re-arm stands once maul records
// the pushed head: the application now fails there, the saved checks'
// replay with it, and an automatic fix round starts on top of the human's
// commit.
func TestIntentEvaluateFixAfterAHumanPush(t *testing.T) {
	e, number, name, _ := startRegression(t)
	e.stopFixes(t, number, name)

	e.plantDefect()
	branch := v1alpha1.IntentBranchPrefix + name
	pushed := e.gh.HumanPush(intentOwner, appRepo, branch, "NOTES.md", "Written by hand.\n", "docs: notes")
	afterSecond(time.Now().UTC().Truncate(time.Second))
	e.gh.CommentAs(number, "/maul verify fix", approver)
	e.serve(t, name, pushed)
	e.waitEval(t, name, "the pushed head recorded with automatic fixes armed",
		func(v *v1alpha1.IntentVerification) bool {
			return len(v.Heads) == 1 && v.Heads[0].Revision == pushed && v.AutoFix.Rearm != nil &&
				v.AutoFix.State == v1alpha1.AutoFixArmed
		})
	fix := e.waitFixRun(t, name)
	if v := e.intent(t, name).Status.Verification; v.AutoFix.Reason == v1alpha1.AutoFixHumanPush {
		t.Errorf("auto-fix = %+v: the push halted it after the later re-arm", v.AutoFix)
	}
	if ref := fix.Spec.Inputs.VerificationRef; ref == nil {
		t.Errorf("fix run inputs %+v, want a verification ref", fix.Spec.Inputs)
	} else if pv := e.verifications(t, name)[ref.Name]; pv.Spec.Heads[0].Revision != pushed {
		t.Errorf("the fix is for the round at %s, want the one at the pushed %s", pv.Spec.Heads[0].Revision,
			pushed)
	}
	e.waitFor(t, name, "the fix pushed on top of the human's commit", func(in *v1alpha1.Intent) bool {
		run := e.run(t, fix.Name)
		return run.Status.PushedCommit != "" && run.Status.BaseSHA == pushed &&
			in.Status.PullRequests[0].HeadSHA == run.Status.PushedCommit
	})
	e.checkNoTester(t, name)
}

// TestIntentEvaluateReset twins TestIntentVerifyReset: /maul verify --reset
// bumps the intent's reset generation, which reaches the Preview;
// preview-controller restarts its parts, and once they run for the reset
// another outcome round runs, on a tool pod of its own, and passes.
func TestIntentEvaluateReset(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	ctx := context.Background()
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "round 1 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_EVALUATE", "nochecks")

	e.gh.CommentAs(number, "/maul verify --reset", approver)
	in := e.waitVerification(t, name, "the reset recorded", func(v *v1alpha1.IntentVerification) bool {
		return v.Reset.Generation == 1
	})
	if r := in.Status.Verification.Reset; r.By != approver.Login || r.ByID != approver.ID {
		t.Errorf("reset = %+v, want the approver's", r)
	}
	eventually(t, "the preview to carry the reset", func() bool {
		var p v1alpha1.Preview
		return e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &p) == nil &&
			p.Spec.ResetGeneration == 1
	})
	eventually(t, "the Deployment's template to carry the reset", func() bool {
		var dep appsv1.Deployment
		key := types.NamespacedName{Namespace: "maul-preview-0", Name: "preview-" + name}
		return e.cl.client.Get(ctx, key, &dep) == nil && len(dep.Spec.Template.Annotations) > 0
	})
	if n := len(e.verifications(t, name)); n != 1 {
		t.Errorf("verifications = %d before the parts ran for the reset, want 1", n)
	}
	e.serve(t, name, pr.HeadSHA)
	eventually(t, "the preview to observe the reset", func() bool {
		var p v1alpha1.Preview
		return e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &p) == nil &&
			p.Status.ObservedResetGeneration == 1
	})
	e.waitEval(t, name, "the round after the reset to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Round == 2
	})
	if n, tools := len(e.verifications(t, name)), len(e.kubelet.ToolRuns()); n != 2 || tools != 2 {
		t.Errorf("verifications %d, tool pods %d; want 2 and 2", n, tools)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes confirmed")
	e.checkNoTester(t, name)
}

// TestIntentEvaluateWithoutStatusesWrite twins
// TestIntentVerifyWithoutStatusesWrite: statuses write is never part of a
// Project's Ready proof (D5, narrowed). An installation without it leaves a
// verifying Project Ready, with AcceptanceStatusUnavailable naming the
// repository and the permission; the evaluation still runs and is handed
// over, and no maul/acceptance is ever posted.
func TestIntentEvaluateWithoutStatusesWrite(t *testing.T) {
	e := startEvaluate(t, evalOptions{github: func(gh *fakegithub.Server) { gh.DenyPermission("statuses") }})
	ctx := context.Background()
	var unavailable *metav1.Condition
	eventually(t, "the status unavailable for want of statuses write", func() bool {
		var p v1alpha1.Project
		if err := e.cl.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, &p); err != nil {
			return false
		}
		unavailable = meta.FindStatusCondition(p.Status.Conditions, "AcceptanceStatusUnavailable")
		return meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady) &&
			unavailable != nil && unavailable.Status == metav1.ConditionTrue
	})
	if !strings.Contains(unavailable.Message, appRepoURL) || !strings.Contains(unavailable.Message, "statuses: write") {
		t.Errorf("AcceptanceStatusUnavailable = %+v, want it naming statuses: write on %s", unavailable, appRepoURL)
	}
	_, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	in := e.waitEval(t, name, "the evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	if h := in.Status.Verification.Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonReady {
		t.Errorf("handoff %+v, want released ready", h)
	}
	eventually(t, "the preview comment to link the preview as ready", func() bool {
		body := e.previewComment(name, pr.Number)
		return strings.Contains(body, "Ready: maul checked the approved outcomes.") &&
			strings.Contains(body, name+previewHostSuffix)
	})
	consistently(t, "no maul/acceptance on the head", func() bool {
		_, posted := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus)
		return !posted
	})
	e.checkNoTester(t, name)
}
