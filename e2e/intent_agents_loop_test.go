// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestIntentAgentsStopAndReset (S4 PR 1) is TestIntentVerifyStop's and
// TestIntentVerifyReset's agents variant, on a Project with no preview: a
// correct cli build's round 1 passes. An approver's /maul verify stop halts
// automatic fixes (HumanStop), is answered, and starts nothing. Their
// /maul verify --reset is then recorded with their account; with no Preview
// to restart it runs one more round at once, on a tool pod of its own,
// which passes, never waiting on a Preview that does not exist; and it
// leaves automatic fixes stopped (only /maul verify fix re-arms them).
func TestIntentAgentsStopAndReset(t *testing.T) {
	e := startAgents(t, agentsOptions{
		files:    map[string]string{"go.mod": tallyGoMod, "main.go": tallyBase},
		surfaces: tallyCLI(), build: tallyGreets, outcomes: tallyGreetOutcome,
		env: map[string]string{"MAUL_FAKE_CLAUDE_EVALUATE": "cli-pass", "MAUL_FAKE_CLAUDE_CLI_ARGS": "--greet",
			"MAUL_FAKE_CLAUDE_CHECK_TEXT": "hello from tally"},
	})
	number, name, pr := e.toReview(t)
	e.waitEval(t, name, "round 1 to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")

	e.gh.CommentAs(number, "/maul verify stop", approver)
	e.waitVerification(t, name, "automatic fixes stopped", func(v *v1alpha1.IntentVerification) bool {
		return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanStop &&
			v.AutoFix.Stop != nil && v.AutoFix.Stop.By == approver.Login
	})
	eventually(t, "the stop to be answered", func() bool { return e.botSays(int64(number), "no longer fixes") })
	consistently(t, "the stop starts nothing", func() bool {
		in := e.intent(t, name)
		return len(e.verifications(t, name)) == 1 && len(e.verifyRuns(t, name)) == 0 &&
			in.Status.Phase == v1alpha1.IntentInReview
	})

	e.gh.CommentAs(number, "/maul verify --reset", approver)
	in := e.waitVerification(t, name, "the reset recorded", func(v *v1alpha1.IntentVerification) bool {
		return v.Reset.Generation == 1
	})
	if r := in.Status.Verification.Reset; r.By != approver.Login || r.ByID != approver.ID {
		t.Errorf("reset = %+v, want the approver's", r)
	}
	in = e.waitEval(t, name, "the round after the reset to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Round == 2
	})
	if a := in.Status.Verification.AutoFix; a.State != v1alpha1.AutoFixHalted || a.Reason != v1alpha1.AutoFixHumanStop {
		t.Errorf("auto-fix = %+v after the reset's round, want still stopped", a)
	}
	if n, pods := len(e.verifications(t, name)), len(e.kubelet.WorkspaceRuns()); n != 2 || pods != 2 {
		t.Errorf("verifications %d, agents tool pods %d; want 2 and 2", n, pods)
	}
	if n := len(e.verifyRuns(t, name)); n != 0 {
		t.Errorf("verify-fix rounds = %d while stopped, want 0", n)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "1/1 outcomes confirmed")
	e.checkNoPreview(t)
}
