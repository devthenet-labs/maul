// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
)

// G-B-LIB-1 (defect gate 2): a look-around anomaly is a fact about the
// commits it was found at. Round 1 at heads H reports one and is
// Unconfirmed; round 2 at the same heads, whose evaluator reports nothing
// (it replayed the green saved checks and stopped), must not pass: the
// anomaly is carried, the round stays Unconfirmed, maul/acceptance is no
// success and the model round is not clear (so no replay round follows).
// Only moved heads, or an approver's acceptance, let a round pass.

// passingJudgement is a judgement every outcome of which passed, the
// manual one accepted, in a clean environment: Passed on its own.
func passingJudgement(t *testing.T, anomalies ...v1alpha1.AnomalyRecord) roundJudgement {
	t.Helper()
	check := savedCheck(t, versionCheck)
	pass := func(id string) acceptance.OutcomeResult {
		return judged(id, acceptance.ResultPassed, acceptance.ReasonConfirmed, check.Digest)
	}
	d := acceptance.Decision{PerOutcome: []acceptance.OutcomeResult{pass(oVersion), pass(oHealthy),
		judged(oManual, acceptance.ResultUnsure, acceptance.ReasonManual, "")}, Env: httpstep.EnvOK}
	return roundJudgement{decision: d, checks: []v1alpha1.SavedCheck{check}, anomalies: anomalies,
		evaluation: &report.Evaluation{}, clear: len(anomalies) == 0}
}

// heldAnomaly is round 1's look-around anomaly, naming the defect.
var heldAnomaly = v1alpha1.AnomalyRecord{Summary: "Likely defect in synth/shift.go: the hat track rotates the " +
	"wrong way", Route: "synth.Shift", Ledger: []int32{4}}

// judgeAt writes j as round's judgement at headSet on intent name, the
// manual outcome accepted there, and returns the verification.
func (e *env) judgeAt(name string, round int32, headSet string, j roundJudgement) *v1alpha1.IntentVerification {
	e.t.Helper()
	p := e.prVerifyPass(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.Round, v.Attempt, v.HeadSetDigest, v.State = round, 1, headSet, v1alpha1.VerificationEvaluating
		routeRecords(in, judgeRoute{acceptManual: true}, e.clock.Now())
		applyJudgement(in, j, time.Now())
		p.releaseIfDue(in)
	})
	return e.verification(name)
}

func TestAnomalyHoldsItsHeads(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	v := e.judgeAt(name, 1, heads, passingJudgement(t, heldAnomaly))
	if v.State != v1alpha1.VerificationUnconfirmed || len(v.Anomalies) != 1 {
		t.Fatalf("round 1 = %s, anomalies %+v; want Unconfirmed on the anomaly", v.State, v.Anomalies)
	}

	// Round 2 at the same heads: its evaluator raised nothing.
	v = e.judgeAt(name, 2, heads, passingJudgement(t))
	if v.State != v1alpha1.VerificationUnconfirmed {
		t.Errorf("round 2 at the same heads = %s, want Unconfirmed: round 1's anomaly holds them", v.State)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Summary != heldAnomaly.Summary {
		t.Errorf("round 2 anomalies = %+v, want round 1's carried", v.Anomalies)
	}
	if v.ModelRound == nil || v.ModelRound.Clear {
		t.Errorf("model round = %+v, want round 2 not clear: a replay round must not follow it", v.ModelRound)
	}
	p := e.prVerifyPass(name)
	if st, _, ok := p.acceptanceStatus(); !ok || st.state == statusSuccess {
		t.Errorf("maul/acceptance = %+v (posted %v), want no success over the held anomaly", st, ok)
	}
	if outcomesReady(p.in.Status.Acceptance, p.in.Status.Verification) {
		t.Error("ready over the held anomaly")
	}

	// Round 3 after the heads moved: the anomaly was about other commits.
	moved := "sha256:" + strings.Repeat("1", 64)
	v = e.judgeAt(name, 3, moved, passingJudgement(t))
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 0 {
		t.Errorf("round 3 at moved heads = %s, anomalies %+v; want Passed, none carried", v.State, v.Anomalies)
	}
}

// TestHeldAnomalyRecords: the carried record keeps the round that found
// it; judging the same round again carries nothing twice; a later round
// repeating the anomaly's words adds no copy; a record from before rounds
// were stamped is its judgement's own; and an approver's acceptance, which
// the record carries, lets a round at the heads pass.
func TestHeldAnomalyRecords(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	e.judgeAt(name, 1, heads, passingJudgement(t, heldAnomaly))
	v := e.judgeAt(name, 2, heads, passingJudgement(t, heldAnomaly))
	if len(v.Anomalies) != 1 || v.Anomalies[0].Round != 1 {
		t.Fatalf("round 2 repeating it: anomalies %+v, want round 1's alone", v.Anomalies)
	}
	other := v1alpha1.AnomalyRecord{Summary: "a second oddity", Ledger: []int32{2}}
	e.judgeAt(name, 2, heads, passingJudgement(t, other))
	v = e.judgeAt(name, 2, heads, passingJudgement(t, other))
	if len(v.Anomalies) != 2 || v.Anomalies[0].Round != 1 || v.Anomalies[1].Round != 2 {
		t.Fatalf("round 2 judged twice: anomalies %+v, want round 1's then round 2's, once each", v.Anomalies)
	}

	// A record of a release before rounds were stamped: its judgement's.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Anomalies = []v1alpha1.AnomalyRecord{heldAnomaly}
	})
	if v = e.judgeAt(name, 3, heads, passingJudgement(t)); v.State != v1alpha1.VerificationUnconfirmed ||
		len(v.Anomalies) != 1 || v.Anomalies[0].Round != 2 {
		t.Fatalf("round 3 over an unstamped record: %s, anomalies %+v; want Unconfirmed, round 2's", v.State,
			v.Anomalies)
	}

	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Anomalies[0].Accepted = &v1alpha1.AnomalyAcceptance{By: approver, ByID: 7,
			At: metav1.NewTime(e.clock.Now()), CommentID: 99}
	})
	if v = e.judgeAt(name, 4, heads, passingJudgement(t)); v.State != v1alpha1.VerificationPassed ||
		len(v.Anomalies) != 1 || v.Anomalies[0].Accepted == nil {
		t.Errorf("round 4 after the acceptance: %s, anomalies %+v; want Passed, the accepted record carried",
			v.State, v.Anomalies)
	}
	if v.ModelRound == nil || !v.ModelRound.Clear {
		t.Errorf("model round %+v, want clear once the anomaly is accepted", v.ModelRound)
	}
}

// TestHeldAnomalyHoldsDecideAndTheRung: judged with the anomalies held at
// its heads, a round whose evaluator raised nothing is neither ready nor a
// clear model round, and its evaluator is told of the anomaly; an accepted
// one, or one at other heads, holds nothing and is not handed over.
func TestHeldAnomalyHoldsDecideAndTheRung(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	version := savedCheck(t, versionCheck)
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o},
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: version.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: version.ID, Reason: "yes"}}}
	heads := "sha256:" + strings.Repeat("a", 64)
	pv := &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Round: 2, HeadSetDigest: heads, Rung: v1alpha1.RungReplay,
			Checks: []v1alpha1.SavedCheck{version}},
		Status: v1alpha1.PreviewVerificationStatus{Replay: []v1alpha1.CheckRun{{CheckDigest: version.Digest,
			Result: v1alpha1.CheckRunPass, Token: "t1", Seq: 1}}},
	}
	round1 := heldAnomaly
	round1.Round = 1
	v := &v1alpha1.IntentVerification{HeadSetDigest: heads, OutcomesRound: 1, OutcomesHeadSetDigest: heads,
		Anomalies: []v1alpha1.AnomalyRecord{round1}}
	stored := []v1alpha1.SavedCheck{version}
	if j := judgeRound(acc, pv, stored, nil); j.decision.Handoff != acceptance.HandoffReady {
		t.Fatalf("with nothing held the replay is %s, want ready (the case's premise)", j.decision.Handoff)
	}
	j := judgeHolding(acc, pv, stored, nil, heldAnomalies(v, heads, 2))
	if j.decision.Handoff == acceptance.HandoffReady || j.clear {
		t.Errorf("held: hand-off %s, clear %v; want neither ready nor clear", j.decision.Handoff, j.clear)
	}
	if got := heldForEvaluator(v, pv); len(got) != 1 || got[0].Round != 1 || got[0].Summary != heldAnomaly.Summary {
		t.Errorf("the evaluator is told %+v, want round 1's anomaly", got)
	}
	moved := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Round: 2,
		HeadSetDigest: "sha256:" + strings.Repeat("b", 64)}}
	if got := heldForEvaluator(v, moved); got != nil {
		t.Errorf("at moved heads the evaluator is told %+v, want nothing", got)
	}
	v.Anomalies[0].Accepted = &v1alpha1.AnomalyAcceptance{By: approver, ByID: 7, CommentID: 9}
	if got := heldForEvaluator(v, pv); got != nil {
		t.Errorf("an accepted anomaly is still handed over: %+v", got)
	}
	if j := judgeHolding(acc, pv, stored, nil, heldAnomalies(v, heads, 2)); j.decision.Handoff !=
		acceptance.HandoffReady {
		t.Errorf("accepted: hand-off %s, want ready", j.decision.Handoff)
	}
}

// TestHeldFindingOutcome: an outcome a round found failing, or suspected a
// defect in, at heads H, on a saved check, is unsure (held-finding) in a
// later round at H that finds it passing (a check that failed at the same
// commits and now passes is no evidence the defect is gone), through any
// number of rounds there; it passes once its check is retired through a
// dispute, once an approver accepts it, or once the heads move.
func TestHeldFindingOutcome(t *testing.T) {
	check := savedCheck(t, versionCheck)
	for _, tc := range []struct {
		name  string
		first acceptance.OutcomeResult
	}{
		{"failed", judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced, check.Digest)},
		{"suspected", judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonSuspectedDefect, check.Digest)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			heads := e.verification(name).HeadSetDigest
			first := passingJudgement(t)
			first.decision.PerOutcome[0] = tc.first
			e.judgeAt(name, 1, heads, first)
			for round := int32(2); round <= 3; round++ {
				v := e.judgeAt(name, round, heads, passingJudgement(t))
				if r := v.Outcomes[0]; v.State == v1alpha1.VerificationPassed || r.Result != v1alpha1.OutcomeUnsure ||
					r.Reason != acceptance.ReasonHeldFinding || r.Basis != check.Digest {
					t.Fatalf("round %d at the same heads: %s, %+v; want unsure, held on its check", round, v.State, r)
				}
			}
			retired := passingJudgement(t)
			retired.decision.Retire = []string{check.Digest}
			if v := e.judgeAt(name, 4, heads, retired); v.State != v1alpha1.VerificationPassed {
				t.Errorf("round 4 retiring the check: %s %+v, want Passed", v.State, v.Outcomes[0])
			}
			e.judgeAt(name, 5, heads, first)
			e.setStatus(name, func(in *v1alpha1.Intent) {
				in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{
					ID: oVersion, By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 98,
					HeadSetDigest: heads})
			})
			if v := e.judgeAt(name, 6, heads, passingJudgement(t)); v.State != v1alpha1.VerificationPassed ||
				v.Outcomes[0].Result != v1alpha1.OutcomeAccepted {
				t.Errorf("round 6 after the acceptance: %s %+v, want Passed, accepted", v.State, v.Outcomes[0])
			}
			moved := "sha256:" + strings.Repeat("2", 64)
			if v := e.judgeAt(name, 7, moved, passingJudgement(t)); v.State != v1alpha1.VerificationPassed ||
				v.Outcomes[0].Result != v1alpha1.OutcomePassed {
				t.Errorf("round 7 at moved heads: %s %+v, want Passed", v.State, v.Outcomes[0])
			}
		})
	}
}

// TestVerifyFixOverAHeldAnomaly (G5, G6): /maul verify fix with no
// outcome failing and an anomaly held starts the round (its evaluator
// examines the anomaly again) and says plainly that nothing is fixed; the
// round, its evaluator raising nothing, stays Unconfirmed; /maul accept all
// then accepts the anomaly at the heads, and the judgement is ready.
func TestVerifyFixOverAHeldAnomaly(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(anomalyReport)
	e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
	e.acceptManual(name)
	e.modelRound(name, 1, v1alpha1.CheckRunPass)
	e.evaluateReporting(evaluationReport)
	replies := e.request(name, "issue", "/maul verify fix")
	if len(replies) != 1 || !strings.Contains(replies[0], "No outcome fails at these commits, so there is nothing "+
		"to fix yet") || !strings.Contains(replies[0], "`/maul accept all`") {
		t.Errorf("verify fix replies %q, want it to say nothing is fixed and how the anomaly clears", replies)
	}
	e.modelRound(name, 2, v1alpha1.CheckRunPass)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnconfirmed || len(openAnomalies(v.Anomalies)) != 1 {
		t.Fatalf("round 2 = %s, anomalies %+v; want Unconfirmed on round 1's", v.State, v.Anomalies)
	}
	if st, _, _ := e.prVerifyPass(name).acceptanceStatus(); st.state == statusSuccess {
		t.Fatalf("maul/acceptance %+v over the held anomaly", st)
	}

	replies = e.request(name, "issue", "/maul accept all")
	if len(replies) != 1 || !strings.Contains(replies[0], "the anomaly maul noticed while it looked around are "+
		"accepted at") {
		t.Errorf("accept all replies %q, want the anomaly accepted", replies)
	}
	v := e.verification(name)
	if len(v.Anomalies) != 1 || v.Anomalies[0].Accepted == nil || v.Anomalies[0].Accepted.By != approver {
		t.Fatalf("anomalies %+v, want round 1's accepted by %s", v.Anomalies, approver)
	}
	if st, _, ok := e.prVerifyPass(name).acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Errorf("maul/acceptance %+v (posted %v), want success once the anomaly is accepted", st, ok)
	}
}

// TestAcceptAllAcceptsOnlyWhatWasSeen: /maul accept all accepts only the
// anomalies recorded before the comment was written (as a confirmation of
// a suspected defect must postdate it): one a round recorded after the
// approver commented, or in the comment's own second, still holds the
// heads. One from before the time was recorded is accepted.
func TestAcceptAllAcceptsOnlyWhatWasSeen(t *testing.T) {
	at := time.Date(2026, 11, 11, 14, 22, 51, 0, time.UTC)
	stamp := func(d time.Duration) *metav1.Time { t := metav1.NewTime(at.Add(d)); return &t }
	heads := "sha256:" + strings.Repeat("c", 64)
	v := &v1alpha1.IntentVerification{HeadSetDigest: heads, OutcomesHeadSetDigest: heads, OutcomesRound: 2,
		Anomalies: []v1alpha1.AnomalyRecord{
			{Summary: "seen", Round: 1, RecordedAt: stamp(-time.Minute)},
			{Summary: "legacy", Round: 1},
			{Summary: "same second", Round: 2, RecordedAt: stamp(400 * time.Millisecond)},
			{Summary: "after", Round: 2, RecordedAt: stamp(time.Minute)},
		}}
	if n := len(acceptableAnomalies(v, at)); n != 2 {
		t.Errorf("acceptable anomalies = %d, want the two recorded before the comment", n)
	}
	c := verifyCommand{id: 9, at: at, actor: ghclient.Actor{Login: approver, ID: 7}}
	if n := acceptAnomalies(v, c); n != 2 {
		t.Fatalf("accepted %d, want 2", n)
	}
	for _, a := range v.Anomalies {
		if accepted := a.Accepted != nil; accepted != (a.Summary == "seen" || a.Summary == "legacy") {
			t.Errorf("anomaly %q accepted %v", a.Summary, accepted)
		}
	}
	if len(openAnomalies(v.Anomalies)) != 2 {
		t.Error("the anomalies recorded after the comment no longer hold the heads")
	}
}

// TestOpenFindingMakesTheRoundFull: the next round at heads an anomaly or
// a held outcome still holds is full (openFindingAt), so no replay round
// is spent on what a replay could never pass; accepted, or at other heads,
// they hold nothing.
func TestOpenFindingMakesTheRoundFull(t *testing.T) {
	heads, other := "sha256:"+strings.Repeat("d", 64), "sha256:"+strings.Repeat("e", 64)
	acc := &v1alpha1.IntentAcceptance{}
	held := &v1alpha1.IntentVerification{HeadSetDigest: heads, OutcomesHeadSetDigest: heads, OutcomesRound: 1,
		Outcomes: []v1alpha1.OutcomeResult{{ID: oVersion, Result: v1alpha1.OutcomeUnsure,
			Reason: acceptance.ReasonHeldFinding}}}
	if !openFindingAt(held, acc, heads) || openFindingAt(held, acc, other) {
		t.Error("a held outcome: want an open finding at its heads alone")
	}
	acc.Accepted = []v1alpha1.AcceptanceRecord{{ID: oVersion, By: approver, ByID: 7, CommentID: 1,
		HeadSetDigest: heads}}
	if openFindingAt(held, acc, heads) {
		t.Error("an accepted held outcome is still an open finding")
	}
	anomaly := &v1alpha1.IntentVerification{HeadSetDigest: heads, OutcomesHeadSetDigest: heads, OutcomesRound: 1,
		Anomalies: []v1alpha1.AnomalyRecord{{Summary: "odd", Round: 1}}}
	if !openFindingAt(anomaly, acc, heads) {
		t.Error("an open anomaly: want an open finding")
	}
	anomaly.Anomalies[0].Accepted = &v1alpha1.AnomalyAcceptance{By: approver, ByID: 7, CommentID: 2}
	if openFindingAt(anomaly, acc, heads) {
		t.Error("an accepted anomaly is still an open finding")
	}
}
