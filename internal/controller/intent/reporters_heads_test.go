// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestEvaluatorSwitchedOffWithOutcomesIsNoSuccess (review CX-1): the agent
// evaluator switched off (the documented rollback) on a slot Project that
// keeps verification on, with an approved plan's outcomes on record. Nothing
// runs them any more, so the intent is Off and its maul/acceptance the
// switched-off error, never "accepted by hand" in green for the one manual
// outcome an approver accepted.
func TestEvaluatorSwitchedOffWithOutcomesIsNoSuccess(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("verification = %+v, want Evaluating", v)
	}
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.review(name, 1)
	if st := e.latestAcceptance(head); st.State != statusPending {
		t.Fatalf("maul/acceptance while evaluating = %+v, want pending", st)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		// As an approver's /maul accept records it (recordAcceptance).
		routeRecords(in, judgeRoute{acceptManual: true}, e.clock.Now())
		countManual(in.Status.Verification, in.Status.Acceptance)
	})

	e.evaluating(false)
	e.review(name, 4)
	if v := e.verification(name); v.State != v1alpha1.VerificationOff {
		t.Errorf("verification = %s, want Off: nothing evaluates the outcomes", v.State)
	}
	for _, st := range e.acceptanceStatuses(head) {
		if st.State == statusSuccess {
			t.Fatalf("maul/acceptance %+v posted with the evaluator off", st)
		}
	}
	if st := e.latestAcceptance(head); st.State != statusError || st.Description != switchedOff.description {
		t.Fatalf("maul/acceptance once the evaluator is off = %+v, want %+v", st, switchedOff)
	}
}

// TestEvaluatorOffIsVerificationOff (S4, owner decision 2): the rollback as
// serve renders it now, the evaluator off taking verification off with it,
// over an outcomes round in flight with its preview link held. Nothing is
// verified or reported as passing: maul/acceptance is the switched-off
// error, never a success; the status comment says verification is off and
// links the preview at once; no new PreviewVerification is made.
func TestEvaluatorOffIsVerificationOff(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("verification = %+v, want Evaluating", v)
	}
	if strings.Contains(e.statusBody(), "("+"https://"+previewHostOf(name)+")") {
		t.Fatalf("the gated link is shown while evaluating:\n%s", e.statusBody())
	}
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.review(name, 1)
	if st := e.latestAcceptance(head); st.State != statusPending {
		t.Fatalf("maul/acceptance while evaluating = %+v, want pending", st)
	}
	pvs := len(e.verifications(name))

	e.evaluating(false)
	e.verifying(false, true)
	e.review(name, 4)
	for _, st := range e.acceptanceStatuses(head) {
		if st.State == statusSuccess {
			t.Fatalf("maul/acceptance %+v posted with the evaluator off", st)
		}
	}
	if st := e.latestAcceptance(head); st.State != statusError || st.Description != switchedOff.description {
		t.Fatalf("maul/acceptance once the evaluator is off = %+v, want %+v", st, switchedOff)
	}
	body := e.statusBody()
	for _, want := range []string{"**Verification:** off", "`--intent-evaluator`",
		"(https://" + previewHostOf(name) + ")"} {
		if !strings.Contains(body, want) {
			t.Errorf("status comment lacks %q:\n%s", want, body)
		}
	}
	if n := len(e.verifications(name)); n > pvs {
		t.Errorf("verifications = %d after the evaluator went off, want at most %d", n, pvs)
	}
}

// TestEvaluationStatusNotDeclaredNeedsEveryOutcomeManual (review CX-1):
// "accepted by hand" is a success only for a plan whose every outcome is
// manual and accepted at the heads, whatever counts the verification
// carries: NotDeclared over outcomes maul evaluates, or with counts never
// made, is no success.
func TestEvaluationStatusNotDeclaredNeedsEveryOutcomeManual(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	p := e.prVerifyPass(name)
	v := p.in.Status.Verification
	v.State, v.ManualOpen, v.ManualAccepted = v1alpha1.VerificationNotDeclared, 0, 0
	if st, ok := p.evaluationStatus(); ok && st.state == statusSuccess {
		t.Errorf("NotDeclared over evaluable outcomes = %+v, want no success", st)
	}
	acc := p.in.Status.Acceptance
	for i := range acc.Outcomes {
		acc.Outcomes[i].Manual = true
	}
	if st, ok := p.evaluationStatus(); ok && st.state == statusSuccess {
		t.Errorf("NotDeclared, every outcome manual, none accepted = %+v, want no success", st)
	}
}

// TestReportersRefuseAJudgementAtOtherHeads (review SM-1): an intent judged
// ready at head A, then a push of B recorded on the pull request alone (the
// verification's head set is synced only at the next poll). Until a round
// is about B, nothing reports A's judgement on B: no success status, no
// "ready" hand-off.
func TestReportersRefuseAJudgementAtOtherHeads(t *testing.T) {
	check := savedCheck(t, versionCheck)
	pass := func(id string) acceptance.OutcomeResult {
		return judged(id, acceptance.ResultPassed, acceptance.ReasonConfirmed, check.Digest)
	}
	tc := judgeRoute{version: pass(oVersion), healthy: pass(oHealthy), acceptManual: true}
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	p := e.prVerifyPass(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		routeRecords(in, tc, e.clock.Now())
		applyJudgement(in, routeJudgement(tc, check), time.Now())
		p.releaseIfDue(in)
	})
	p = e.prVerifyPass(name)
	if st, _, ok := p.acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Fatalf("maul/acceptance at the judged heads = %+v, want success", st)
	}
	if h := p.handoffView(); h == nil || h.State != templates.HandoffReady {
		t.Fatalf("hand-off at the judged heads = %+v, want ready", h)
	}

	moved := strings.Repeat("b", 40)
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.PullRequests[0].HeadSHA = moved })
	p = e.prVerifyPass(name)
	if st, _, ok := p.acceptanceStatus(); ok && st.state != statusPending {
		t.Errorf("maul/acceptance with the head moved = %+v, want pending or none", st)
	}
	if _, err := p.syncAcceptanceStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, st := range e.acceptanceStatuses(moved) {
		if st.State == statusSuccess {
			t.Errorf("maul/acceptance %+v posted on a head nothing judged", st)
		}
	}
	if h := p.handoffView(); h != nil && h.State == templates.HandoffReady {
		t.Errorf("hand-off with the head moved = %+v, want not ready", h)
	}
}

// movedHeadsChecks: with the pull request's head moved past the judged
// heads (recorded on the pull request alone), no comment presents the old
// round's result as current: the status comment's progress and every
// verification section read "checking the new commits" and carry no
// result, and the issue's verification comment is neither posted nor
// edited while the heads differ (review SM-1). judged is the word the old
// result is shown by.
func (e *env) movedHeadsChecks(name, judged string) {
	e.t.Helper()
	t := e.t
	round := e.verification(name).Round
	before := e.verifyComments(name, round)
	moved := strings.Repeat("b", 40)
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.PullRequests[0].HeadSHA = moved })
	p := e.prVerifyPass(name)
	if prog := p.verifyProgress(); prog == nil || prog.State != templates.VerifyHeadsMoved || prog.Total != 0 {
		t.Errorf("progress with the head moved = %+v, want %s and no counts", prog, templates.VerifyHeadsMoved)
	}
	for _, pr := range []*v1alpha1.IntentPullRequest{nil, &p.in.Status.PullRequests[0]} {
		s := p.verifySection(context.Background(), pr)
		if s == nil {
			t.Fatal("no verification section")
		}
		if s.Progress.State != templates.VerifyHeadsMoved || len(s.Outcomes) != 0 ||
			len(s.Logs) != 0 || s.Suspected != nil || s.NoDefect != nil {
			t.Errorf("section with the head moved = %+v, want the moved line alone", s)
		}
		body, err := templates.RenderVerifyComment(templates.VerifyComment{Namespace: testNS, Intent: name,
			Round: round, Attempt: 1, Section: *s})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, judged) || !strings.Contains(body, "checking the new commits") {
			t.Errorf("section with the head moved shows the old judgement:\n%s", body)
		}
	}
	p.syncVerifyComment(context.Background())
	after := e.verifyComments(name, round)
	if len(after) != len(before) {
		t.Fatalf("verification comments %d -> %d: written while the heads moved", len(before), len(after))
	}
	for i := range after {
		if after[i].Body != before[i].Body {
			t.Errorf("verification comment edited while the heads moved:\n%s", after[i].Body)
		}
	}
}

// TestCommentsRefuseAPassAtOtherHeads (review SM-1): an evaluation judged
// passed at head A, then a push of B recorded on the pull request alone.
// Until a round is about B, no comment shows A's pass.
func TestCommentsRefuseAPassAtOtherHeads(t *testing.T) {
	check := savedCheck(t, versionCheck)
	pass := func(id string) acceptance.OutcomeResult {
		return judged(id, acceptance.ResultPassed, acceptance.ReasonConfirmed, check.Digest)
	}
	tc := judgeRoute{version: pass(oVersion), healthy: pass(oHealthy), acceptManual: true}
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	p := e.prVerifyPass(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		routeRecords(in, tc, e.clock.Now())
		applyJudgement(in, routeJudgement(tc, check), time.Now())
		p.releaseIfDue(in)
	})
	if prog := e.prVerifyPass(name).verifyProgress(); prog == nil || prog.State != templates.VerifyPassed {
		t.Fatalf("progress at the judged heads = %+v, want passed", prog)
	}
	e.movedHeadsChecks(name, "passed")
}

// TestCommentsRefuseAFailureAtOtherHeads (review SM-1): an evaluation
// judged failing at head A, then a push of B recorded on the pull request
// alone: no comment shows A's failure as B's.
func TestCommentsRefuseAFailureAtOtherHeads(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	if prog := e.prVerifyPass(name).verifyProgress(); prog == nil || prog.State != templates.VerifyFailing {
		t.Fatalf("progress at the judged heads = %+v, want failing", prog)
	}
	e.movedHeadsChecks(name, "failed")
}
