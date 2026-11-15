// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The recheck (rung.go, gate 5 G5-B-CLI-1): a full round judged unsure only
// because the check reviewer has not accepted a check of an outcome its
// evaluator passed asks for the head set's one escalation, a full round at
// the same heads whose evaluator is handed the refusal.

const recheckHeads = "sha256:392c6fc58a741720edca81fe0c2316ecaa09fd01e78c5bfc8883907b06c9ba67"

// recheckRound is a full round judged Unconfirmed at recheckHeads: rs its
// results, acc's outcomes theirs (manual ones listed in manual), its
// evaluator's report clear, its environment clean, nothing else found.
func recheckRound(rs []v1alpha1.OutcomeResult, manual ...string) (*v1alpha1.IntentVerification,
	*v1alpha1.IntentAcceptance, roundJudgement) {
	acc := &v1alpha1.IntentAcceptance{OutcomesDigest: "sha256:0f35"}
	for _, r := range rs {
		m := false
		for _, id := range manual {
			m = m || id == r.ID
		}
		acc.Outcomes = append(acc.Outcomes, v1alpha1.AcceptanceOutcome{ID: r.ID, Manual: m})
	}
	nv := &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed, Round: 1, Rung: v1alpha1.RungFull,
		HeadSetDigest: recheckHeads, Outcomes: rs, OutcomesRound: 1, OutcomesHeadSetDigest: recheckHeads,
		OutcomesEnvironment: string(httpstep.EnvOK)}
	j := roundJudgement{decision: acceptance.Decision{Env: httpstep.EnvOK, Handoff: acceptance.HandoffUnconfirmed},
		evaluation: &report.Evaluation{}, clear: true}
	return nv, acc, j
}

func passedR(id string) v1alpha1.OutcomeResult {
	return v1alpha1.OutcomeResult{ID: id, Result: v1alpha1.OutcomePassed, Reason: acceptance.ReasonConfirmed}
}

func unsureR(id, reason string) v1alpha1.OutcomeResult {
	return v1alpha1.OutcomeResult{ID: id, Result: v1alpha1.OutcomeUnsure, Reason: reason}
}

// TestRecheckable: only a round whose every outcome left open is one its
// evaluator passed on no proven check (model-only-pass) is rechecked; any
// finding, doubt, dispute, fix, suspected defect, unclean environment or
// objecting evaluator keeps it as it is today.
func TestRecheckable(t *testing.T) {
	refused := unsureR("json-unchanged", acceptance.ReasonModelOnlyPass)
	for _, tc := range []struct {
		name   string
		rs     []v1alpha1.OutcomeResult
		manual []string
		mutate func(*v1alpha1.IntentVerification, *v1alpha1.IntentAcceptance, *roundJudgement)
		want   bool
	}{
		// Gate 5's control G4, round 1: six confirmed, json-unchanged's only
		// check refused does_not_prove.
		{name: "gate 5 G4 round 1", want: true, rs: []v1alpha1.OutcomeResult{passedR("changes-plain-example"),
			passedR("changes-no-wrap"), passedR("changes-kick-1237"), passedR("changes-zero"),
			passedR("changes-json"), passedR("plain-unchanged"), refused}},
		// Gate 5's G1, round 1: a suspected defect beside a refused check.
		{name: "gate 5 G1 round 1", rs: []v1alpha1.OutcomeResult{
			unsureR("longest-plain-lines", acceptance.ReasonModelOnlyPass), passedR("longest-run-example"),
			unsureR("longest-json", acceptance.ReasonSuspectedDefect), passedR("json-unchanged")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				v.Suspected = &v1alpha1.SuspectedDefect{HeadSetDigest: recheckHeads, Outcomes: []string{"longest-json"}}
				j.decision.Suspected = &acceptance.FixRequest{}
			}},
		{name: "a manual outcome open beside it", want: true, manual: []string{"looks-right"},
			rs: []v1alpha1.OutcomeResult{passedR("a"), refused, unsureR("looks-right", acceptance.ReasonManual)}},
		{name: "an outcome an approver accepted beside it", want: true,
			rs: []v1alpha1.OutcomeResult{{ID: "a", Result: v1alpha1.OutcomeAccepted,
				Reason: acceptance.ReasonModelOnlyFail}, refused}},
		{name: "nothing left open", rs: []v1alpha1.OutcomeResult{passedR("a"), passedR("b")}},
		{name: "only a manual outcome open", manual: []string{"m"},
			rs: []v1alpha1.OutcomeResult{passedR("a"), unsureR("m", acceptance.ReasonManual)}},
		{name: "a failed outcome", rs: []v1alpha1.OutcomeResult{refused,
			{ID: "a", Result: v1alpha1.OutcomeFailed, Reason: acceptance.ReasonReproduced}}},
		{name: "a quarantined outcome", rs: []v1alpha1.OutcomeResult{refused,
			{ID: "a", Result: v1alpha1.OutcomeQuarantined, Reason: acceptance.ReasonQuarantined}}},
		{name: "the evaluator's objection", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.clear = false // a fail verdict or a contrary entry, on any outcome
			}},
		{name: "a failing environment", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.decision.Env = httpstep.EnvAppFailure
			}},
		{name: "no evaluator report", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.evaluation = nil
			}},
		{name: "a replay round", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.replay = true
			}},
		{name: "a fix asked for", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.decision.Fix = &acceptance.FixRequest{}
			}},
		{name: "a dispute", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.disputes = []report.Dispute{{}}
			}},
		{name: "a dispute's replay due", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.decision.ReplayRound = true
				v.Disputed = []string{recheckHeads}
			}},
		{name: "a suspected defect this round", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.decision.Suspected = &acceptance.FixRequest{}
			}},
		{name: "a replay round asked for", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *roundJudgement) {
				j.decision.ReplayRound = true
			}},
		{name: "disputed checks on record", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, _ *roundJudgement) {
				v.Disputed = []string{recheckHeads}
			}},
		{name: "a suspected defect on record at the heads", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, _ *roundJudgement) {
				v.Suspected = &v1alpha1.SuspectedDefect{HeadSetDigest: recheckHeads, Outcomes: []string{"a"}}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nv, acc, j := recheckRound(tc.rs, tc.manual...)
			if tc.mutate != nil {
				tc.mutate(nv, acc, &j)
			}
			if got := recheckable(nv, acc, j); got != tc.want {
				t.Errorf("recheckable = %v, want %v", got, tc.want)
			}
		})
	}
	// Every other reason an outcome is left open is a finding, a doubt or
	// maul's own trouble: none is rechecked.
	for _, reason := range []string{acceptance.ReasonModelOnlyFail, acceptance.ReasonNoCheck,
		acceptance.ReasonSuspectedDefect, acceptance.ReasonHeldFinding, acceptance.ReasonEvaluatorSawFailure,
		acceptance.ReasonEvaluatorDisagrees, acceptance.ReasonNotReproduced, acceptance.ReasonDisputed,
		acceptance.ReasonRetired, acceptance.ReasonFlaky, acceptance.ReasonChecksDisagree,
		acceptance.ReasonNotAsserted, acceptance.ReasonNoBaseEvidence, acceptance.ReasonBuildFailed,
		acceptance.ReasonNotReplayed, acceptance.ReasonEnvFailed, acceptance.ReasonInfra,
		acceptance.ReasonQuarantined, acceptance.ReasonManual, ""} {
		t.Run("reason "+reason, func(t *testing.T) {
			nv, acc, j := recheckRound([]v1alpha1.OutcomeResult{refused, unsureR("a", reason)})
			if recheckable(nv, acc, j) {
				t.Errorf("an outcome unsure for %q beside a refused check is rechecked", reason)
			}
		})
	}
}

// TestRecordLadderRechecksOnce: a full round recheckable asks for its head
// set's one escalation; one whose head set already had it (asked for or
// spent, by a replay's escalation or an earlier recheck) asks for nothing,
// and a Passed round asks for nothing.
func TestRecordLadderRechecksOnce(t *testing.T) {
	rs := []v1alpha1.OutcomeResult{passedR("a"), unsureR("b", acceptance.ReasonModelOnlyPass)}
	nv, acc, j := recheckRound(rs)
	recordLadder(nv, acc, j)
	if e := nv.Escalation; e == nil || e.Round != 1 || e.HeadSetDigest != recheckHeads || !e.Recheck {
		t.Fatalf("escalation %+v, want round 1's at its heads", e)
	}
	if !escalationAsked(nv, recheckHeads) {
		t.Error("the recheck asked for is not due")
	}

	nv, acc, j = recheckRound(rs)
	nv.Round, nv.OutcomesRound = 3, 3
	nv.Escalation = &v1alpha1.VerificationEscalation{Round: 1, HeadSetDigest: recheckHeads}
	recordLadder(nv, acc, j)
	if e := nv.Escalation; e.Round != 1 {
		t.Errorf("escalation %+v, want the head set's spent one kept: a head set is rechecked once", e)
	}
	if escalationAsked(nv, recheckHeads) {
		t.Error("a spent escalation is due again")
	}

	nv, acc, j = recheckRound(rs)
	nv.State = v1alpha1.VerificationPassed
	recordLadder(nv, acc, j)
	if nv.Escalation != nil {
		t.Errorf("escalation %+v asked for by a Passed round", nv.Escalation)
	}
}

// refusedRound is gate 5's control G4 in the fake: the manual outcome
// accepted, round 1 a full round whose evaluator reports text, the version
// check proven and the health check, its only one, refused does_not_prove.
// It returns the env, the intent, the reviewer and round 1.
func refusedRound(t *testing.T, text string) (*env, string, *fakeReviewer, v1alpha1.PreviewVerification) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	r := &fakeReviewer{answers: map[string]v1alpha1.ReviewAnswer{health.Digest: v1alpha1.ReviewDoesNotProve}}
	e.reviewChecksWith(r)
	e.evaluateReporting(text)
	e.acceptManual(name)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.confirm(pv.Name, version, health)
	e.settleReviews(name)
	return e, name, r, pv
}

// recheckedRound drives round 2, the recheck, to its judgement: the stored
// version check replays green and the evaluator's new health check
// (healthCheck2, answered by r as given) is confirmed.
func (e *env) recheckedRound(name string, r *fakeReviewer, answer v1alpha1.ReviewAnswer) {
	e.t.Helper()
	pv := e.roundOf(name, 2)
	again := savedCheck(e.t, healthCheck2)
	r.answers[again.Digest] = answer
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.confirm(pv.Name, again)
	_, stored := e.checkStore(name)
	got := e.pvNamed(pv.Name)
	for i, c := range stored {
		got.Status.Replay = append(got.Status.Replay, v1alpha1.CheckRun{CheckDigest: c.Digest,
			Result: v1alpha1.CheckRunPass, Token: "rp" + c.Digest[7:11], Seq: int32(1 + i)})
	}
	if err := e.c.Update(context.Background(), got); err != nil {
		e.t.Fatal(err)
	}
	e.settleReviews(name)
}

// recheckAsked holds round 1, judged, to the recheck it asks for: Unconfirmed
// only on the refused health check, the escalation recorded as a recheck,
// said in the comments, the hand-off waiting and maul/acceptance pending.
func (e *env) recheckAsked(name string) {
	t := e.t
	t.Helper()
	v := e.verification(name)
	if v.Round != 1 || v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("round 1 = %d %s %+v, want Unconfirmed", v.Round, v.State, v.Outcomes)
	}
	if r := v.Outcomes[1]; r.ID != oHealthy || r.Reason != acceptance.ReasonModelOnlyPass {
		t.Fatalf("health outcome %+v, want unsure on its refused check (the case's premise)", r)
	}
	if esc := v.Escalation; esc == nil || esc.Round != 1 || esc.HeadSetDigest != v.HeadSetDigest || !esc.Recheck {
		t.Fatalf("escalation %+v, want round 1's recheck at its heads", esc)
	}
	p := e.prVerifyPass(name)
	if pr := p.verifyProgress(); pr == nil || !pr.Recheck {
		t.Errorf("progress %+v with the recheck due, want it said", pr)
	}
	if why := p.gaveUp(p.in.Status.Acceptance, v); why != "" {
		t.Errorf("gave up (%s) with the recheck due", why)
	}
	if st, _, ok := p.acceptanceStatus(); !ok || st.state != statusPending ||
		!strings.HasPrefix(st.description, "maul is checking this") {
		t.Errorf("maul/acceptance %+v (posted %v), want pending while the recheck is due", st, ok)
	}
}

// recheckStarted holds round 2 to the recheck: full and automatic at round
// 1's heads, counted, said in the comments while it runs, its evaluator
// handed the health check's refusal and the refused check itself (G5R-B-CLI-1).
func (e *env) recheckStarted(name string, pv1 v1alpha1.PreviewVerification) {
	t := e.t
	t.Helper()
	pv2 := e.roundOf(name, 2)
	if pr := e.prVerifyPass(name).verifyProgress(); pr == nil || !pr.Recheck ||
		pr.State != templates.VerifyEvaluating {
		t.Errorf("progress %+v while the recheck runs, want it said to be maul checking once more", pr)
	}
	if pv2.Spec.Rung != v1alpha1.RungFull || pv2.Spec.HeadSetDigest != pv1.Spec.HeadSetDigest ||
		pv2.Annotations[annotationVerificationTrigger] != verificationByAutomatic {
		t.Fatalf("round 2 = %s at %s by %q, want the full recheck at round 1's heads, automatic",
			pv2.Spec.Rung, pv2.Spec.HeadSetDigest, pv2.Annotations[annotationVerificationTrigger])
	}
	if v := e.verification(name); v.Automatic != 2 {
		t.Errorf("automatic = %d, want the recheck counted against maxVerifications", v.Automatic)
	}
	h := decodeEvaluateHandoff(t, mustHandoff(t, e.prVerifyPass(name), &pv2))
	if len(h.PriorRefused) != 1 || h.PriorRefused[0] != (agentrun.PriorRefusal{Outcome: oHealthy,
		Check: "still-healthy", Reason: "read the steps", Body: savedCheck(t, healthCheck).Body}) {
		t.Errorf("round 2's refused checks = %+v, want the health check's refusal with the check itself", h.PriorRefused)
	}
}

// TestRefusedCheckIsRecheckedOnce (gate 5 G5-B-CLI-1): round 1, held only
// by a check the reviewer refused, asks for its heads' one recheck: the
// hand-off waits, maul/acceptance stays pending and the comment says maul
// checks once more because a check was not accepted. Round 2 starts by
// itself, full and automatic at the same heads, counted against
// maxVerifications, its evaluator handed the refusal; it writes a stronger
// check, which the reviewer accepts, and the round passes on Decide's
// rules. Refused again instead, it stays Unconfirmed and no round 3 starts.
func TestRefusedCheckIsRecheckedOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer v1alpha1.ReviewAnswer
		want   v1alpha1.IntentVerificationState
	}{
		{"the stronger check is accepted", v1alpha1.ReviewProves, v1alpha1.VerificationPassed},
		{"the stronger check is refused too", v1alpha1.ReviewDoesNotProve, v1alpha1.VerificationUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name, r, pv1 := refusedRound(t, evaluationReport)
			e.recheckAsked(name)
			e.recheckStarted(name, pv1)
			e.recheckedRound(name, r, tc.answer)
			v := e.verification(name)
			if v.Round != 2 || v.State != tc.want {
				t.Fatalf("round 2 = %d %s %+v, want %s", v.Round, v.State, v.Outcomes, tc.want)
			}
			e.review(name, 8)
			if n := len(e.verifications(name)); n != 2 || e.verification(name).Round != 2 {
				t.Fatalf("%d verifications, round %d: a head set is rechecked once", n, e.verification(name).Round)
			}
			if cs := e.verifyComments(name, 2); len(cs) != 1 || !strings.Contains(cs[0].Body,
				"### maul: verification 2, attempt 1 (maul checked once more by itself: a check was not accepted)") {
				t.Errorf("round 2's comments %+v, want its heading to say maul checked once more", cs)
			}
			if tc.want == v1alpha1.VerificationPassed {
				return
			}
			v = e.verification(name)
			if v.Handoff == nil || !v.Handoff.Released || v.Handoff.Reason != v1alpha1.HandoffReasonUnconfirmed {
				t.Errorf("handoff %+v, want released unconfirmed once the recheck is spent", v.Handoff)
			}
			if st, _, ok := e.prVerifyPass(name).acceptanceStatus(); !ok || st.state != statusError {
				t.Errorf("maul/acceptance %+v (posted %v), want the final error", st, ok)
			}
		})
	}
}

// TestAcceptanceOvertakesTheRecheck: an approver's /maul accept of the
// outcome left open, before the recheck starts, makes the heads ready: no
// model round re-rolls them, and maul/acceptance is success.
func TestAcceptanceOvertakesTheRecheck(t *testing.T) {
	e, name, _, _ := refusedRound(t, evaluationReport)
	e.recheckAsked(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{ID: oHealthy,
			By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 98,
			HeadSetDigest: in.Status.Verification.HeadSetDigest})
	})
	e.review(name, 8)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("%d verifications: a recheck re-rolled heads an approver made ready", n)
	}
	p := e.prVerifyPass(name)
	if !outcomesReady(p.in.Status.Acceptance, p.in.Status.Verification) || p.rechecking(p.in.Status.Verification) {
		t.Error("not ready, or still rechecking, once the open outcome is accepted")
	}
	if st, _, ok := p.acceptanceStatus(); !ok || st.state != statusSuccess {
		t.Errorf("maul/acceptance %+v (posted %v), want success once accepted", st, ok)
	}
}

// TestRequestSpendsTheRecheck: an approver's /maul verify while the recheck
// is due runs that full round as theirs: the recheck is spent, its round's
// comment never says maul checked by itself, and no round follows it.
func TestRequestSpendsTheRecheck(t *testing.T) {
	e, name, _, _ := refusedRound(t, evaluationReport)
	e.recheckAsked(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: actorOf(approver).ID,
			At: metav1.NewTime(e.clock.Now()), CommentID: 4403}
	})
	pv := e.roundOf(name, 2)
	v := e.verification(name)
	if pv.Spec.Rung != v1alpha1.RungFull || pv.Annotations[annotationVerificationTrigger] != verificationByRequest {
		t.Fatalf("round 2 = %s by %q, want the request's full round", pv.Spec.Rung,
			pv.Annotations[annotationVerificationTrigger])
	}
	if esc := v.Escalation; esc == nil || esc.Round != 1 || esc.Recheck || rechecked(v) {
		t.Errorf("escalation %+v, want round 1's spent by the request, no recheck", esc)
	}
}

// TestFindingIsNeverRechecked: an outcome its evaluator saw fail beside a
// refused check (G1's shape: a finding and a refusal) asks for no recheck:
// the round stays as today, nothing more starts at its heads by itself, and
// the finding holds.
func TestFindingIsNeverRechecked(t *testing.T) {
	e, name, _, _ := refusedRound(t, failingReport)
	e.review(name, 8)
	v := e.verification(name)
	if v.Round != 1 || v.Escalation != nil || len(e.verifications(name)) != 1 {
		t.Fatalf("round %d, escalation %+v, %d verifications: a round with a finding was rechecked", v.Round,
			v.Escalation, len(e.verifications(name)))
	}
	if r := v.Outcomes[0]; r.ID != oVersion || r.Result == v1alpha1.OutcomePassed {
		t.Errorf("version outcome %+v, want the evaluator's finding kept, never passed", r)
	}
}

// TestAgentsRecheckThatCannotRunSettles: on a Project the agents tool pod
// evaluates, a recheck (or a dispute's replay) due at heads whose round can
// no longer run there (the accepted image gone) settles
// once, as an approver's request there does: no round starts, the
// judgement's outcomes stand, the recheck is no longer due, the hand-off is
// released as couldn't confirm and maul/acceptance is no longer pending.
// Left due, it held the hand-off and maul/acceptance pending for ever.
func TestAgentsRecheckThatCannotRunSettles(t *testing.T) {
	for _, tc := range []struct {
		name string
		// replay: a dispute's replay round is the follow-up due, not the
		// recheck.
		replay bool
	}{
		{name: "the recheck"},
		{name: "a dispute's replay", replay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			e.reviewReady(name, 4)
			pv := e.onlyVerification(name)
			// Round 1 judged Unconfirmed only on outcomes its evaluator
			// passed with no check accepted: the recheck is due (or, in the
			// replay case, the replay a dispute asked for).
			e.setStatus(name, func(in *v1alpha1.Intent) {
				v := in.Status.Verification
				v.State, v.Cause, v.Round, v.Rung = v1alpha1.VerificationUnconfirmed, "", 1, v1alpha1.RungFull
				v.Ref = &v1alpha1.ObjectReference{Name: pv.Name, UID: pv.UID}
				v.OutcomesRound, v.OutcomesHeadSetDigest, v.Outcomes = 1, v.HeadSetDigest, nil
				for _, o := range in.Status.Acceptance.Outcomes {
					v.Outcomes = append(v.Outcomes, unsureR(o.ID, acceptance.ReasonModelOnlyPass))
				}
				v.Escalation = &v1alpha1.VerificationEscalation{Round: 1, HeadSetDigest: v.HeadSetDigest,
					Recheck: true}
				if tc.replay {
					v.Escalation, v.Disputed = nil, []string{recheckHeads}
				}
			})
			if p := e.prVerifyPass(name); !p.followUpDue(p.in.Status.Verification) {
				t.Fatal("no follow-up is due (the case's premise)")
			}
			dropRunnerImages(t, e)
			e.review(name, 3)

			p := e.prVerifyPass(name)
			v := p.in.Status.Verification
			if v.State != v1alpha1.VerificationUnconfirmed || v.Cause != v1alpha1.SettleSandboxImage ||
				p.followUpDue(v) {
				t.Fatalf("verification %s (%s), follow-up due %v; want Unconfirmed (no image), nothing due", v.State,
					v.Cause, p.followUpDue(v))
			}
			if len(v.Outcomes) != len(p.in.Status.Acceptance.Outcomes) ||
				v.Outcomes[0].Reason != acceptance.ReasonModelOnlyPass {
				t.Errorf("outcomes %+v, want round 1's judgement kept", v.Outcomes)
			}
			if vs := e.verifications(name); len(vs) != 1 {
				t.Fatalf("%d verifications, want round 1 alone", len(vs))
			}
			if h := v.Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonUnconfirmed {
				t.Errorf("hand-off %+v, want released as couldn't confirm", h)
			}
			if st, _, ok := p.acceptanceStatus(); ok && st.state == statusPending {
				t.Errorf("maul/acceptance %+v, want no longer pending", st)
			}
			e.review(name, 3) // the comments catch up
			rv := e.get(name).ResourceVersion
			e.review(name, 3)
			if got := e.get(name).ResourceVersion; got != rv {
				t.Errorf("the intent was written again (%s -> %s) with nothing changed", rv, got)
			}
		})
	}
}

// TestRecheckWaitsForAHuman: the human always wins. A changes-requested
// review pending when the recheck is due starts no model round in its
// quiet window; the human's round comes first.
func TestRecheckWaitsForAHuman(t *testing.T) {
	e, name, _, _ := refusedRound(t, evaluationReport)
	e.recheckAsked(name)
	e.reviewOn(e.prNumber(name), 4401, "Please also log the version.")
	for range 10 {
		e.clock.Advance(10 * time.Second)
		e.mustIntent(name)
	}
	e.review(name, 6)
	for _, p := range e.verifications(name) {
		if p.Spec.Round > 1 {
			t.Fatalf("round %d (%s) started over a human's pending review", p.Spec.Round, p.Spec.Rung)
		}
	}
	if e.stageCount(name, v1alpha1.IntentStageRevise) == 0 {
		t.Error("the human's round never started")
	}
}
