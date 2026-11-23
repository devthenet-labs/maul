// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The fifth review of the round-1 fallback: a suspected defect is confirmed
// only by a /maul verify fix created after maul recorded it, stays visible
// and confirmable beside other failures, and a check saved red that has
// cleared is a saved green check.

// commandOn posts body as the approver on surface ("issue" or "pull
// request") of intent name, returning the comment's id.
func (e *env) commandOn(name, surface, body string) int64 {
	if surface == "issue" {
		return e.gh.comment(approver, body)
	}
	return e.prCommand(e.get(name).Status.PullRequests[0].Number, approver, body)
}

// TestClearedRedCheckIsGreen (fifth review, 4): a check saved red that
// cleared in an earlier round and is reviewed with the green question is a
// saved green check: its later regression fails the outcome and starts the
// automatic fix, with no person's confirmation.
func TestClearedRedCheckIsGreen(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	red := redCheck(t)
	e.storeChecks(name, red) // reviewed proves with the green question
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.RedCleared = []v1alpha1.RedClearance{{Digest: red.Digest, Verification: name + "-v0-a1"}}
	})
	e.evaluateReporting(failingReport)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail, Token: "t1",
		Seq: 2})
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationFailing || v.Failed != 1 || v.Suspected != nil {
		t.Fatalf("verification = %s %+v, suspected %+v; want Failing on the cleared check, nothing suspected",
			v.State, v.Outcomes, v.Suspected)
	}
	if all := e.fixInput(name); !strings.Contains(all, "`version-shown`") {
		t.Errorf("the fix round's input lacks the regression:\n%s", all)
	}
}
