// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The judge marker (verify_migrate.go): every verification this release
// writes is marked; one an earlier release left unmarked with nothing judged
// (the check-era census refuses any other) is only marked; and an intent
// 0.14.18 withdrew, whose fresh round it started, says so until that round
// is judged.

// superseded reports the Intent's VerificationSuperseded condition true.
func superseded(in *v1alpha1.Intent) bool {
	return meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}

// TestWithdrawalIsShownUntilJudged: an intent 0.14.18 superseded and whose
// fresh round it started (marked, the VerificationSuperseded condition
// standing) has its status comment and preview comment say the earlier
// result was withdrawn, and maul/acceptance pending, while that round runs;
// once it is judged, neither comment says so and the condition is gone.
func TestWithdrawalIsShownUntilJudged(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	first := e.onlyVerification(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		meta.SetStatusCondition(&in.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionVerificationSuperseded, Status: metav1.ConditionTrue, Reason: reasonRulesChanged,
			Message: "maul's rules for judging the approved outcomes changed"})
	})
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), templates.WithdrawnSentence) })
	if s := e.latestAcceptance(head); s.State != statusPending {
		t.Errorf("maul/acceptance = %+v, want pending", s)
	}
	pr := e.get(name).Status.PullRequests[0].Number
	e.reviewUntil(name, func() bool { return strings.Contains(e.prVerifyBody(name, pr), templates.WithdrawnSentence) })

	e.playTools(first.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.review(name, 2)
	e.playTools(first.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.reviewUntil(name, func() bool { return len(e.verification(name).Outcomes) > 0 })
	if in := e.get(name); superseded(in) {
		t.Errorf("conditions %+v: the withdrawal outlived the fresh judgement", in.Status.Conditions)
	}
	// reviewUntil fails if the comments never stop stating it.
	e.reviewUntil(name, func() bool { return !strings.Contains(e.statusBody(), templates.WithdrawnSentence) })
	e.reviewUntil(name, func() bool { return !strings.Contains(e.prVerifyBody(name, pr), templates.WithdrawnSentence) })
}

// TestNothingToWithdrawIsOnlyMarked: an earlier release's record that never
// started a round has nothing to withdraw: it is marked, no withdrawal is
// recorded, and its first round is the automatic one, counted as always.
func TestNothingToWithdrawIsOnlyMarked(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.reviewUntil(name, func() bool { return e.get(name).Status.Verification != nil })
	if pvs := e.verifications(name); len(pvs) != 0 {
		t.Fatalf("verifications = %v before the record could be rewritten", pvNames(pvs))
	}
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification.Judge = "" })
	e.review(name, 4)
	in := e.get(name)
	v := in.Status.Verification
	if v.Judge != v1alpha1.JudgeObserved || superseded(in) {
		t.Errorf("judge %q, conditions %+v: want marked, nothing withdrawn", v.Judge, in.Status.Conditions)
	}
	pv := e.onlyVerification(name)
	if pv.Spec.Round != 1 || pv.Annotations[annotationVerificationTrigger] != verificationByAutomatic ||
		v.Automatic != 1 {
		t.Errorf("first round %s trigger %q, automatic %d: want the counted automatic round", pv.Name,
			pv.Annotations[annotationVerificationTrigger], v.Automatic)
	}
}

// TestNewRecordsAreBornObserved: a verification record intent-controller
// writes is marked from the start, so a new intent is never superseded and
// its first round is the counted automatic one.
func TestNewRecordsAreBornObserved(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.reviewUntil(name, func() bool { return e.get(name).Status.Verification != nil })
	if v := e.verification(name); v.Judge != v1alpha1.JudgeObserved {
		t.Fatalf("a new record's judge = %q, want %q", v.Judge, v1alpha1.JudgeObserved)
	}
	e.review(name, 3)
	if in := e.get(name); superseded(in) {
		t.Errorf("conditions %+v: a new intent was superseded", in.Status.Conditions)
	}
	if v := e.verification(name); v.Automatic != 1 {
		t.Errorf("automatic = %d, want the first round counted", v.Automatic)
	}
}

// prVerifyBody is maul's preview comment on pull request n, "" while there
// is none: its verification section is gated by its own digest.
func (e *env) prVerifyBody(name string, n int64) string {
	cs := e.previewComments(name, n)
	if len(cs) != 1 {
		return ""
	}
	return cs[0].Body
}

// pvNames are the verifications' names.
func pvNames(pvs []v1alpha1.PreviewVerification) []string {
	out := make([]string, 0, len(pvs))
	for _, pv := range pvs {
		out = append(out, pv.Name)
	}
	return out
}
