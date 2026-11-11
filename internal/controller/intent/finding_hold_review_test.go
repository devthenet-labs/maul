// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
)

// G-B-LIB-1 review (PR #408): the paths by which a finding at heads H was
// still lost before a later round at H passed.

// TestAcceptedAnomaliesNeverCrowdOutAnOpenOne: the record holds at most
// maxAnomalies. A round at H whose own new anomaly would land past that
// bound, behind anomalies an approver already accepted there, must keep it:
// dropping it would let the round pass on a finding it just made.
func TestAcceptedAnomaliesNeverCrowdOutAnOpenOne(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	first := make([]v1alpha1.AnomalyRecord, 0, maxAnomalies)
	for i := range maxAnomalies {
		first = append(first, v1alpha1.AnomalyRecord{Summary: fmt.Sprintf("standing oddity %d", i)})
	}
	e.judgeAt(name, 1, heads, passingJudgement(t, first...))
	e.setStatus(name, func(in *v1alpha1.Intent) {
		for i := range in.Status.Verification.Anomalies {
			in.Status.Verification.Anomalies[i].Accepted = &v1alpha1.AnomalyAcceptance{By: approver, ByID: 7,
				At: metav1.NewTime(e.clock.Now()), CommentID: 99}
		}
	})
	fresh := v1alpha1.AnomalyRecord{Summary: "a new defect round 2 noticed", Ledger: []int32{3}}
	v := e.judgeAt(name, 2, heads, passingJudgement(t, fresh))
	if v.State == v1alpha1.VerificationPassed {
		t.Errorf("round 2 = Passed over its own new anomaly, dropped behind %d accepted ones", maxAnomalies)
	}
	if open := openAnomalies(v.Anomalies); len(open) != 1 || open[0].Summary != fresh.Summary {
		t.Errorf("open anomalies %+v, want round 2's own", open)
	}
	if len(v.Anomalies) > maxAnomalies {
		t.Errorf("%d anomalies recorded, over the schema's %d", len(v.Anomalies), maxAnomalies)
	}
	if outcomesReady(e.prVerifyPass(name).in.Status.Acceptance, v) {
		t.Error("ready over round 2's own anomaly")
	}
}

// TestDisputeRoundKeepsTheHold: a round at H whose evaluator disputes the
// held outcome's check with a replacement (Decide's Retire) releases
// nothing by itself: the retirement may yet be refused (retirable), and
// the round's own record must keep the hold for the next round there. Only
// the check gone from the store as read releases it.
func TestDisputeRoundKeepsTheHold(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	check := savedCheck(t, versionCheck)
	first := passingJudgement(t)
	first.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced,
		check.Digest)
	e.judgeAt(name, 1, heads, first)

	dispute := passingJudgement(t)
	dispute.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonDisputed, "")
	dispute.decision.Retire = []string{check.Digest}
	dispute.stored = []v1alpha1.SavedCheck{check} // the retirement refused: still stored
	if v := e.judgeAt(name, 2, heads, dispute); v.State == v1alpha1.VerificationPassed {
		t.Fatalf("round 2 (dispute) = Passed")
	}
	// Round 3 at H: the check, still stored, replays green.
	still := passingJudgement(t)
	still.stored = []v1alpha1.SavedCheck{check}
	if v := e.judgeAt(name, 3, heads, still); v.State == v1alpha1.VerificationPassed ||
		v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
		t.Errorf("round 3 with the check still stored: %s %+v; want held", v.State, v.Outcomes[0])
	}
	// Round 4 after the retirement went through: the check is gone.
	gone := passingJudgement(t)
	gone.stored = []v1alpha1.SavedCheck{savedCheck(t, healthCheck)}
	if v := e.judgeAt(name, 4, heads, gone); v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 4 with the check retired: %s %+v; want Passed", v.State, v.Outcomes[0])
	}
}

// TestNoDefectReleasesOnlyWhatTheFixerWasHanded: a fix round's no-defect
// conclusion is about the failures it was handed. An outcome suspected at
// the same heads that no approver confirmed was never handed to it, so the
// conclusion does not release it.
func TestNoDefectReleasesOnlyWhatTheFixerWasHanded(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	check, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	first := passingJudgement(t)
	first.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced,
		check.Digest)
	first.decision.PerOutcome[1] = judged(oHealthy, acceptance.ResultUnsure, acceptance.ReasonSuspectedDefect,
		health.Digest)
	e.judgeAt(name, 1, heads, first)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.NoDefect = &v1alpha1.NoDefectRecord{Run: name + "-rev-r2-a1", Reason: "fine",
			At: metav1.NewTime(e.clock.Now()), HeadSetDigest: heads}
	})
	v := e.judgeAt(name, 2, heads, passingJudgement(t))
	if v.State == v1alpha1.VerificationPassed {
		t.Errorf("round 2 = Passed: the no-defect conclusion released %s, which its fixer was never handed",
			oHealthy)
	}
	for _, r := range v.Outcomes {
		switch r.ID {
		case oVersion:
			if r.Result != v1alpha1.OutcomePassed {
				t.Errorf("%s = %+v, want passed: the fixer was handed it and found no defect", r.ID, r)
			}
		case oHealthy:
			if r.Reason != acceptance.ReasonHeldFinding {
				t.Errorf("%s = %+v, want held", r.ID, r)
			}
		}
	}
}

// TestModelFailVerdictHoldsItsHeads: an evaluator that found an outcome
// failing at H with no check maul could confirm (model-only-fail,
// evaluator-saw-failure) made a finding at H as surely as one that
// reported an anomaly there: a later round at H whose evaluator saw
// nothing wrong does not pass the outcome.
func TestModelFailVerdictHoldsItsHeads(t *testing.T) {
	for _, reason := range []string{acceptance.ReasonModelOnlyFail, acceptance.ReasonEvaluatorSawFailure} {
		t.Run(reason, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			heads := e.verification(name).HeadSetDigest
			first := passingJudgement(t)
			first.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultUnsure, reason, "")
			e.judgeAt(name, 1, heads, first)
			v := e.judgeAt(name, 2, heads, passingJudgement(t))
			if v.State == v1alpha1.VerificationPassed || v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
				t.Errorf("round 2 at H: %s %+v; want the outcome held", v.State, v.Outcomes[0])
			}
			if !openFindingAt(v, e.prVerifyPass(name).in.Status.Acceptance, heads) {
				t.Error("no open finding: a replay round could follow")
			}
		})
	}
}
