// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// Round helpers shared by the evaluation tests, end to end through the
// intent reconciler and the run scheduler, preview-controller played.

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

// request has an approver comment body on the intent issue and settles
// it; it returns maul's replies to it.
func (e *env) request(name, body string) []string {
	e.t.Helper()
	e.clock.Advance(verifyInterval)
	id := e.commandOn(name, "issue", body)
	e.settleActions(name)
	var out []string
	for _, c := range e.gh.ownComments() {
		if strings.Contains(markerOf(c.Body), "comment-"+itoa(id)) {
			out = append(out, c.Body)
		}
	}
	return out
}

// settleReviews runs review passes and the run scheduler until the round
// is judged, at most a dozen times each.
func (e *env) settleReviews(name string) {
	e.t.Helper()
	for range 12 {
		if e.verification(name).State != v1alpha1.VerificationEvaluating {
			return
		}
		e.review(name, 1)
		e.runRuns()
		e.runRuns()
	}
}

// commandOn posts body as the approver on surface ("issue" or "pull
// request") of intent name, returning the comment's id.
func (e *env) commandOn(name, surface, body string) int64 {
	if surface == "issue" {
		return e.gh.comment(approver, body)
	}
	return e.prCommand(e.get(name).Status.PullRequests[0].Number, approver, body)
}

// legacyBranch is the branch of an intent never replanned in review, spelled
// out: maul-intent/<intent>, whatever its plan revision.
func legacyBranch(name string) string { return v1alpha1.IntentBranchPrefix + name }
