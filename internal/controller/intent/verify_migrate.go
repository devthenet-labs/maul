// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The supersede at upgrade ("evaluator observed", migration). An earlier
// release judged evaluations by saved checks, replays and a check reviewer;
// this one judges the evaluator's cited, verified calls
// (acceptance.Judge). A verification this release writes carries the judge
// marker (status.verification.judge, v1alpha1.JudgeObserved) from the
// start (newVerification, createVerification). One an earlier release
// wrote has none, and its result was reached under rules that no longer
// hold, so on the first pass over it (supersede, the top of
// syncEvaluation):
//
//  1. Nothing to withdraw (no round in flight, nothing judged, none of the
//     retired ladder's records): the marker alone is written. Its next
//     round is whatever is due, counted as always. Something to withdraw on
//     a Project that can run nothing now (no evaluable outcome, an
//     unverified evaluator) is left as it is, unmarked, until it can.
//  2. Otherwise the in-flight round is dropped (dropInFlight: an unsettled
//     PreviewVerification deleted; its evaluate run then stops,
//     evaluationEnded), the old judgement and the retired ladder's records
//     are cleared (withdraw), and the Intent's VerificationSuperseded
//     condition records the withdrawal. What the humans and the spend hold
//     is kept: the round number, a waiting request, the reset generation,
//     a cancelled run, the hand-off (a released one stays released), the
//     first green, automatic fixes (an approver's stop included), the
//     automatic count and time, the evaluation spend, a no-defect record,
//     and status.acceptance whole. syncAcceptance is not reused: it re-arms
//     automatic fixes and starts the bounds afresh.
//  3. The gate is then due (verifyDue: verifySupersede, ahead of every
//     branch but a round in flight) once: createVerification starts a new
//     round's first attempt, charged to neither maxVerifications nor the
//     request spacing, held to the evaluation ceiling (startEvaluation),
//     writing the marker in the same status update. Past the ceiling the
//     verification is Unconfirmed with the marker set (evaluationExhausted).
//  4. The comments say the earlier result was withdrawn
//     (templates.WithdrawnSentence) while the condition holds, until a
//     round judged under these rules records its own (applyJudgement) or a
//     new plan revision starts the record afresh (syncAcceptance).
//     maul/acceptance is pending meanwhile (WaitingForCI, then Evaluating).
//
// A checkreview run left by the earlier release is the run scheduler's
// (run_controller.go, retireRun): its Job deleted, the run Aborted.
// Stored reports are never judged again: their evaluators worked to other
// instructions, and replay rounds have none. After a rollback and a roll
// forward the marker is gone again, and the supersede runs once more.

// verificationBySupersede is the trigger annotation of the round the
// supersede started: neither the loop's automatic round nor an approver's
// request, so the request spacing (lastRequestAt) never counts it.
const verificationBySupersede = "supersede"

// reasonRulesChanged is the VerificationSuperseded condition's reason.
const reasonRulesChanged = "VerificationRulesChanged"

// newVerification is a verification record as this release starts one:
// judged under its rules from the start, so never superseded.
func newVerification() *v1alpha1.IntentVerification {
	return &v1alpha1.IntentVerification{Judge: v1alpha1.JudgeObserved}
}

// supersedePending reports in's withdrawal done and its fresh round not yet
// started: the condition holds and the marker is not written.
func supersedePending(in *v1alpha1.Intent) bool {
	v := in.Status.Verification
	return v != nil && v.Judge != v1alpha1.JudgeObserved &&
		meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}

// supersede is the first pass over a verification an earlier release
// wrote (no marker): the marker alone where there is nothing to withdraw,
// the withdrawal (step 2 above) where there is and canRun, nothing where
// there is and nothing can run. It reports
// whether it wrote the Intent's status. Once withdrawn, and until the
// fresh round writes the marker, it writes nothing more.
func (p *pass) supersede(ctx context.Context, canRun bool) (bool, error) {
	v := p.in.Status.Verification
	if v.Judge == v1alpha1.JudgeObserved {
		return false, nil
	}
	switch {
	case withdrawable(v) && canRun:
		return true, p.withdraw(ctx)
	case withdrawable(v), supersedePending(p.in):
		// Nothing can run: the earlier result stays as it is, unmarked,
		// and is withdrawn once the Project can evaluate (marking it would
		// leave it standing, read as current then).
		return false, nil
	}
	return true, p.update(ctx, func(cur *v1alpha1.Intent) error {
		cur.Status.Verification.Judge = v1alpha1.JudgeObserved
		return nil
	})
}

// withdrawable reports a verification holding something an earlier
// release's rules decided: a round in flight, a judgement, or a record of
// the retired cost ladder, dispute, suspicion or quarantine.
func withdrawable(v *v1alpha1.IntentVerification) bool {
	return v.Ref != nil || len(v.Outcomes) > 0 || v.OutcomesRound > 0 || v.OutcomesHeadSetDigest != "" ||
		v.OutcomesEnvironment != "" || len(v.Anomalies) > 0 || v.Passed > 0 || v.Failed > 0 ||
		v.Quarantined > 0 || len(v.QuarantinedIDs) > 0 || len(v.Disputed) > 0 || v.Suspected != nil ||
		v.Rung != "" || v.ModelRound != nil || v.Escalation != nil
}

// withdraw drops the in-flight round, clears what an earlier release
// judged, and records the withdrawal (step 2 above).
func (p *pass) withdraw(ctx context.Context) error {
	v := p.in.Status.Verification
	if v.Ref != nil {
		p.dropInFlight(ctx, *v.Ref)
	}
	p.r.log().LogAttrs(ctx, slog.LevelInfo, "an earlier release's verification is withdrawn; one fresh "+
		"evaluation is due", slog.String("intent", p.in.Name), slog.Int("round", int(v.Round)),
		slog.String("state", string(v.State)))
	return p.update(ctx, func(cur *v1alpha1.Intent) error {
		nv := cur.Status.Verification
		nv.State, nv.Cause = v1alpha1.VerificationWaitingForCI, ""
		nv.Ref, nv.Attempt, nv.InfraRetries, nv.RetryOwed = nil, 0, 0, false
		nv.Outcomes, nv.OutcomesRound, nv.OutcomesHeadSetDigest, nv.OutcomesEnvironment = nil, 0, "", ""
		nv.Anomalies, nv.Disputed, nv.Suspected = nil, nil, nil
		nv.Rung, nv.ModelRound, nv.Escalation = "", nil, nil
		clearJudgement(nv)
		countManual(nv, cur.Status.Acceptance)
		setCondition(cur, v1alpha1.ConditionVerificationSuperseded, metav1.ConditionTrue, reasonRulesChanged,
			"maul's rules for judging the approved outcomes changed: the earlier result was withdrawn, and one "+
				"fresh evaluation, which spends none of maxVerifications, is due")
		return nil
	})
}

// clearSuperseded drops cur's withdrawal: a round judged under these rules
// recorded its own result, or a new plan revision starts afresh.
func clearSuperseded(cur *v1alpha1.Intent) {
	meta.RemoveStatusCondition(&cur.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}

// supersededSet is the intents this process completed a supersede for, by
// UID: the maul.intent.verification_superseded gauge.
type supersededSet struct {
	mu   sync.Mutex
	uids map[types.UID]struct{}
}

// verificationsSuperseded is this process's supersededSet.
var verificationsSuperseded = &supersededSet{uids: map[types.UID]struct{}{}}

// add counts in once.
func (s *supersededSet) add(in *v1alpha1.Intent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uids[in.UID] = struct{}{}
}

// count is how many intents were counted.
func (s *supersededSet) count() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.uids))
}

// supersededGauge registers the maul.intent.verification_superseded gauge
// once, on censusScope's meter, observing verificationsSuperseded.
var supersededGauge = sync.OnceFunc(func() {
	m := otel.Meter(censusScope)
	g, err := m.Int64ObservableGauge("maul.intent.verification_superseded",
		metric.WithDescription("open intents whose earlier release's verification result intent-controller "+
			"withdrew and evaluated afresh (or found past the evaluation ceiling) since it started"))
	if err != nil {
		otel.Handle(err)
		return
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(g, verificationsSuperseded.count())
		return nil
	}, g); err != nil {
		otel.Handle(err)
	}
})

// ReportSuperseded publishes the maul.intent.verification_superseded gauge
// for the life of the process.
func ReportSuperseded() { supersededGauge() }
