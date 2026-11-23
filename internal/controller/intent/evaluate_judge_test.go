// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
)

// outcomesPlan's outcomes.
const (
	oVersion = "version-shown"
	oHealthy = "still-healthy"
	oManual  = "looks-right"
)

// judgeRoute is what the humans did around a round's judgement
// (routeRecords).
type judgeRoute struct {
	acceptManual, acceptVersion, noDefect bool
	// rearm is when an approver's /maul verify fix re-armed automatic
	// fixes, relative to the no-defect conclusion (zero: none did).
	rearm time.Duration
}

// routeRecords writes what tc's humans did onto in: their acceptances at
// the heads, and a fix round's no-defect conclusion there.
func routeRecords(in *v1alpha1.Intent, tc judgeRoute, now time.Time) {
	v := in.Status.Verification
	accept := func(id string) {
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{
			ID: id, By: approver, ByID: 7, At: metav1.NewTime(now), CommentID: 99,
			HeadSetDigest: v.HeadSetDigest, Heads: []string{"abcdef1"}})
	}
	if tc.acceptManual {
		accept(oManual)
	}
	if tc.acceptVersion {
		accept(oVersion)
	}
	if tc.noDefect {
		v.NoDefect = &v1alpha1.NoDefectRecord{Run: "x-rev-r2-a1", Reason: "fine", At: metav1.NewTime(now),
			HeadSetDigest: v.HeadSetDigest}
	}
	if tc.rearm != 0 {
		at := metav1.NewTime(now.Add(tc.rearm))
		v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixArmed, Since: &at,
			Rearm: &v1alpha1.Rearm{By: approver, ByID: 7, At: at, Round: in.Status.Rounds}}
	}
}

// failingReport is an evaluator's report that the version outcome fails,
// citing the call it saw fail.
const failingReport = `---
verdicts:
  - id: version-shown
    verdict: fail
    ledger: [2]
  - id: still-healthy
    verdict: pass
    ledger: [3]
---
The version endpoint answers with the wrong note.
`

// evaluateReporting makes e's evaluate Jobs report text.
func (e *env) evaluateReporting(text string) {
	prev := e.jobs.output
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "evaluate" {
			return evaluationOutput(envelope.Stage{Outcome: envelope.OutcomeOK, Harness: "claude", Model: spec.Model,
				Usage: envelope.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.4}}, text)
		}
		return prev(spec)
	}
}

// TestEvaluationConfirmsAndHandsOver: a round whose evaluator sees every
// outcome hold, each verdict citing a call of its own maul counts, with the
// manual outcome accepted by an approver, is judged ready: Passed, every
// outcome seen to hold with the call it cites recorded, the link released
// ready in the judgement's own write, and the status comment shows it
// labelled.
func TestEvaluationConfirmsAndHandsOver(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	if len(pv.Spec.Checks) != 0 || pv.Spec.ChecksDigest != "" || pv.Spec.Rung != v1alpha1.RungFull {
		t.Fatalf("verification checks = %+v, rung %s; want none, full", pv.Spec.Checks, pv.Spec.Rung)
	}
	if body := e.statusBody(); strings.Contains(body, previewHostOf(name)) {
		t.Errorf("the link is shown while held:\n%s", body)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{ID: oManual,
			By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99, HeadSetDigest: v.HeadSetDigest})
	})
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationPassed || v.Passed != 2 {
		t.Fatalf("verification = %s %+v, want Passed", v.State, v.Outcomes)
	}
	if v.Handoff == nil || !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonReady {
		t.Errorf("hand-off = %+v, want released ready with the judgement", v.Handoff)
	}
	for i, r := range v.Outcomes[:2] {
		if r.Reason != acceptance.ReasonSeenToHold || !slices.Equal(r.Cites, []int32{int32(i + 3)}) {
			t.Errorf("outcome %+v, want seen to hold on the call it cites", r)
		}
	}
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "**Preview:** Ready") })
	if body := e.statusBody(); !strings.Contains(body, templates.ReadyLabel) ||
		!strings.Contains(body, previewHostOf(name)) {
		t.Errorf("status comment does not hand over the link labelled ready:\n%s", body)
	}
}

// reviewUntil runs review passes, a minute apart, until done holds, at most
// a dozen.
func (e *env) reviewUntil(name string, done func() bool) {
	e.t.Helper()
	for range 12 {
		if done() {
			return
		}
		e.review(name, 1)
	}
	e.t.Fatalf("intent %s: the condition never held; phase %s, verification %+v", name, e.get(name).Status.Phase,
		e.verification(name))
}

// TestEvaluationEnvFailedWithoutLedgerDetail: preview-controller drops an
// EnvFailed round's ledger when its status is over the size cap, keeping the phase and the facts. Judged so, with the
// evaluator reporting every outcome holds, no outcome passes or fails: each
// is unsure on the application's failure (env-failed, never maul's infra,
// though no index verified), and the hand-off says the environment failed.
func TestEvaluationEnvFailedWithoutLedgerDetail(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationEnvFailed, toolPodIP, true)
	got := e.pvNamed(pv.Name)
	got.Status.Reason = v1alpha1.VerificationReasonComponentCrashed
	got.Status.Ledger = nil
	got.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent, Ready: true,
		CrashedDuring: true, Restarts: 1}}
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || len(v.Outcomes) == 0 {
		t.Fatalf("verification = %s %+v, want Unconfirmed", v.State, v.Outcomes)
	}
	for _, r := range v.Outcomes {
		if r.Result == v1alpha1.OutcomePassed || r.Result == v1alpha1.OutcomeFailed {
			t.Errorf("outcome %s is %s with no ledger detail in a failing environment", r.ID, r.Result)
		}
		if r.ID != oManual && r.Reason != acceptance.ReasonEnvFailed {
			t.Errorf("outcome %+v, want env-failed", r)
		}
	}
	if got := environmentWhy(v); got != templates.HandoffWhyEnvironment {
		t.Errorf("hand-off why = %q, want the environment's", got)
	}
}

// TestEvaluationFailureStartsAFix: the evaluator's fail, citing a call maul
// counts, is a failure: the verification is Failing and the kept machinery
// starts one acceptance-fix round on it, handed the failing outcome's text
// by its id, never the evaluator's report or an earlier release's check
// store; the link stays held while it runs.
func TestEvaluationFailureStartsAFix(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(failingReport)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.settleReviews(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing || v.Failed != 1 {
		t.Fatalf("verification = %s %+v, want Failing on the version outcome", v.State, v.Outcomes)
	}
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerVerify || run.Spec.Inputs.VerificationRef == nil ||
		run.Spec.Inputs.VerificationRef.UID != pv.UID {
		t.Fatalf("revise run = %+v, want the acceptance fix of the round", run.Spec)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationFixing || v.Handoff.Released {
		t.Errorf("verification = %s, hand-off %+v; want Fixing with the link held", v.State, v.Handoff)
	}
	var in *corev1.ConfigMap
	for range 8 {
		var cm corev1.ConfigMap
		if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: run.Spec.Inputs.ConfigMap},
			&cm); err == nil {
			in = &cm
			break
		}
		e.readyRepositories(repoImage)
		e.runRuns()
		e.review(name, 1)
	}
	if in == nil {
		t.Fatal("the fix round has no input")
	}
	all := ""
	for _, s := range in.Data {
		all += s
	}
	if !strings.Contains(all, "GET /version answers with the commit it runs") || !strings.Contains(all,
		"`version-shown`") {
		t.Errorf("the fix round's input lacks the failing outcome:\n%s", all)
	}
	for _, f := range []string{"-checks-", "wrong note"} {
		if strings.Contains(all, f) {
			t.Errorf("the fix round's input carries %q", f)
		}
	}
}

// liveShapedFailingReport is failingReport as the live evaluators wrote
// reports on 0.13.7 (S1 bug 5): each verdict names its outcome as outcome.
// (Its disputes, proposal naming a file and retire lists are gone: a report
// writing a retired key, even as an empty list, now claims nothing,
// TestRetiredReportKeysNeverPass.)
const liveShapedFailingReport = `---
verdicts:
  - outcome: "version-shown"
    verdict: fail
    ledger: [2]
    note: "The version endpoint answers with the wrong note."
  - outcome: "still-healthy"
    verdict: pass
    ledger: [3]
anomalies: []
---
The version endpoint answers with the wrong note.
`

// TestLiveShapedReportIsJudged (S1 bug 5): a report shaped like the live
// failures is no longer refused: its claims are read, and the round is
// judged on them as on any other report, the failure reproduced and a fix
// started (before, the report was refused and the round claimed nothing).
func TestLiveShapedReportIsJudged(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(liveShapedFailingReport)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationFailing || v.Failed != 1 {
		t.Fatalf("verification = %s %+v, want Failing on the version outcome", v.State, v.Outcomes)
	}
	if r := v.Outcomes[1]; r.ID != oHealthy || r.Result != v1alpha1.OutcomePassed {
		t.Errorf("health outcome = %+v, want passed on the call it cites", r)
	}
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
}
