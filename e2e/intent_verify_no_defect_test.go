// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestIntentVerifyFixFindsNoDefect: the verification fails, and the
// acceptance-fix round's agent (told by its Job that it is one) concludes
// that the code has no defect, as beatbattle-44's did after a network
// policy race. The round completes as no_defect with nothing pushed and is
// not retried; automatic fixes halt (FixFoundNoDefect), the failing status
// says so, and the issue shows the agent's reason. An approver's /maul
// verify, once the application answers, runs one more verification, which
// passes.
func TestIntentVerifyFixFindsNoDefect(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusInternalServerError)
	e.kubelet.setAgentEnv("MAUL_FAKE_FIX_NO_DEFECT", "1")
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)

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

	// The approver checks again once the application answers.
	e.app.set(http.StatusOK)
	e.gh.CommentAs(number, "/maul verify", approver)
	e.waitVerification(t, name, "the requested verification to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Heads[0].Revision == pr.HeadSHA
	})
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 automated passed")
}

// TestIntentVerifyStop: an approver's /maul verify stop halts automatic
// fixes (HumanStop) and starts nothing: the failing verification that
// follows starts no fix round, while verification still runs and reports.
// /maul verify fix re-arms them, and the failure is fixed.
func TestIntentVerifyStop(t *testing.T) {
	e := startVerify(t, versionCriterion, http.StatusInternalServerError)
	number, name, pr := e.toReview(t)
	e.gh.CommentAs(number, "/maul verify stop", approver)
	e.waitVerification(t, name, "automatic fixes stopped", func(v *v1alpha1.IntentVerification) bool {
		return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanStop &&
			v.AutoFix.Stop != nil && v.AutoFix.Stop.By == approver.Login
	})
	eventually(t, "the stop to be answered", func() bool { return e.botSays(int64(number), "no longer fixes") })

	e.serve(t, name, pr.HeadSHA)
	e.waitVerification(t, name, "the verification to fail", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationFailing
	})
	e.waitStatus(t, pr.HeadSHA, "failure", "automatic fixes halted")
	consistently(t, "no fix round while stopped", func() bool {
		return len(e.verifyRuns(t, name)) == 0 && e.intent(t, name).Status.Phase == v1alpha1.IntentInReview
	})

	e.gh.CommentAs(number, "/maul verify fix", approver)
	e.waitFixRun(t, name)
}
