// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// TestNewRecordsAreBornObserved: a verification record intent-controller
// writes is marked from the start, and its first round is the counted
// automatic one.
func TestNewRecordsAreBornObserved(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.reviewUntil(name, func() bool { return e.get(name).Status.Verification != nil })
	if v := e.verification(name); v.Judge != v1alpha1.JudgeObserved {
		t.Fatalf("a new record's judge = %q, want %q", v.Judge, v1alpha1.JudgeObserved)
	}
	e.review(name, 3)
	if v := e.verification(name); v.Automatic != 1 {
		t.Errorf("automatic = %d, want the first round counted", v.Automatic)
	}
}

// TestWithdrawnRecordRunsAsAnyOther: a record 0.14.18 withdrew (no judge
// marker, nothing judged, no round in flight, its VerificationSuperseded
// condition standing, which nothing reads any more) is an ordinary
// verification waiting for its next round: a pending /maul verify runs as a
// request, consumed and uncounted, and with none the loop's automatic round
// runs, counted toward maxVerifications. Either round writes the marker, and
// the passes after it write nothing more.
func TestWithdrawnRecordRunsAsAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested bool
		trigger   string
		automatic int32
	}{
		{"a pending request", true, verificationByRequest, 1},
		{"no request", false, verificationByAutomatic, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newEvaluatorEnv(t, 0.4)
			e.review(name, 3)
			first := e.onlyVerification(name)
			e.playTools(first.Name, v1alpha1.VerificationRunning, toolPodIP, true)
			e.review(name, 1)
			e.runRuns()
			e.runRuns()
			e.review(name, 2)
			e.playTools(first.Name, v1alpha1.VerificationObserved, toolPodIP, true)
			e.reviewUntil(name, func() bool { return len(e.verification(name).Outcomes) > 0 })
			// As 0.14.18's withdrawal left it.
			e.setStatus(name, func(in *v1alpha1.Intent) {
				nv := in.Status.Verification
				nv.Judge, nv.State, nv.Cause = "", v1alpha1.VerificationWaitingForCI, ""
				nv.Ref, nv.Attempt, nv.InfraRetries, nv.RetryOwed = nil, 0, 0, false
				nv.Outcomes, nv.OutcomesRound, nv.OutcomesHeadSetDigest, nv.OutcomesEnvironment = nil, 0, "", ""
				nv.Anomalies = nil
				clearJudgement(nv)
				meta.SetStatusCondition(&in.Status.Conditions, metav1.Condition{Type: "VerificationSuperseded",
					Status: metav1.ConditionTrue, Reason: "VerificationRulesChanged",
					Message: "maul's rules for judging the approved outcomes changed"})
				if tc.requested {
					nv.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
						At: metav1.NewTime(e.clock.Now()), CommentID: 99}
				}
			})
			e.clock.Advance(time.Second)
			e.review(name, 6)
			v := e.verification(name)
			pvs := e.verifications(name)
			if len(pvs) != 2 || v.Round != 2 || v.State != v1alpha1.VerificationEvaluating {
				t.Fatalf("verifications %v, round %d %s: want round 2 evaluating", pvNames(pvs), v.Round, v.State)
			}
			if got := pvs[1].Annotations[annotationVerificationTrigger]; got != tc.trigger ||
				v.Automatic != tc.automatic || v.Requested != nil || v.Judge != v1alpha1.JudgeObserved {
				t.Errorf("trigger %q automatic %d requested %v judge %q; want %q, %d, none, marked", got,
					v.Automatic, v.Requested != nil, v.Judge, tc.trigger, tc.automatic)
			}
			rv := e.get(name).ResourceVersion
			e.review(name, 4)
			if after := e.get(name).ResourceVersion; after != rv {
				t.Errorf("the passes after the round kept writing the Intent: %s -> %s", rv, after)
			}
		})
	}
}

// pvNames are the verifications' names.
func pvNames(pvs []v1alpha1.PreviewVerification) []string {
	out := make([]string, 0, len(pvs))
	for _, pv := range pvs {
		out = append(out, pv.Name)
	}
	return out
}
