// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// The outcome twins of the verify loop's tests (S4 PR 1): every behaviour
// of the shared verification machinery (fix rounds, their bounds, the
// human's precedence, the commands) that verify_*_test.go proves through
// a slice-1 criteria plan is proven here again through an approved
// outcomes plan judged by the agent evaluator, so that the criteria path
// can be deleted without deleting the only guard of the shared code. Each
// helper holds its round to the outcome path: outcomes on the request, no
// criteria.

// evalRound plays one outcome round of intent name to its judgement: the
// tool pod Ready, the evaluator run, the stored checks replayed as runs,
// the reviews settled. It returns the round's PreviewVerification.
func (e *env) evalRound(name string, runs ...v1alpha1.CheckRun) v1alpha1.PreviewVerification {
	e.t.Helper()
	v := e.verification(name)
	if v.Ref == nil {
		e.t.Fatalf("no round to play: %+v", v)
	}
	pv := e.pvNamed(v.Ref.Name)
	outcomeRequest(e.t, pv)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, runs...)
	e.settleReviews(name)
	return *e.pvNamed(pv.Name)
}

// outcomeRequest fails t unless pv is an outcome round: outcomes and their
// digest on the request.
func outcomeRequest(t *testing.T, pv *v1alpha1.PreviewVerification) {
	t.Helper()
	if len(pv.Spec.Outcomes) == 0 || pv.Spec.OutcomesDigest == "" {
		t.Fatalf("verification %s is no outcome round: outcomes %d digest %q", pv.Name,
			len(pv.Spec.Outcomes), pv.Spec.OutcomesDigest)
	}
}

// replays are runs of version and health's checks with these results, the
// version check's first.
func replays(version, health v1alpha1.SavedCheck, vr, hr v1alpha1.CheckRunResult) []v1alpha1.CheckRun {
	return []v1alpha1.CheckRun{{CheckDigest: version.Digest, Result: vr, Token: "t1", Seq: 2},
		{CheckDigest: health.Digest, Result: hr, Token: "t2", Seq: 3}}
}

// outcomeEnv is newEvaluatorEnv whose evaluator reports the version
// outcome failing and the health outcome passing, with both outcomes'
// checks stored and reviewed proves. It returns the env, the intent's name
// and the two checks.
func outcomeEnv(t *testing.T) (*env, string, v1alpha1.SavedCheck, v1alpha1.SavedCheck) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(failingReport)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	return e, name, version, health
}

// failingEvaluation is failingVerification's outcome twin: a new intent of
// e in review, its first round live, the version outcome's saved check
// failing its replay with the evaluator's cited fail: judged Failing, a
// reproduced failure. It returns the env, the intent and the round's
// PreviewVerification's name.
func failingEvaluation(t *testing.T) (*env, string, string) {
	t.Helper()
	e, name, version, health := outcomeEnv(t)
	pv := e.evalRound(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing || v.Failed != 1 {
		t.Fatalf("verification = %s %+v, want Failing on the version outcome", v.State, v.Outcomes)
	}
	return e, name, pv.Name
}

// TestOutcomeFixRoundStartsAndPushes is TestVerifyFixRoundStartsAndPushes
// on outcomes: a reproduced failure at maul's own heads starts one
// acceptance-fix round on the app's repository, consuming the round by
// UID; its handoff records the evaluation's fingerprint; it pushes and is
// counted as a verify fix, not a revision.
func TestOutcomeFixRoundStartsAndPushes(t *testing.T) {
	e, name, pvName := failingEvaluation(t)
	e.reviewUntil(name, func() bool { return len(e.verifyRuns(name)) > 0 })
	in := e.get(name)
	runs := e.verifyRuns(name)
	if in.Status.Phase != v1alpha1.IntentRevising || len(runs) != 1 {
		t.Fatalf("phase %s, verify runs %d; want Revising with one", in.Status.Phase, len(runs))
	}
	pv := e.pvNamed(pvName)
	run := runs[0]
	if ref := run.Spec.Inputs.VerificationRef; ref == nil || ref.Name != pvName || ref.UID != pv.UID ||
		!sameRepo(run.Spec.Repository.URL, appRepoURL) || run.Spec.Round != 1 {
		t.Fatalf("verify run = %+v", run.Spec)
	}
	if v := in.Status.Verification; v.State != v1alpha1.VerificationFixing {
		t.Fatalf("verification = %+v, want Fixing", v)
	}
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.VerifyFixes != 1 || in.Status.Revisions != 0 || in.Status.CheckFixes != 0 {
		t.Fatalf("counters = revisions %d checkFixes %d verifyFixes %d", in.Status.Revisions,
			in.Status.CheckFixes, in.Status.VerifyFixes)
	}
	run = e.verifyRuns(name)[0]
	if run.Status.Phase != v1alpha1.RunComplete || run.Status.PushedCommit == "" {
		t.Fatalf("verify run = %s %s %q", run.Status.Phase, run.Status.Outcome, run.Status.Detail)
	}
	cm := e.runInput(run)
	if sig := cm.Data[keyVerifySignature]; !strings.HasPrefix(sig, acceptance.EvaluationFingerprintVersion+":") {
		t.Errorf("signature = %q, want the evaluation's fingerprint", sig)
	}
	if cm.Data[keyCheckSignature] != "" {
		t.Errorf("a verify round recorded a check signature %q", cm.Data[keyCheckSignature])
	}
}

// fixedAndFailingAgain drives intent name's fix round to push, the Preview
// live at the fixed heads, and plays round 2 there with runs; it returns
// the verification.
func (e *env) fixedAndFailingAgain(name string, runs ...v1alpha1.CheckRun) *v1alpha1.IntentVerification {
	e.t.Helper()
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.readyPreview(name)
	e.review(name, 4)
	if again := e.verification(name); again.Ref == nil || again.Round != 2 {
		e.t.Fatalf("no re-verification at the fixed heads: %+v", again)
	}
	e.evalRound(name, runs...)
	e.review(name, 3)
	return e.verification(name)
}

// TestOutcomeFixRepeatedFailureHalts is TestVerifyFixRepeatedFailureHalts
// on outcomes: the round after a fix fails exactly as before (the same
// outcome on the same saved check): automatic fixes halt
// (RepeatedFailure), the condition says so, and the intent stays in
// review.
func TestOutcomeFixRepeatedFailureHalts(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	v := e.fixedAndFailingAgain(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentInReview || len(e.verifyRuns(name)) != 1 ||
		v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixRepeatedFailure {
		t.Fatalf("phase %s, verify runs %d, verification %+v", in.Status.Phase, len(e.verifyRuns(name)), v)
	}
	if c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason != string(v1alpha1.AutoFixRepeatedFailure) {
		t.Fatalf("condition = %+v", c)
	}
}
