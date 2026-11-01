// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The fifth review of the round-1 fallback: a suspected defect is confirmed
// only by a /maul verify fix created after maul recorded it, stays visible
// and confirmable beside other failures, and a check saved red that has
// cleared is a saved green check.

// suspectedRound is an evaluator intent whose first round found a defect
// on its red check alone: suspected, unconfirmed.
func suspectedRound(t *testing.T) (*env, string) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	red := redCheck(t)
	e.reviewChecksWith(&fakeReviewer{})
	e.redRound(name, red, redRun(red), true)
	e.settleReviews(name)
	v := e.verification(name)
	if v.Suspected == nil || v.Suspected.Confirmed != nil || v.Suspected.RecordedAt.IsZero() {
		t.Fatalf("suspected = %+v, want one recorded, unconfirmed", v.Suspected)
	}
	return e, name
}

// backdate sets comment id's creation time, on the issue or a pull request,
// to at: a comment written before the poll that reads it.
func (e *env) backdate(id int64, at time.Time) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	for _, is := range e.gh.issues {
		for _, c := range is.comments {
			if c.ID == id {
				c.CreatedAt, c.UpdatedAt = at, at
			}
		}
	}
	for _, cs := range e.gh.prComments {
		for _, c := range cs {
			if c.ID == id {
				c.CreatedAt, c.UpdatedAt = at, at
			}
		}
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

// settleCommands runs passes enough to settle the issue's and the pull
// requests' commands.
func (e *env) settleCommands(name string) {
	e.t.Helper()
	e.settleActions(name)
	e.review(name, 2)
}

// TestSuspectedConfirmedOnlyAfterItsRecord (fifth review, 1): a /maul
// verify fix created before the suspected defect was recorded never
// confirms it, on the issue or the pull request: it keeps its earlier
// meaning (a request that re-arms automatic fixes), and no fix round starts
// on the suspected defect. One created after it confirms it.
func TestSuspectedConfirmedOnlyAfterItsRecord(t *testing.T) {
	for _, surface := range []string{"issue", "pull request"} {
		t.Run(surface+", written before the record", func(t *testing.T) {
			e, name := suspectedRound(t)
			at := e.verification(name).Suspected.RecordedAt.Time
			id := e.commandOn(name, surface, "/maul verify fix")
			e.backdate(id, at.Add(-time.Minute))
			e.settleCommands(name)
			v := e.verification(name)
			if v.Suspected != nil && v.Suspected.Confirmed != nil {
				t.Fatalf("suspected = %+v: a comment written before the evidence confirmed it", v.Suspected)
			}
			if v.AutoFix.Rearm == nil || v.AutoFix.Rearm.ByID != actorOf(approver).ID {
				t.Errorf("auto-fix = %+v, want the earlier meaning: re-armed by the approver", v.AutoFix)
			}
			if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
				t.Errorf("revise runs = %d, want no fix of the suspected defect", n)
			}
		})
		t.Run(surface+", written after the record", func(t *testing.T) {
			e, name := suspectedRound(t)
			e.clock.Advance(time.Minute)
			e.commandOn(name, surface, "/maul verify fix")
			e.settleCommands(name)
			if s := e.verification(name).Suspected; s == nil || s.Confirmed == nil ||
				s.Confirmed.ByID != actorOf(approver).ID {
				t.Fatalf("suspected = %+v, want confirmed by the approver", s)
			}
		})
	}
}

// TestSuspectedLeftByOtherCommands: /maul verify and /maul verify fix
// --reset are requests, never a confirmation.
func TestSuspectedLeftByOtherCommands(t *testing.T) {
	for _, body := range []string{"/maul verify", "/maul verify fix --reset"} {
		t.Run(body, func(t *testing.T) {
			e, name := suspectedRound(t)
			e.clock.Advance(time.Minute)
			e.gh.comment(approver, body)
			e.settleCommands(name)
			if s := e.verification(name).Suspected; s != nil && s.Confirmed != nil {
				t.Fatalf("suspected = %+v: %s confirmed it", s, body)
			}
		})
	}
}

// TestSuspectedNotConfirmedOnceTheHeadsMoved: a suspected defect found at
// heads that are no longer the pull requests' is not confirmed.
func TestSuspectedNotConfirmedOnceTheHeadsMoved(t *testing.T) {
	e, name := suspectedRound(t)
	was := e.verification(name).Suspected.HeadSetDigest
	e.pushHead(e.prNumber(name), strings.Repeat("e", 40))
	e.review(name, 3)
	if v := e.verification(name); v.HeadSetDigest == was {
		t.Fatalf("heads %s: the push did not move them", v.HeadSetDigest)
	}
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul verify fix")
	e.settleCommands(name)
	if s := e.verification(name).Suspected; s != nil && s.Confirmed != nil {
		t.Fatalf("suspected = %+v: confirmed at heads that moved", s)
	}
}

// TestSuspectedReleasedByTheHandoffLimit: a held suspected defect does not
// hold the link past the hand-off's time limit.
func TestSuspectedReleasedByTheHandoffLimit(t *testing.T) {
	e, name := suspectedRound(t)
	if h := e.verification(name).Handoff; h == nil || h.Released {
		t.Fatalf("hand-off = %+v, want held", h)
	}
	e.clock.Advance(time.Hour + time.Minute) // handoffMax, an hour here
	e.review(name, 2)
	if h := e.verification(name).Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonOutOfTime {
		t.Fatalf("hand-off = %+v, want released, out of time", h)
	}
}

// bothReport: the version outcome fails on the red check, and the health
// outcome on its saved green check.
const bothReport = `---
verdicts:
  - outcome: version-shown
    verdict: fail
    ledger: [4, 10]
  - outcome: still-healthy
    verdict: fail
    ledger: [5]
proposed:
  - id: version-shown
    outcome: version-shown
    file: "/work/scratch/checks/version-shown.json"
    expect: fail
---
The version endpoint answers with the wrong note, and health is down.
`

// TestSuspectedBesideAGreenFailure (fifth review, 5): a suspected defect
// beside another outcome failing on a saved green check stays visible on
// the issue, and an approver's /maul verify fix after it confirms it; the
// fix round then carries both failures.
func TestSuspectedBesideAGreenFailure(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	health := savedCheck(t, healthCheck)
	e.storeChecks(name, health)
	// Automatic fixes halted, so the green failure waits for a person too.
	e.gh.comment(approver, "/maul verify stop")
	e.settleActions(name)
	red := redCheck(t)
	e.reviewChecksWith(&fakeReviewer{})
	e.evaluateReporting(bothReport)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayed(pv.Name, v1alpha1.CheckRun{CheckDigest: health.Digest, Result: v1alpha1.CheckRunFail, Token: "t1",
		Seq: 2})
	e.confirmRed(pv.Name, red, redRun(red), true)
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationFailing || v.Suspected == nil || v.Suspected.Confirmed != nil {
		t.Fatalf("verification = %s, suspected %+v; want Failing beside an unconfirmed suspected defect", v.State,
			v.Suspected)
	}
	e.review(name, 2)
	evidence := ""
	for _, c := range e.verifyComments(name, v.Round) {
		evidence += c.Body
	}
	for _, want := range []string{"maul believes", "a person should confirm", "POST /notes"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("the verification comment lacks %q beside a failing outcome:\n%s", want, evidence)
		}
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
		t.Fatalf("revise runs = %d with automatic fixes halted", n)
	}

	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	if s := e.verification(name).Suspected; s == nil || s.Confirmed == nil {
		t.Fatalf("suspected = %+v, want confirmed beside the failing outcome", s)
	}
	all := e.fixInput(name)
	for _, want := range []string{"`version-shown`", "`still-healthy`"} {
		if !strings.Contains(all, want) {
			t.Errorf("the fix round's input lacks %s:\n%s", want, all)
		}
	}
}

// TestConfirmedDefectReported (fifth review, 6): once a person confirmed
// the defect, the commit status and the status comment say so and that a
// fix is running, never an automatic fix's count or "0 failing".
func TestConfirmedDefectReported(t *testing.T) {
	e, name := suspectedRound(t)
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	e.reviewUntil(name, func() bool { return len(e.runsOf(name, v1alpha1.IntentStageRevise)) > 0 })
	e.review(name, 1)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	st := e.latestAcceptance(head)
	if !strings.Contains(st.Description, "a person confirmed the defect") ||
		strings.Contains(st.Description, "automatic fix") || strings.Contains(st.Description, "0 failing") {
		t.Errorf("maul/acceptance = %q, want a person's confirmation and a fix", st.Description)
	}
	body := e.statusBody()
	if !strings.Contains(body, "a person confirmed the defect") || strings.Contains(body, "automatic fix running") {
		t.Errorf("status comment does not say a person confirmed the defect:\n%s", body)
	}
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
