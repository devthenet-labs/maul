// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// G-B-LIB-1 (defect gate 2) and the owner's decision of 2026-11-14: a
// finding about an OUTCOME at heads H holds every later round at H (an
// outcome seen not to hold there stays held-finding), while a look-around
// anomaly is a note for the human reviewer and holds nothing: a round whose
// outcomes all pass is Passed with its notes shown.

// observedReport is an evaluator's report: each outcome of claims (id to
// pass, fail or unsure) citing the head http call its position numbers
// (version-shown 1, still-healthy 2), and the anomalies given.
func observedReport(claims map[string]report.Verdict, anomalies ...report.Anomaly) *report.Evaluation {
	ev := &report.Evaluation{Anomalies: anomalies}
	for i, id := range []string{oVersion, oHealthy} {
		if c, ok := claims[id]; ok {
			ev.Verdicts = append(ev.Verdicts, report.OutcomeVerdict{ID: id, Verdict: c, Ledger: []int32{int32(i + 1)}})
		}
	}
	return ev
}

// passing is every evaluable outcome seen to hold.
var passing = map[string]report.Verdict{oVersion: report.VerdictPass, oHealthy: report.VerdictPass}

// heldAnomaly is round 1's look-around anomaly, naming the defect.
var heldAnomaly = report.Anomaly{Summary: "Likely defect in synth/shift.go: the hat track rotates the " +
	"wrong way", Route: "synth.Shift", Ledger: []int32{4}}

// judgeAt writes ev's judgement as round's at headSet on intent name, its
// ledger five head http calls of the evaluator's, the manual outcome
// accepted there, and returns the verification.
func (e *env) judgeAt(name string, round int32, headSet string, ev *report.Evaluation) *v1alpha1.IntentVerification {
	e.t.Helper()
	p := e.prVerifyPass(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.Round, v.Attempt, v.HeadSetDigest, v.State = round, 1, headSet, v1alpha1.VerificationEvaluating
		routeRecords(in, judgeRoute{acceptManual: true}, e.clock.Now())
		pv := &v1alpha1.PreviewVerification{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-v%d", name, round)},
			Spec:   v1alpha1.PreviewVerificationSpec{Round: round, Attempt: 1, HeadSetDigest: headSet},
			Status: v1alpha1.PreviewVerificationStatus{Ledger: observedLedger(5)}}
		applyJudgement(in, judgeRound(v, in.Status.Acceptance, pv, ev))
		p.releaseIfDue(in)
	})
	return e.verification(name)
}

// TestAnomalyIsANote: a round whose every outcome is seen to hold is Passed
// with its look-around anomaly recorded as a note and maul/acceptance
// success; a later round at the same heads keeps the note (round 1's) and
// passes too; at moved heads it is gone.
func TestAnomalyIsANote(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	v := e.judgeAt(name, 1, heads, observedReport(passing, heldAnomaly))
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 1 {
		t.Fatalf("round 1 = %s, anomalies %+v; want Passed with the note", v.State, v.Anomalies)
	}
	p := e.prVerifyPass(name)
	if st, _, ok := p.acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Errorf("maul/acceptance = %+v (posted %v), want success beside the note", st, ok)
	}
	if !outcomesReady(p.in.Status.Acceptance, p.in.Status.Verification) {
		t.Error("not ready over a note")
	}

	// Round 2 at the same heads: its evaluator raised nothing.
	v = e.judgeAt(name, 2, heads, observedReport(passing))
	if v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 2 at the same heads = %s, want Passed", v.State)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Summary != heldAnomaly.Summary || v.Anomalies[0].Round != 1 {
		t.Errorf("round 2 anomalies = %+v, want round 1's note carried", v.Anomalies)
	}

	// Round 3 after the heads moved: the note was about other commits.
	moved := "sha256:" + strings.Repeat("1", 64)
	v = e.judgeAt(name, 3, moved, observedReport(passing))
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 0 {
		t.Errorf("round 3 at moved heads = %s, anomalies %+v; want Passed, none carried", v.State, v.Anomalies)
	}
}

// TestNoteDoesNotHoldAnAcceptance: a round left Unconfirmed by an outcome
// maul could not count, beside a note, is ready once an approver accepts
// that outcome: the note holds nothing.
func TestNoteDoesNotHoldAnAcceptance(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	ev := observedReport(map[string]report.Verdict{oVersion: report.VerdictUnsure, oHealthy: report.VerdictPass},
		heldAnomaly)
	if v := e.judgeAt(name, 1, heads, ev); v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("round 1 = %s, want Unconfirmed (the case's premise)", v.State)
	}
	replies := e.request(name, "issue", "/maul accept "+oVersion)
	if len(replies) != 1 || !strings.Contains(replies[0], "is done") {
		t.Fatalf("accept replies %q, want it accepted", replies)
	}
	p := e.prVerifyPass(name)
	if !outcomesReady(p.in.Status.Acceptance, p.in.Status.Verification) {
		t.Error("not ready once the outcome is accepted, beside a note")
	}
	if st, _, ok := p.acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Errorf("maul/acceptance %+v (posted %v), want success once accepted", st, ok)
	}
}

// TestCarriedNoteRecords: a note carried at the same heads keeps the round
// that found it; judging the same round again carries nothing twice; a
// later round repeating the note's words adds no copy; a record from before
// rounds were stamped is its judgement's own. None of it holds a round.
func TestCarriedNoteRecords(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	e.judgeAt(name, 1, heads, observedReport(passing, heldAnomaly))
	v := e.judgeAt(name, 2, heads, observedReport(passing, heldAnomaly))
	if len(v.Anomalies) != 1 || v.Anomalies[0].Round != 1 {
		t.Fatalf("round 2 repeating it: anomalies %+v, want round 1's alone", v.Anomalies)
	}
	other := report.Anomaly{Summary: "a second oddity", Ledger: []int32{2}}
	e.judgeAt(name, 2, heads, observedReport(passing, other))
	v = e.judgeAt(name, 2, heads, observedReport(passing, other))
	if len(v.Anomalies) != 2 || v.Anomalies[0].Round != 1 || v.Anomalies[1].Round != 2 {
		t.Fatalf("round 2 judged twice: anomalies %+v, want round 1's then round 2's, once each", v.Anomalies)
	}

	// A record of a release before rounds were stamped: its judgement's.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Anomalies = []v1alpha1.AnomalyRecord{{Summary: heldAnomaly.Summary,
			Route: heldAnomaly.Route, Ledger: heldAnomaly.Ledger}}
	})
	if v = e.judgeAt(name, 3, heads, observedReport(passing)); v.State != v1alpha1.VerificationPassed ||
		len(v.Anomalies) != 1 || v.Anomalies[0].Round != 2 {
		t.Fatalf("round 3 over an unstamped record: %s, anomalies %+v; want Passed, round 2's", v.State,
			v.Anomalies)
	}
}

// TestNoteHoldsNothing: a round whose evaluator reported a look-around
// anomaly, every outcome seen to hold, is ready, with the note recorded;
// an anomaly citing no call maul counts (a relay's, or no seq the ledger
// holds) is not recorded: it needs evidence, by the verdicts' own rule.
func TestNoteHoldsNothing(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o}}
	heads := "sha256:" + strings.Repeat("a", 64)
	ledger := observedLedger(4)
	ledger.Entries[2].Kind = v1alpha1.LedgerKindRelay
	pv := &v1alpha1.PreviewVerification{Spec: v1alpha1.PreviewVerificationSpec{Round: 1, HeadSetDigest: heads},
		Status: v1alpha1.PreviewVerificationStatus{Ledger: ledger}}
	ev := &report.Evaluation{
		Verdicts: []report.OutcomeVerdict{{ID: oVersion, Verdict: report.VerdictPass, Ledger: []int32{1}}},
		Anomalies: []report.Anomaly{{Summary: heldAnomaly.Summary, Route: heldAnomaly.Route, Ledger: []int32{2}},
			{Summary: "on the relay's call", Ledger: []int32{3}}, {Summary: "on no call", Ledger: []int32{9}}}}
	j := judgeRound(nil, acc, pv, ev)
	if j.judgement.Handoff != acceptance.HandoffReady || len(j.anomalies) != 1 ||
		j.anomalies[0].Summary != heldAnomaly.Summary {
		t.Errorf("hand-off %s, notes %+v; want ready, the evidenced note alone recorded", j.judgement.Handoff,
			j.anomalies)
	}
}

// TestHeldFindingOutcome: an outcome a round saw not to hold at heads H,
// citing a call maul counts, is unsure (held-finding) in every later round
// at H, whether that round sees it hold, is unsure of it or does not report
// it (the hold is kept on record, so an unsure round does not free it for
// the next); a new fail there is failed again. It passes once the heads
// move, once an approver accepts it, or once the fix round it was handed
// concludes no defect at H.
func TestHeldFindingOutcome(t *testing.T) {
	failing := map[string]report.Verdict{oVersion: report.VerdictFail, oHealthy: report.VerdictPass}
	start := func(t *testing.T) (*env, string, string) {
		e, name := newEvaluatorEnv(t, 0.4)
		e.review(name, 3)
		heads := e.verification(name).HeadSetDigest
		if v := e.judgeAt(name, 1, heads, observedReport(failing)); v.State != v1alpha1.VerificationFailing ||
			v.Outcomes[0].Reason != acceptance.ReasonSeenNotToHold {
			t.Fatalf("round 1 = %s %+v, want Failing, seen not to hold", v.State, v.Outcomes[0])
		}
		return e, name, heads
	}
	held := func(t *testing.T, v *v1alpha1.IntentVerification, round int32) {
		t.Helper()
		if r := v.Outcomes[0]; v.State == v1alpha1.VerificationPassed || r.Result != v1alpha1.OutcomeUnsure ||
			r.Reason != acceptance.ReasonHeldFinding {
			t.Fatalf("round %d at the same heads: %s, %+v; want unsure, held", round, v.State, r)
		}
	}

	t.Run("held through later rounds", func(t *testing.T) {
		e, name, heads := start(t)
		held(t, e.judgeAt(name, 2, heads, observedReport(passing)), 2)
		held(t, e.judgeAt(name, 3, heads, observedReport(map[string]report.Verdict{oVersion: report.VerdictUnsure,
			oHealthy: report.VerdictPass})), 3)
		held(t, e.judgeAt(name, 4, heads, observedReport(map[string]report.Verdict{oHealthy: report.VerdictPass})), 4)
		held(t, e.judgeAt(name, 5, heads, observedReport(passing)), 5)
		if v := e.judgeAt(name, 6, heads, observedReport(failing)); v.State != v1alpha1.VerificationFailing ||
			v.Outcomes[0].Result != v1alpha1.OutcomeFailed {
			t.Errorf("round 6 seeing it fail again: %s %+v, want Failing", v.State, v.Outcomes[0])
		}
	})
	t.Run("released by moved heads", func(t *testing.T) {
		e, name, heads := start(t)
		held(t, e.judgeAt(name, 2, heads, observedReport(passing)), 2)
		moved := "sha256:" + strings.Repeat("2", 64)
		if v := e.judgeAt(name, 3, moved, observedReport(passing)); v.State != v1alpha1.VerificationPassed ||
			v.Outcomes[0].Result != v1alpha1.OutcomePassed {
			t.Errorf("round 3 at moved heads: %s %+v, want Passed", v.State, v.Outcomes[0])
		}
	})
	t.Run("released by an acceptance", func(t *testing.T) {
		e, name, heads := start(t)
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{
				ID: oVersion, By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 98,
				HeadSetDigest: heads})
		})
		if v := e.judgeAt(name, 2, heads, observedReport(passing)); v.State != v1alpha1.VerificationPassed ||
			v.Outcomes[0].Result != v1alpha1.OutcomeAccepted {
			t.Errorf("round 2 after the acceptance: %s %+v, want Passed, accepted", v.State, v.Outcomes[0])
		}
	})
	t.Run("released by the fix round's no defect", func(t *testing.T) {
		e, name, heads := start(t)
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.Verification.NoDefect = &v1alpha1.NoDefectRecord{Run: name + "-rev-r2-a1",
				Reason: "the endpoint is right", HeadSetDigest: heads, Round: 1, At: metav1.NewTime(e.clock.Now())}
		})
		if v := e.judgeAt(name, 2, heads, observedReport(passing)); v.State != v1alpha1.VerificationPassed ||
			v.Outcomes[0].Result != v1alpha1.OutcomePassed {
			t.Errorf("round 2 after the no-defect conclusion: %s %+v, want Passed", v.State, v.Outcomes[0])
		}
	})
	t.Run("a no defect about another round releases nothing", func(t *testing.T) {
		e, name, heads := start(t)
		held(t, e.judgeAt(name, 2, heads, observedReport(passing)), 2)
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.Verification.NoDefect = &v1alpha1.NoDefectRecord{Run: name + "-rev-r2-a1",
				Reason: "the endpoint is right", HeadSetDigest: heads, Round: 1, At: metav1.NewTime(e.clock.Now())}
		})
		held(t, e.judgeAt(name, 3, heads, observedReport(passing)), 3)
	})
}

// TestNoteRoundPasses: a round whose evaluator sees every outcome hold and
// reports a look-around anomaly is Passed and maul/acceptance success; a
// /maul verify fix over it says nothing of anomalies, and /maul accept
// anomalies, a command no longer offered, gets the reply any id the plan
// does not name gets and accepts nothing, as does a reply owed again for
// such a comment applied before the upgrade.
func TestNoteRoundPasses(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(anomalyReport)
	e.acceptManual(name)
	e.observedRound(name, 1)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 1 {
		t.Fatalf("round 1 = %s, anomalies %+v; want Passed with the note", v.State, v.Anomalies)
	}
	if st, _, ok := e.prVerifyPass(name).acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Errorf("maul/acceptance %+v (posted %v), want success beside the note", st, ok)
	}

	replies := e.request(name, "issue", "/maul verify fix")
	if len(replies) != 1 || strings.Contains(replies[0], "anomal") {
		t.Errorf("verify fix replies %q, want no word of anomalies", replies)
	}
	accepted := len(e.get(name).Status.Acceptance.Accepted)
	replies = e.request(name, "issue", "/maul accept anomalies")
	if len(replies) != 1 || !strings.Contains(replies[0], "names no outcome of the approved plan") {
		t.Errorf("accept anomalies replies %q, want the unknown-outcome reply", replies)
	}
	if got := len(e.get(name).Status.Acceptance.Accepted); got != accepted {
		t.Errorf("accepted %d records after /maul accept anomalies, want %d", got, accepted)
	}
	again := e.prVerifyPass(name).recordedVerifyNotice(verifyCommand{surface: "issue", key: "comment-1", id: 4242,
		verb: "accept", body: "/maul accept anomalies"})
	if again.Outcome == templates.VerifyAccepted {
		t.Errorf("recorded reply %+v for an old /maul accept anomalies, want nothing accepted", again)
	}
}
