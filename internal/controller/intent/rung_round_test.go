// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// Round helpers shared by the evaluation tests (the cost ladder's replay
// rung they once drove is retired: every round is full), end to end through
// the intent reconciler and the run scheduler, preview-controller played.

// anomalyReport is an evaluator's report that both outcomes hold, with a
// look-around anomaly it cites.
const anomalyReport = `---
verdicts:
  - id: version-shown
    verdict: pass
    ledger: [3]
  - id: still-healthy
    verdict: pass
    ledger: [4]
anomalies:
  - summary: the console logs an uncaught TypeError on /
    route: /
    ledger: [2]
---
Both outcomes hold; the home page throws in the console.
`

// observedRound drives intent name's round, as its first attempt, to its
// judgement: the evaluator reports as e's evaluate Jobs do, and the round's
// verified ledger is five head http calls of the evaluator's.
func (e *env) observedRound(name string, round int32) {
	e.t.Helper()
	pv := e.roundOf(name, round)
	if pv.Spec.Rung != v1alpha1.RungFull {
		e.t.Fatalf("round %d is %s, want full", pv.Spec.Round, pv.Spec.Rung)
	}
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(pv.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.settleReviews(name)
}

// roundOf runs review passes until the intent's round want has its first
// attempt, and returns it.
func (e *env) roundOf(name string, want int32) v1alpha1.PreviewVerification {
	e.t.Helper()
	var out v1alpha1.PreviewVerification
	e.reviewUntil(name, func() bool {
		for _, pv := range e.verifications(name) {
			if pv.Spec.Round == want && pv.Spec.Attempt == 1 {
				out = pv
				return true
			}
		}
		return false
	})
	return out
}

// acceptManual records an approver's acceptance of the manual outcome at
// the intent's heads.
func (e *env) acceptManual(name string) {
	e.t.Helper()
	e.review(name, 3)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{ID: oManual,
			By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99,
			HeadSetDigest: in.Status.Verification.HeadSetDigest})
	})
}

// request has an approver comment body on surface ("issue" or "pull
// request") and settles it; it returns maul's replies to it.
func (e *env) request(name, surface, body string) []string {
	e.t.Helper()
	e.clock.Advance(verifyInterval)
	id := e.commandOn(name, surface, body)
	e.settleActions(name)
	if surface != "issue" {
		return e.prReplies(e.get(name).Status.PullRequests[0].Number, id)
	}
	var out []string
	for _, c := range e.gh.ownComments() {
		if strings.Contains(markerOf(c.Body), "comment-"+itoa(id)) {
			out = append(out, c.Body)
		}
	}
	return out
}

// stageCount is the number of intent name's runs of stage.
func (e *env) stageCount(name string, stage v1alpha1.IntentStage) int {
	return len(e.runsOf(name, stage))
}

// TestRoundWithNoEvaluatorReportIsNoModelRound: a full round judged
// with no evaluator report (its preview failed before the evaluator ran)
// had no model look at anything, so it is no model round: a later round at
// its heads is full, never a replay trusting a judgement no model made.
func TestRoundWithNoEvaluatorReportIsNoModelRound(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationEnvFailed, "", false)
	got := e.pvNamed(pv.Name)
	got.Status.Reason = v1alpha1.VerificationReasonComponentCrashed
	got.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent, Ready: true,
		CrashedDuring: true, Restarts: 1}}
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || len(v.Outcomes) == 0 {
		t.Fatalf("verification = %s %+v, want round 1 judged Unconfirmed", v.State, v.Outcomes)
	}
	if n := e.stageCount(name, v1alpha1.IntentStageEvaluate); n != 0 {
		t.Fatalf("evaluate runs = %d, want none: the preview failed first", n)
	}
	if v.ModelRound != nil {
		t.Errorf("model round %+v recorded for a round no evaluator reported on", v.ModelRound)
	}
}
