// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/httpstep"
	"github.com/devthenet-labs/maul/internal/report"
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
	*v1alpha1.IntentAcceptance, checksJudgement) {
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
	j := checksJudgement{decision: acceptance.Decision{Env: httpstep.EnvOK, Handoff: acceptance.HandoffUnconfirmed},
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
		mutate func(*v1alpha1.IntentVerification, *v1alpha1.IntentAcceptance, *checksJudgement)
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
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
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
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.clear = false // a fail verdict or a contrary entry, on any outcome
			}},
		{name: "a failing environment", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.decision.Env = httpstep.EnvAppFailure
			}},
		{name: "no evaluator report", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.evaluation = nil
			}},
		{name: "a replay round", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.replay = true
			}},
		{name: "a fix asked for", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.decision.Fix = &acceptance.FixRequest{}
			}},
		{name: "a dispute", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.disputes = []report.Dispute{{}}
			}},
		{name: "a dispute's replay due", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.decision.ReplayRound = true
				v.Disputed = []string{recheckHeads}
			}},
		{name: "a suspected defect this round", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.decision.Suspected = &acceptance.FixRequest{}
			}},
		{name: "a replay round asked for", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(_ *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, j *checksJudgement) {
				j.decision.ReplayRound = true
			}},
		{name: "disputed checks on record", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, _ *checksJudgement) {
				v.Disputed = []string{recheckHeads}
			}},
		{name: "a suspected defect on record at the heads", rs: []v1alpha1.OutcomeResult{refused, passedR("a")},
			mutate: func(v *v1alpha1.IntentVerification, _ *v1alpha1.IntentAcceptance, _ *checksJudgement) {
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
