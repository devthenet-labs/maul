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
)

// G-B-LIB-1 review (PR #408): the paths by which a finding at heads H was
// still lost before a later round at H passed, on the observed judgement.

// failingVersion is a report seeing version-shown not hold, still-healthy
// hold, each citing a head http call maul counts.
var failingVersion = map[string]report.Verdict{oVersion: report.VerdictFail, oHealthy: report.VerdictPass}

// TestCarriedNotesNeverCrowdOutARoundsOwn: the record holds at most
// maxAnomalies notes. A round at H whose own new note would land past that
// bound, behind notes earlier rounds made there, keeps it: the oldest
// carried note goes first, so the reviewer sees what the latest round saw.
func TestCarriedNotesNeverCrowdOutARoundsOwn(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	first := make([]report.Anomaly, 0, maxAnomalies)
	for i := range maxAnomalies {
		first = append(first, report.Anomaly{Summary: fmt.Sprintf("standing oddity %d", i), Ledger: []int32{5}})
	}
	e.judgeAt(name, 1, heads, observedReport(passing, first...))
	fresh := report.Anomaly{Summary: "a new oddity round 2 noticed", Ledger: []int32{3}}
	v := e.judgeAt(name, 2, heads, observedReport(passing, fresh))
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

// TestNoDefectReleasesOnlyWhatTheFixerWasHanded: a fix round's no-defect
// conclusion is about the failures it was handed: the outcomes the
// judgement it was handed saw not to hold. An outcome that judgement held
// from an earlier round at the same heads was never handed to it, so the
// conclusion does not release it.
func TestNoDefectReleasesOnlyWhatTheFixerWasHanded(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	e.judgeAt(name, 1, heads, observedReport(failingVersion))
	healthFails := map[string]report.Verdict{oVersion: report.VerdictPass, oHealthy: report.VerdictFail}
	if v := e.judgeAt(name, 2, heads, observedReport(healthFails)); v.Outcomes[0].Reason !=
		acceptance.ReasonHeldFinding || v.Outcomes[1].Reason != acceptance.ReasonSeenNotToHold {
		t.Fatalf("round 2 = %+v, want version held and health seen not to hold (the case's premise)", v.Outcomes)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.NoDefect = &v1alpha1.NoDefectRecord{Run: name + "-rev-r3-a1", Reason: "fine",
			At: metav1.NewTime(e.clock.Now()), HeadSetDigest: heads, Round: 2}
	})
	v := e.judgeAt(name, 3, heads, observedReport(passing))
	if v.State == v1alpha1.VerificationPassed {
		t.Errorf("round 3 = Passed: the no-defect conclusion released %s, which its fixer was never handed",
			oVersion)
	}
	for _, r := range v.Outcomes {
		switch r.ID {
		case oHealthy:
			if r.Result != v1alpha1.OutcomePassed {
				t.Errorf("%s = %+v, want passed: the fixer was handed it and found no defect", r.ID, r)
			}
		case oVersion:
			if r.Reason != acceptance.ReasonHeldFinding {
				t.Errorf("%s = %+v, want held", r.ID, r)
			}
		}
	}
}

// TestUncitedFailHoldsNothing: an evaluator's fail that cites no call maul
// counts (only a relay's, a refused one, or none) is no finding: the
// outcome is unsure (no-valid-evidence) and a later round at the same heads
// that sees it hold passes it.
func TestUncitedFailHoldsNothing(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	ev := observedReport(failingVersion)
	ev.Verdicts[0].Ledger = []int32{9}
	if v := e.judgeAt(name, 1, heads, ev); v.State == v1alpha1.VerificationFailing ||
		v.Outcomes[0].Reason != acceptance.ReasonNoValidEvidence {
		t.Fatalf("round 1 = %s %+v, want unsure on no valid evidence", v.State, v.Outcomes[0])
	}
	if v := e.judgeAt(name, 2, heads, observedReport(passing)); v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 2 = %s %+v, want Passed: an uncited fail holds nothing", v.State, v.Outcomes[0])
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
	e.judgeAt(name, 1, heads, observedReport(failingVersion))
	if v := e.judgeAt(name, 1, heads, observedReport(passing)); v.Outcomes[0].Reason == acceptance.ReasonHeldFinding {
		t.Errorf("round 1 judged again is held by its own earlier record: %+v", v.Outcomes[0])
	}

	own := report.Anomaly{Summary: "round 2's oddity", Ledger: []int32{2}}
	e.judgeAt(name, 2, heads, observedReport(passing, own))
	v := e.judgeAt(name, 2, heads, observedReport(passing, own))
	if n := len(v.Anomalies); n != 1 || v.Anomalies[0].Round != 2 {
		t.Errorf("round 2 judged again: notes %+v, want its own once", v.Anomalies)
	}
	if v.State != v1alpha1.VerificationPassed {
		t.Errorf("round 2 judged again beside its note = %s, want Passed", v.State)
	}
}

// TestNoDefectReleasesOnlyTheJudgementItWasHanded: a no-defect conclusion
// is about the judgement its fix round was handed. A later round at the
// same heads that sees the failure again (maul's evaluator and its fixer
// disagree: Exhausted) is a new finding, which the conclusion predates:
// a round after it that sees the outcome hold there does not pass it.
func TestNoDefectReleasesOnlyTheJudgementItWasHanded(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	heads := e.verification(name).HeadSetDigest
	e.judgeAt(name, 1, heads, observedReport(failingVersion))
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.NoDefect = &v1alpha1.NoDefectRecord{Run: name + "-rev-r2-a1", Reason: "fine",
			At: metav1.NewTime(e.clock.Now()), HeadSetDigest: heads, Round: 1}
	})
	if v := e.judgeAt(name, 2, heads, observedReport(failingVersion)); v.State != v1alpha1.VerificationExhausted {
		t.Fatalf("round 2 seeing it fail again after no defect = %s, want Exhausted", v.State)
	}
	if v := e.judgeAt(name, 3, heads, observedReport(passing)); v.State == v1alpha1.VerificationPassed ||
		v.Outcomes[0].Reason != acceptance.ReasonHeldFinding {
		t.Errorf("round 3 after round 2 saw the failure again: %s %+v; want held", v.State, v.Outcomes[0])
	}
}

// TestHeldFailIsAcceptedByCommand: an outcome held at its heads releases
// to an approver's /maul accept, by its id or with all.
func TestHeldFailIsAcceptedByCommand(t *testing.T) {
	for _, cmd := range []string{"/maul accept " + oVersion, "/maul accept all"} {
		t.Run(cmd, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			heads := e.verification(name).HeadSetDigest
			e.judgeAt(name, 1, heads, observedReport(failingVersion))
			if v := e.judgeAt(name, 2, heads, observedReport(passing)); v.Outcomes[0].Reason !=
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
