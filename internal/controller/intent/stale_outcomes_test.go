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

// confirmedTwo is a judgement confirming two of outcomesPlan's three
// outcomes, the manual one left to a human.
var confirmedTwo = acceptance.Decision{Env: acceptance.EnvOK, PerOutcome: []acceptance.OutcomeResult{
	{ID: oVersion, Result: acceptance.ResultPassed, Reason: acceptance.ReasonConfirmed},
	{ID: oHealthy, Result: acceptance.ResultPassed, Reason: acceptance.ReasonConfirmed},
	{ID: oManual, Result: acceptance.ResultUnsure, Reason: acceptance.ReasonManual}}}

// checkNothingConfirmed: no view of name's verification (the progress, the
// verification comment's rows, the commit status) presents an outcome as
// confirmed: the round it is about was never judged (S2-B3).
func (e *env) checkNothingConfirmed(name string) {
	e.t.Helper()
	t := e.t
	p := e.prVerifyPass(name)
	v := p.in.Status.Verification
	if prog := p.verifyProgress(); prog != nil && prog.Passing != 0 {
		t.Errorf("progress = %+v: confirmed outcomes from an earlier round", prog)
	}
	section := p.verifySection(context.Background(), nil)
	if section == nil {
		t.Fatal("no verification section")
	}
	for _, o := range section.Outcomes {
		if o.Result == templates.OutcomeResultPassed || o.Check != "" {
			t.Errorf("outcome %+v shown from an earlier round", o)
		}
	}
	body, err := templates.RenderVerifyComment(templates.VerifyComment{Namespace: testNS, Intent: name,
		Round: v.Round, Attempt: v.Attempt, Section: *section})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "confirmed") && !strings.Contains(body, "0/3 outcomes confirmed") ||
		strings.Contains(body, "2/3") {
		t.Errorf("the verification comment presents an earlier round's results:\n%s", body)
	}
	if st, _, ok := p.acceptanceStatus(); ok && (st.state == statusSuccess || strings.Contains(st.description, "2/3")) {
		t.Errorf("commit status = %+v: an earlier round's results", st)
	}
	if outcomesReady(p.in.Status.Acceptance, v) {
		t.Error("an unjudged round is ready on an earlier round's results")
	}
}

// TestUnjudgedRoundAtMovedHeadsShowsNoEarlierResults (S2-B3, bbcount-63
// round 4): a judged agents round, then a push, then a round at the new
// heads every attempt of which the tool pod cannot build: the verification
// is Unconfirmed for the build, and nothing shows the earlier round's
// outcomes as confirmed at heads maul never checked.
func TestUnjudgedRoundAtMovedHeadsShowsNoEarlierResults(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		applyJudgement(in, roundJudgement{decision: confirmedTwo}, time.Now())
	})
	if p := e.prVerifyPass(name); confirmedCount(p.in.Status.Acceptance, p.in.Status.Verification) != 2 {
		t.Fatalf("verification = %+v, want two confirmed", p.in.Status.Verification)
	}
	e.pushHead(e.get(name).Status.PullRequests[0].Number, strings.Repeat("f", 40))
	e.reviewReady(name, 8)
	for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
		v := e.verification(name)
		if v.Ref == nil || v.Attempt != attempt || v.Round != 2 {
			t.Fatalf("attempt %d: verification = %+v", attempt, v)
		}
		e.settleTools(v.Ref.Name, v1alpha1.VerificationInfra, v1alpha1.VerificationReasonToolsBuildFailed)
		e.reviewReady(name, 3)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationUnconfirmed ||
		v.UnavailableReason != v1alpha1.VerificationUnavailableToolsBuildFailed {
		t.Fatalf("verification = %+v, want Unconfirmed for the build", v)
	}
	e.checkNothingConfirmed(name)
}

// TestUnjudgedRoundAtTheSameHeadsShowsNoEarlierResults (S2-B3, the slot
// path): a judged slot round, then a later round at the same heads whose
// every attempt maul's own machinery failed: the verification is
// Unavailable, and its comment does not list the earlier round's outcomes
// as confirmed under the new round's number.
func TestUnjudgedRoundAtTheSameHeadsShowsNoEarlierResults(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		applyJudgement(in, roundJudgement{decision: confirmedTwo}, time.Now())
	})
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.Round++
		v.Attempt = v1alpha1.MaxVerificationAttempts
	})
	if err := e.prVerifyPass(name).unjudged(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable {
		t.Fatalf("verification = %+v, want Unavailable", v)
	}
	e.checkNothingConfirmed(name)
}

// TestUnjudgedReplayRoundIsNotStillChecking (S2-B3 review): a judgement
// that disputed a check asks for one replay round at the same heads; when
// that round settles unjudged (the tool pod cannot build the head), the
// earlier judgement's disputes are not this round's: no further replay is
// due, so the commit status never says maul is still checking a round
// nothing will start.
func TestUnjudgedReplayRoundIsNotStillChecking(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	disputed := confirmedTwo
	disputed.Retire, disputed.ReplayRound = []string{"sha256:" + strings.Repeat("d", 64)}, true
	e.setStatus(name, func(in *v1alpha1.Intent) {
		applyJudgement(in, roundJudgement{decision: disputed}, time.Now())
	})
	if p := e.prVerifyPass(name); !p.replayDue(p.in.Status.Verification) {
		t.Fatalf("verification = %+v, want its replay round due", p.in.Status.Verification)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.Round++
		v.Attempt = v1alpha1.MaxVerificationAttempts
		v.State, v.UnavailableReason = v1alpha1.VerificationUnconfirmed,
			v1alpha1.VerificationUnavailableToolsBuildFailed
	})
	p := e.prVerifyPass(name)
	if p.replayDue(p.in.Status.Verification) {
		t.Error("an unjudged replay round still asks for a replay of the earlier judgement")
	}
	if st, _, ok := p.acceptanceStatus(); ok && st.state == statusPending {
		t.Errorf("commit status = %+v: still checking a round nothing will start", st)
	}
	e.checkNothingConfirmed(name)
}
