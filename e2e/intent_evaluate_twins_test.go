// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The last of slice 1's verify e2e (intent_verify_test.go, retired in S4
// with the criteria it ran), proven again on the agent evaluator: a plan
// the real agent-runner states outcomes in, a plan stating none, and a
// manual outcome an approver accepts before a human pushes.

// manualOutcome follows versionOutcomes in a plan that also asks a human.
const manualOutcome = `
  - id: notes-read-well
    outcome: the release notes read well
    manual: true
    reason: only a human can judge how they read`

// TestIntentEvaluateAgentPlansOutcomes twins
// TestIntentVerifyAgentPlansCriteria: the plan of a verifying Project runs
// on the REAL agent-runner, with hack/fake-agent/claude as the model, which
// states acceptance only when its prompt asks for it, as a model would. The
// plan Job carries the Project's previewed components and the evaluator's
// ask, the prompt the agent receives teaches the acceptance outcomes field
// (never criteria), the posted plan comment shows the approver the
// outcomes, and the planned outcomes are recorded and confirmed against
// the preview.
func TestIntentEvaluateAgentPlansOutcomes(t *testing.T) {
	e := startEvaluate(t, evalOptions{agentRunner: []string{"plan"}})
	number, name, pr := e.toReview(t)

	r := e.kubelet.waitRun(t, "the plan Job", func(r agentRun) bool { return r.Env["MAUL_PHASE"] == "plan" })
	if r.ExitCode != 0 {
		t.Fatalf("plan agent-runner exited %d:\n%s", r.ExitCode, r.Stdout)
	}
	if got, want := r.Env["MAUL_PLAN_VERIFICATION"],
		`{"components":[{"key":"shop","path":"/","readiness":"/healthz"}],"outcomes":true}`; got != want {
		t.Errorf("plan Job MAUL_PLAN_VERIFICATION = %q, want %q", got, want)
	}
	prompt, err := os.ReadFile(filepath.Join(r.Workspace, ".fake-claude", "prompt-plan"))
	if err != nil {
		t.Fatalf("read the prompt the scripted claude received: %v", err)
	}
	if p := string(prompt); !strings.Contains(p, "### Acceptance outcomes") ||
		strings.Contains(p, "### Acceptance criteria") {
		t.Errorf("the plan prompt does not ask for outcomes alone:\n%s", p)
	}

	in := e.waitFor(t, name, "the approved plan's outcomes recorded", func(in *v1alpha1.Intent) bool {
		return in.Status.Acceptance != nil && len(in.Status.Acceptance.Outcomes) > 0
	})
	if acc := in.Status.Acceptance; len(acc.Outcomes) != 2 || acc.Outcomes[0].ID != "version-served" ||
		acc.Outcomes[1].ID != "still-serving" || len(acc.Criteria) != 0 {
		t.Fatalf("recorded acceptance = %+v, want the planner's two outcomes and no criteria", acc)
	}
	pl := in.Status.Plan
	plans := e.own(number, templates.PlanMarker(namespace, name, pl.Revision, pl.Digest))
	if len(plans) != 1 || !strings.Contains(plans[0].Body, "version-served") {
		t.Errorf("plan comments = %+v, want one showing the outcomes", plans)
	}

	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "the planned outcomes to be confirmed", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed
	})
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes confirmed")
	e.checkNoTester(t, name)
}

// TestIntentEvaluateNotDeclared twins TestIntentVerifyNotDeclared: a plan
// stating no outcomes is reported NotDeclared: nothing is evaluated, and
// maul/acceptance is pending (never green) until an approver's /maul
// accept all, which turns it success naming them.
func TestIntentEvaluateNotDeclared(t *testing.T) {
	e := startEvaluate(t, evalOptions{noOutcomes: true})
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	e.waitEval(t, name, "the verification NotDeclared", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationNotDeclared && v.HeadSetDigest != ""
	})
	e.waitStatus(t, pr.HeadSHA, "pending", "an approver may /maul accept all")
	consistently(t, "nothing evaluated and nothing green", func() bool {
		st, _ := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus)
		return st.State == "pending" && len(e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)) == 0 &&
			len(e.verifications(t, name)) == 0
	})
	e.gh.CommentAs(number, "/maul accept all", approver)
	e.waitStatus(t, pr.HeadSHA, "success", "accepted by "+approver.Login)
	e.checkNoTester(t, name)
}

// TestIntentEvaluateHumanPushAndAccept twins
// TestIntentVerifyHumanPushAndAccept: the evaluator confirms the plan's
// evaluated outcomes, and maul/acceptance stays short of green until an
// approver accepts its manual one with /maul accept, which turns it
// success, naming them. A human then pushes to the pull request's branch:
// the combined change moved, so automatic fixes halt (HumanPush) and
// maul/acceptance on the new head is pending, never carrying the earlier
// acceptance to heads nobody checked. The approver asks for a round; the
// application now fails it, which is reported failing, with no
// automatic fix.
func TestIntentEvaluateHumanPushAndAccept(t *testing.T) {
	e := startEvaluate(t, evalOptions{outcomes: versionOutcomes + manualOutcome})
	// The checks write their run's own record, so a regression of them is
	// a defect maul confirms itself, which would start an automatic fix.
	e.kubelet.setAgentEnv("MAUL_FAKE_CLAUDE_CHECK_WRITES", "1")
	number, name, pr := e.toReview(t)
	e.serve(t, name, pr.HeadSHA)
	// The evaluated two confirmed, the manual one open: what maul could not
	// confirm stands until an approver accepts it.
	e.waitEval(t, name, "the evaluated outcomes confirmed, the manual one open",
		func(v *v1alpha1.IntentVerification) bool {
			return v.Passed == 2 && v.ManualOpen == 1 && v.Handoff != nil && v.Handoff.Released
		})
	e.waitStatus(t, pr.HeadSHA, "error", "maul couldn't confirm this")
	consistently(t, "nothing green before the manual outcome is accepted", func() bool {
		st, _ := e.gh.LatestStatus(pr.HeadSHA, acceptanceStatus)
		return st.State != "success"
	})

	accept := e.gh.CommentAs(number, "/maul accept notes-read-well", approver)
	in := e.waitFor(t, name, "the manual outcome accepted", func(in *v1alpha1.Intent) bool {
		return in.Status.Acceptance != nil && len(in.Status.Acceptance.Accepted) == 1
	})
	rec := in.Status.Acceptance.Accepted[0]
	if rec.ID != "notes-read-well" || rec.By != approver.Login || rec.ByID != approver.ID ||
		rec.CommentID != accept || rec.HeadSetDigest != in.Status.Verification.HeadSetDigest {
		t.Errorf("acceptance record = %+v, want the approver's at the verified head set", rec)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "accepted by hand")

	branch := v1alpha1.IntentBranchPrefix + name
	pushed := e.gh.HumanPush(intentOwner, appRepo, branch, "NOTES.md", "Written by hand.\n", "docs: notes")
	e.waitEval(t, name, "automatic fixes halted for the human's push", func(v *v1alpha1.IntentVerification) bool {
		return v.AutoFix.State == v1alpha1.AutoFixHalted && v.AutoFix.Reason == v1alpha1.AutoFixHumanPush
	})
	e.waitStatus(t, pushed, "pending", "combined change moved: re-verifying")
	e.serve(t, name, pushed)
	consistently(t, "nothing green on the pushed head before it is verified", func() bool {
		st, _ := e.gh.LatestStatus(pushed, acceptanceStatus)
		return st.State != "success"
	})

	// The approver asks; the application now fails: reported, not fixed.
	e.plantDefect()
	e.gh.CommentAs(number, "/maul verify", approver)
	// The saved checks' replay fails: a regression maul confirms itself,
	// reported failing, and halted fixes keep it from being fixed.
	e.waitEval(t, name, "the requested round to fail at the pushed head", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationFailing && v.Heads[0].Revision == pushed
	})
	e.waitStatus(t, pushed, "failure", "automatic fixes halted")
	consistently(t, "no automatic fix after a human's push", func() bool {
		return len(e.verifyRuns(t, name)) == 0 && e.intent(t, name).Status.Phase == v1alpha1.IntentInReview
	})
	e.checkNoTester(t, name)
}
