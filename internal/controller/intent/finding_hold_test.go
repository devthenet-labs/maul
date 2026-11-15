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
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// G-B-LIB-1 (defect gate 2) and the owner's decision of 2026-11-14: a
// finding about an OUTCOME at heads H holds every later round at H (an
// outcome found failing or suspected there stays held-finding), while a
// look-around anomaly is a note for the human reviewer and holds nothing:
// a round whose outcomes all pass is Passed with its notes shown.

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
		evaluation: &report.Evaluation{}, clear: true}
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

// TestAnomalyIsANote: a round whose every outcome is confirmed is Passed
// with its look-around anomaly recorded as a note, maul/acceptance success
// and a clear model round; a later round at the same heads keeps the note
// (round 1's) and passes too; at moved heads it is gone.
func TestAnomalyIsANote(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	v := e.judgeAt(name, 1, heads, passingJudgement(t, heldAnomaly))
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 1 {
		t.Fatalf("round 1 = %s, anomalies %+v; want Passed with the note", v.State, v.Anomalies)
	}
	if v.ModelRound == nil || !v.ModelRound.Clear {
		t.Errorf("model round = %+v, want clear: a note is no objection", v.ModelRound)
	}
	p := e.prVerifyPass(name)
	if st, _, ok := p.acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Errorf("maul/acceptance = %+v (posted %v), want success beside the note", st, ok)
	}
	if !outcomesReady(p.in.Status.Acceptance, p.in.Status.Verification) {
		t.Error("not ready over a note")
	}

	// Round 2 at the same heads: its evaluator raised nothing.
	v = e.judgeAt(name, 2, heads, passingJudgement(t))
	if v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 2 at the same heads = %s, want Passed", v.State)
	}
	if len(v.Anomalies) != 1 || v.Anomalies[0].Summary != heldAnomaly.Summary || v.Anomalies[0].Round != 1 {
		t.Errorf("round 2 anomalies = %+v, want round 1's note carried", v.Anomalies)
	}

	// Round 3 after the heads moved: the note was about other commits.
	moved := "sha256:" + strings.Repeat("1", 64)
	v = e.judgeAt(name, 3, moved, passingJudgement(t))
	if v.State != v1alpha1.VerificationPassed || len(v.Anomalies) != 0 {
		t.Errorf("round 3 at moved heads = %s, anomalies %+v; want Passed, none carried", v.State, v.Anomalies)
	}
}

// TestNoteDoesNotHoldAnAcceptance: a round left Unconfirmed by an outcome
// maul could not confirm, beside a note, is ready once an approver accepts
// that outcome: the note holds nothing.
func TestNoteDoesNotHoldAnAcceptance(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	j := passingJudgement(t, heldAnomaly)
	// No check and no verdict: unsure, and nothing a recheck is asked for.
	j.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonNoCheck, "")
	if v := e.judgeAt(name, 1, heads, j); v.State != v1alpha1.VerificationUnconfirmed {
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
	if v = e.judgeAt(name, 3, heads, passingJudgement(t)); v.State != v1alpha1.VerificationPassed ||
		len(v.Anomalies) != 1 || v.Anomalies[0].Round != 2 {
		t.Fatalf("round 3 over an unstamped record: %s, anomalies %+v; want Passed, round 2's", v.State,
			v.Anomalies)
	}
}

// TestNoteHoldsNeitherDecideNorTheRung: a round whose evaluator reported
// only a look-around anomaly, every outcome confirmed, is ready and a clear
// model round, with the note recorded.
func TestNoteHoldsNeitherDecideNorTheRung(t *testing.T) {
	o := v1alpha1.AcceptanceOutcome{ID: oVersion, Outcome: "The build page shows the version"}
	version := savedCheck(t, versionCheck)
	acc := &v1alpha1.IntentAcceptance{Outcomes: []v1alpha1.AcceptanceOutcome{o},
		Reviews: []v1alpha1.CheckReviewRecord{{CheckDigest: version.Digest, OutcomeDigest: outcomeDigest(o),
			Answer: v1alpha1.ReviewProves, Run: "r1", Check: version.ID, Reason: "yes"}}}
	heads := "sha256:" + strings.Repeat("a", 64)
	pv := &v1alpha1.PreviewVerification{
		Spec: v1alpha1.PreviewVerificationSpec{Round: 1, HeadSetDigest: heads,
			Checks: []v1alpha1.SavedCheck{version}},
		Status: v1alpha1.PreviewVerificationStatus{Ledger: &v1alpha1.LedgerSummary{Count: 4},
			Replay: []v1alpha1.CheckRun{{CheckDigest: version.Digest, Result: v1alpha1.CheckRunPass, Token: "t1",
				Seq: 1}}},
	}
	ev := &report.Evaluation{
		Verdicts:  []report.OutcomeVerdict{{ID: oVersion, Verdict: report.VerdictPass, Ledger: []int32{1}}},
		Anomalies: []report.Anomaly{{Summary: heldAnomaly.Summary, Route: heldAnomaly.Route, Ledger: []int32{2}}}}
	j := judgeRound(acc, pv, []v1alpha1.SavedCheck{version}, ev)
	if j.decision.Handoff != acceptance.HandoffReady || !j.clear || len(j.anomalies) != 1 {
		t.Errorf("hand-off %s, clear %v, notes %+v; want ready, clear, the note recorded", j.decision.Handoff,
			j.clear, j.anomalies)
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
			// Retired through a dispute: gone from the store as read.
			retired.stored = []v1alpha1.SavedCheck{savedCheck(t, healthCheck)}
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

// TestNoteRoundPasses: a round whose evaluator confirms every outcome and
// reports a look-around anomaly is Passed and maul/acceptance success; a
// /maul verify fix over it says nothing of anomalies, and /maul accept
// anomalies, a command no longer offered, gets the reply any id the plan
// does not name gets and accepts nothing, as does a reply owed again for
// such a comment applied before the upgrade.
func TestNoteRoundPasses(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(anomalyReport)
	e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
	e.acceptManual(name)
	e.modelRound(name, 1, v1alpha1.CheckRunPass)
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

// TestOpenFindingMakesTheRoundFull: the next round at heads a held outcome
// still holds is full (openFindingAt), so no replay round is spent on what
// a replay could never pass; accepted, or at other heads, it holds nothing,
// and a look-around note never does.
func TestOpenFindingMakesTheRoundFull(t *testing.T) {
	heads, other := "sha256:"+strings.Repeat("d", 64), "sha256:"+strings.Repeat("e", 64)
	acc := &v1alpha1.IntentAcceptance{}
	held := &v1alpha1.IntentVerification{HeadSetDigest: heads, OutcomesHeadSetDigest: heads, OutcomesRound: 1,
		Outcomes: []v1alpha1.OutcomeResult{{ID: oVersion, Result: v1alpha1.OutcomeUnsure,
			Reason: acceptance.ReasonHeldFinding}}}
	if !openFindingAt(held, acc, heads) || openFindingAt(held, acc, other) {
		t.Error("a held outcome: want an open finding at its heads alone")
	}
	// A fix round handed the reproduction concluded no defect at the heads:
	// its recorded reason releases the outcome it was handed, the suspected
	// defect an approver confirmed (TestIntentEvaluateFixFindsNoDefect), and
	// nothing it was not handed.
	noDefect := held.DeepCopy()
	noDefect.NoDefect = &v1alpha1.NoDefectRecord{Run: "x-rev-r2-a1", Reason: "fine", HeadSetDigest: heads,
		Round: 1}
	if !openFindingAt(noDefect, acc, heads) {
		t.Error("a no-defect conclusion released an outcome held from an earlier round, never handed to it")
	}
	noDefect.Outcomes[0].Reason = acceptance.ReasonSuspectedDefect
	if !openFindingAt(noDefect, acc, heads) {
		t.Error("a no-defect conclusion released a suspected defect no approver confirmed")
	}
	noDefect.Suspected = &v1alpha1.SuspectedDefect{Verification: "x-v-r1", HeadSetDigest: heads,
		Outcomes: []string{oVersion}, Confirmed: &v1alpha1.Rearm{By: approver, ByID: 7}}
	if openFindingAt(noDefect, acc, heads) || len(heldOutcomes(noDefect, heads, 2)) != 0 {
		t.Error("a confirmed suspected defect after a no-defect conclusion at its heads still holds them")
	}
	for _, round := range []int32{0, 2} {
		noDefect.NoDefect.Round = round
		if !openFindingAt(noDefect, acc, heads) {
			t.Errorf("a no-defect conclusion about round %d released round 1's judgement", round)
		}
	}
	acc.Accepted = []v1alpha1.AcceptanceRecord{{ID: oVersion, By: approver, ByID: 7, CommentID: 1,
		HeadSetDigest: heads}}
	if openFindingAt(held, acc, heads) {
		t.Error("an accepted held outcome is still an open finding")
	}
	anomaly := &v1alpha1.IntentVerification{HeadSetDigest: heads, OutcomesHeadSetDigest: heads, OutcomesRound: 1,
		Anomalies: []v1alpha1.AnomalyRecord{{Summary: "odd", Round: 1}}}
	if openFindingAt(anomaly, acc, heads) {
		t.Error("a look-around note is an open finding: it holds nothing")
	}
}
