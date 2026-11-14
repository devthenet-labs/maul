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
)

// G-B-LIB-1 review (PR #408): the paths by which a finding at heads H was
// still lost before a later round at H passed.

// TestCarriedNotesNeverCrowdOutARoundsOwn: the record holds at most
// maxAnomalies notes. A round at H whose own new note would land past that
// bound, behind notes earlier rounds made there, keeps it: the oldest
// carried note goes first, so the reviewer sees what the latest round saw.
func TestCarriedNotesNeverCrowdOutARoundsOwn(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	first := make([]v1alpha1.AnomalyRecord, 0, maxAnomalies)
	for i := range maxAnomalies {
		first = append(first, v1alpha1.AnomalyRecord{Summary: fmt.Sprintf("standing oddity %d", i)})
	}
	e.judgeAt(name, 1, heads, passingJudgement(t, first...))
	fresh := v1alpha1.AnomalyRecord{Summary: "a new oddity round 2 noticed", Ledger: []int32{3}}
	v := e.judgeAt(name, 2, heads, passingJudgement(t, fresh))
	if len(v.Anomalies) != maxAnomalies {
		t.Fatalf("%d notes recorded, want the schema's %d", len(v.Anomalies), maxAnomalies)
	}
	if last := v.Anomalies[maxAnomalies-1]; last.Summary != fresh.Summary || last.Round != 2 {
		t.Errorf("last note %+v, want round 2's own", last)
	}
	if v.Anomalies[0].Summary != "standing oddity 1" {
		t.Errorf("first note %+v, want the oldest carried one dropped", v.Anomalies[0])
	}
	if v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 2 = %s over notes alone, want Passed", v.State)
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
			At: metav1.NewTime(e.clock.Now()), HeadSetDigest: heads, Round: 1}
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

// TestJudgingARoundAgainKeepsItsOwnRecords: the same round judged again
// is that round's judgement, written anew: its own earlier finding does
// not hold it (only an earlier round's does), and its own note is
// recorded once.
func TestJudgingARoundAgainKeepsItsOwnRecords(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	check := savedCheck(t, versionCheck)
	failing := passingJudgement(t)
	failing.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced,
		check.Digest)
	e.judgeAt(name, 1, heads, failing)
	if v := e.judgeAt(name, 1, heads, passingJudgement(t)); v.Outcomes[0].Reason == acceptance.ReasonHeldFinding {
		t.Errorf("round 1 judged again is held by its own earlier record: %+v", v.Outcomes[0])
	}

	own := v1alpha1.AnomalyRecord{Summary: "round 2's oddity"}
	e.judgeAt(name, 2, heads, passingJudgement(t, own))
	v := e.judgeAt(name, 2, heads, passingJudgement(t, own))
	if n := len(v.Anomalies); n != 1 || v.Anomalies[0].Round != 2 {
		t.Errorf("round 2 judged again: notes %+v, want its own once", v.Anomalies)
	}
	if v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 2 judged again beside its note = %s, want Passed", v.State)
	}
}

// TestNoDefectReleasesOnlyTheJudgementItWasHanded: a no-defect conclusion
// is about the judgement its fix round was handed. A later round at the
// same heads that reproduces the failure again (maul and its fixer
// disagree: Exhausted) is a new finding, which the conclusion predates:
// a round after it that replays green there does not pass the outcome.
func TestNoDefectReleasesOnlyTheJudgementItWasHanded(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	check := savedCheck(t, versionCheck)
	failing := passingJudgement(t)
	failing.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultFailed, acceptance.ReasonReproduced,
		check.Digest)
	e.judgeAt(name, 1, heads, failing)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.NoDefect = &v1alpha1.NoDefectRecord{Run: name + "-rev-r2-a1", Reason: "fine",
			At: metav1.NewTime(e.clock.Now()), HeadSetDigest: heads, Round: 1}
	})
	e.judgeAt(name, 2, heads, failing)
	if v := e.judgeAt(name, 3, heads, passingJudgement(t)); v.State == v1alpha1.VerificationPassed ||
		v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
		t.Errorf("round 3 after round 2 reproduced the failure again: %s %+v; want held", v.State, v.Outcomes[0])
	}
}

// TestHeldEvaluatorFailIsAcceptedByCommand: an outcome held on the
// evaluator's word alone (no check behind it) has no dispute to release
// it, so an approver's /maul accept must: by its id, or with all.
func TestHeldEvaluatorFailIsAcceptedByCommand(t *testing.T) {
	for _, cmd := range []string{"/maul accept " + oVersion, "/maul accept all"} {
		t.Run(cmd, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			heads := e.verification(name).HeadSetDigest
			first := passingJudgement(t)
			first.decision.PerOutcome[0] = judged(oVersion, acceptance.ResultUnsure, acceptance.ReasonModelOnlyFail,
				"")
			e.judgeAt(name, 1, heads, first)
			if v := e.judgeAt(name, 2, heads, passingJudgement(t)); v.Outcomes[0].Reason !=
				acceptance.ReasonHeldFinding {
				t.Fatalf("round 2 = %+v, want held (the case's premise)", v.Outcomes[0])
			}
			replies := e.request(name, "issue", cmd)
			if len(replies) != 1 || !strings.Contains(replies[0], "is done") {
				t.Errorf("%s replies %q, want it accepted", cmd, replies)
			}
			if st, _, ok := e.prVerifyPass(name).acceptanceStatus(); !ok || st.state != statusSuccess {
				t.Errorf("maul/acceptance %+v (posted %v), want success once accepted", st, ok)
			}
		})
	}
}
