// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/agentrun"
)

// A defect found in round 1 (S1 bug 7, the post-demo decision): the
// evaluator reports fail and proposes a check that shows it; tools confirms
// it red by assertion; the reviewer answers the red question; maul stores
// it expecting to fail and judges the outcome failed, which starts a
// verify-fix round.

// redReport is the evaluator's report that the version outcome fails,
// proposing the check that shows it.
const redReport = `---
verdicts:
  - outcome: version-shown
    verdict: fail
    ledger: [4, 10]
  - outcome: still-healthy
    verdict: unsure
    ledger: []
proposed:
  - id: version-shown
    outcome: version-shown
    file: "/work/scratch/checks/version-shown.json"
    expect: fail
---
The version endpoint answers with the wrong note.
`

// redCheck is versionCheck saved red.
func redCheck(t *testing.T) v1alpha1.SavedCheck {
	c := savedCheck(t, versionCheck)
	c.Expect = v1alpha1.CheckExpectFail
	return c
}

// confirmRed plays preview-controller collecting pv with red confirmed as
// expected to fail, its confirmation run as given.
func (e *env) confirmRed(pvName string, red v1alpha1.SavedCheck, run v1alpha1.CheckRun, confirmed bool) {
	e.t.Helper()
	e.playTools(pvName, v1alpha1.VerificationObserved, toolPodIP, true)
	pv := e.pvNamed(pvName)
	pv.Status.Confirmed = nil
	if confirmed {
		pv.Status.Confirmed = []v1alpha1.SavedCheck{red}
	}
	pv.Status.Confirmations = []v1alpha1.CheckRun{run}
	if err := e.c.Update(context.Background(), pv); err != nil {
		e.t.Fatal(err)
	}
}

// redRun is the red check's confirmation red by assertion at its second
// step.
func redRun(red v1alpha1.SavedCheck) v1alpha1.CheckRun {
	return v1alpha1.CheckRun{CheckDigest: red.Digest, Result: v1alpha1.CheckRunFail, Token: "cf-tok", Seq: 10,
		DryRunSeq: 4, Expect: v1alpha1.CheckExpectFail, AssertionStep: 2}
}

// redRound drives intent name's first round to its collection, the
// evaluator reporting redReport and the red check confirmed as run shows.
func (e *env) redRound(name string, red v1alpha1.SavedCheck, run v1alpha1.CheckRun, confirmed bool) {
	e.t.Helper()
	e.evaluateReporting(redReport)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.confirmRed(pv.Name, red, run, confirmed)
}

// TestRedCheckFailsTheRoundAndStartsAFix: confirmed red by assertion,
// reviewed with the red question (told the step that failed) and answered
// proves, the check is stored expecting to fail, its review cached as the
// red question's; the outcome is failed, reproduced, and a verify-fix round
// starts.
func TestRedCheckFailsTheRoundAndStartsAFix(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	red := redCheck(t)
	r := &fakeReviewer{}
	e.reviewChecksWith(r)
	e.redRound(name, red, redRun(red), true)
	e.settleReviews(name)

	if len(r.handoffs) != 1 || len(r.handoffs[0].Reviews) != 1 {
		t.Fatalf("handoffs = %+v, want the red check reviewed", r.handoffs)
	}
	if it := r.handoffs[0].Reviews[0]; it.Expect != "fail" || it.FailedStep != 2 || it.CheckDigest != red.Digest {
		t.Errorf("review = %+v, want the red question at step 2", it)
	}
	if _, stored := e.checkStore(name); len(stored) != 1 || stored[0] != red {
		t.Fatalf("store = %+v, want the red check, expecting to fail", stored)
	}
	acc := e.get(name).Status.Acceptance
	if i := slices.IndexFunc(acc.Reviews, func(rv v1alpha1.CheckReviewRecord) bool {
		return rv.CheckDigest == red.Digest
	}); i < 0 || acc.Reviews[i].Expect != v1alpha1.CheckExpectFail || acc.Reviews[i].Answer != v1alpha1.ReviewProves {
		t.Errorf("reviews = %+v, want the red question's proves", acc.Reviews)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationFailing || v.Failed != 1 || v.Outcomes[0].Result != v1alpha1.OutcomeFailed ||
		v.Outcomes[0].Reason != acceptance.ReasonReproduced || v.Outcomes[0].Basis != red.Digest {
		t.Fatalf("verification = %s %+v, want version-shown failed on the red check", v.State, v.Outcomes)
	}
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
	if run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]; run.Spec.Trigger != v1alpha1.IntentRunTriggerVerify {
		t.Errorf("revise run = %+v, want the verify fix", run.Spec)
	}
}

// TestRedCheckGuards: each guard alone keeps the outcome unsure, never
// failed, and starts no fix: a reviewer's no (nothing stored), a red that is
// no assertion (never a candidate, never reviewed), and a check proposed red
// whose own confirmation ran green (flaky: never stored).
func TestRedCheckGuards(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    v1alpha1.ReviewAnswer
		run       func(v1alpha1.SavedCheck) v1alpha1.CheckRun
		confirmed bool
		reviewed  bool
	}{
		{"the reviewer says no", v1alpha1.ReviewDoesNotProve, redRun, true, true},
		{"red, but not by assertion", v1alpha1.ReviewProves, func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
			r := redRun(c)
			r.AssertionStep = 0
			return r
		}, true, false},
		{"its confirmation ran green", v1alpha1.ReviewProves, func(c v1alpha1.SavedCheck) v1alpha1.CheckRun {
			r := redRun(c)
			r.Result, r.AssertionStep = v1alpha1.CheckRunPass, 0
			return r
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			red := redCheck(t)
			r := &fakeReviewer{answers: map[string]v1alpha1.ReviewAnswer{red.Digest: tc.answer}}
			e.reviewChecksWith(r)
			e.redRound(name, red, tc.run(red), tc.confirmed)
			e.settleReviews(name)
			if reviewed := len(r.handoffs) > 0; reviewed != tc.reviewed {
				t.Errorf("reviewed %v, want %v", reviewed, tc.reviewed)
			}
			if _, stored := e.checkStore(name); len(stored) != 0 {
				t.Errorf("store = %+v, want nothing", stored)
			}
			v := e.verification(name)
			if v.State != v1alpha1.VerificationUnconfirmed || v.Failed != 0 ||
				v.Outcomes[0].Result != v1alpha1.OutcomeUnsure {
				t.Fatalf("verification = %s %+v, want version-shown unsure", v.State, v.Outcomes)
			}
			e.review(name, 3)
			if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
				t.Errorf("revise runs = %d, want no fix", n)
			}
		})
	}
}

// TestCheckReviewDefaultTurnsLeaveARepair (S1 bug 8): each check review's
// default turns keep enough back for agent-runner to repair its report.
func TestCheckReviewDefaultTurnsLeaveARepair(t *testing.T) {
	if !agentrun.Repairable(int(DefaultCheckReviewMaxTurns)) {
		t.Errorf("DefaultCheckReviewMaxTurns %d leaves a review no repair", DefaultCheckReviewMaxTurns)
	}
}

// TestUnreviewedCheckIsUnclearAlone (S1 bug 8): one review that ended
// without an accepted report leaves only its check unstored and its answer
// uncached; the other checks of the round are stored as reviewed.
func TestUnreviewedCheckIsUnclearAlone(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	version, health, again := savedCheck(t, versionCheck), savedCheck(t, healthCheck), savedCheck(t, healthCheck2)
	r := &fakeReviewer{unreviewed: map[string]bool{health.Digest: true}}
	e.reviewChecksWith(r)
	e.evaluatedRound(name, version, health, again)
	e.settleReviews(name)

	if _, stored := e.checkStore(name); !slices.Equal(digestsOf(stored), []string{version.Digest, again.Digest}) {
		t.Fatalf("store = %v, want every check but the unreviewed one", digestsOf(stored))
	}
	for _, rv := range e.get(name).Status.Acceptance.Reviews {
		if rv.CheckDigest == health.Digest {
			t.Errorf("the unreviewed check's answer was cached: %+v", rv)
		}
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageCheckReview)); n != 1 {
		t.Errorf("checkreview runs = %d, want the round's one", n)
	}
	if v := e.verification(name); v.State == v1alpha1.VerificationEvaluating {
		t.Errorf("state = %s, want the round judged", v.State)
	}
}
