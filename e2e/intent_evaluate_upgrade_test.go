// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package e2e

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestIntentEvaluateSupersededAtUpgrade (evaluator observed, W5): an intent
// whose evaluation passed is left as 0.14.15 leaves one, its record
// without the judge marker and carrying the cost ladder's records, a
// dispute, maxVerifications spent and an approver's /maul verify stop, and
// a checkreview run still running. intent-controller is restarted over it,
// as the upgrade rolls it. The checkreview run is aborted as retired; the
// earlier result is withdrawn, and the status comment says so while one
// fresh round, charged to no bound, evaluates the outcomes at the same
// heads, maul/acceptance pending meanwhile; once that round passes, the
// withdrawal is no longer stated, and what the approver set is kept.
func TestIntentEvaluateSupersededAtUpgrade(t *testing.T) {
	e := startEvaluate(t, evalOptions{})
	number, name, pr := e.toEvaluating(t)
	in := e.waitEval(t, name, "the first evaluation to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.Judge == v1alpha1.JudgeObserved
	})
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes seen to hold")
	first := in.Status.Verification.Round

	e.stopIntents()
	release := e.kubelet.holdToolsReady(t)
	review := e.oldCheckReviewRun(t, name, first)
	old := e.rewriteAsOldRelease(t, name)
	e.restartAgentsIntents(t, false)

	eventually(t, "the checkreview run aborted as retired", func() bool {
		var cur v1alpha1.IntentRun
		if err := e.cl.client.Get(context.Background(), client.ObjectKeyFromObject(review), &cur); err != nil {
			return false
		}
		return cur.Status.Phase == v1alpha1.RunFailed && strings.Contains(cur.Status.Detail, "retired")
	})
	in = e.waitEval(t, name, "the fresh round to start, marked", func(v *v1alpha1.IntentVerification) bool {
		return v.Judge == v1alpha1.JudgeObserved && v.Round == first+1 &&
			v.State == v1alpha1.VerificationEvaluating
	})
	e.checkSuperseded(t, in, old, first+1)
	e.waitStatus(t, pr.HeadSHA, "pending", "")
	e.waitStatusComment(t, name, number, "the status comment to state the withdrawal", func(body string) bool {
		return strings.Contains(body, templates.WithdrawnSentence)
	})

	release()
	in = e.waitEval(t, name, "the fresh round to pass", func(v *v1alpha1.IntentVerification) bool {
		return v.State == v1alpha1.VerificationPassed && v.OutcomesRound == first+1
	})
	if meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded) {
		t.Errorf("conditions %+v: the withdrawal outlived the fresh judgement", in.Status.Conditions)
	}
	if v := in.Status.Verification; v.Automatic != old.Automatic || v.AutoFix.Reason != v1alpha1.AutoFixHumanStop {
		t.Errorf("verification = %+v after the fresh round, want its bounds and the stop kept", v)
	}
	e.waitStatus(t, pr.HeadSHA, "success", "2/2 outcomes seen to hold")
	e.waitStatusComment(t, name, number, "the status comment to drop the withdrawal", func(body string) bool {
		return !strings.Contains(body, templates.WithdrawnSentence)
	})
	consistently(t, "no further round", func() bool { return len(e.verifications(t, name)) == 2 })
}

// oldCheckReviewRun creates the checkreview run 0.14.15 left for round of
// intent name, running with a Job that is no longer there to read.
func (e *evalEnv) oldCheckReviewRun(t *testing.T, name string, round int32) *v1alpha1.IntentRun {
	t.Helper()
	ctx := context.Background()
	evaluate := e.stageRuns(t, name, v1alpha1.IntentStageEvaluate)[0]
	review := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{
		Name:      v1alpha1.IntentRunName(name, v1alpha1.IntentStageCheckReview, round, "", 1),
		Namespace: namespace, Labels: evaluate.Labels, OwnerReferences: evaluate.OwnerReferences},
		Spec: *evaluate.Spec.DeepCopy()}
	review.Spec.Stage, review.Spec.Evaluation = v1alpha1.IntentStageCheckReview, nil
	if err := e.cl.client.Create(ctx, review); err != nil {
		t.Fatalf("create the checkreview run: %v", err)
	}
	review.Status.Phase = v1alpha1.RunRunning
	review.Status.JobRef = &v1alpha1.JobReference{Namespace: agentsNS, Name: name + "-old-checkreview"}
	if err := e.cl.client.Status().Update(ctx, review); err != nil {
		t.Fatalf("mark the checkreview run running: %v", err)
	}
	return review
}

// rewriteAsOldRelease rewrites intent name's verification as 0.14.15
// writes it: no judge marker, the cost ladder's model round and rung, a
// disputed check, maxVerifications spent, an approver's stop. It returns
// the record written.
func (e *evalEnv) rewriteAsOldRelease(t *testing.T, name string) v1alpha1.IntentVerification {
	t.Helper()
	most := e.project(t).Spec.Limits.MaxVerificationsLimit()
	var old v1alpha1.IntentVerification
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := e.intent(t, name)
		v := cur.Status.Verification
		now := metav1.Now().Rfc3339Copy()
		v.Judge = ""
		v.Rung = v1alpha1.RungFull
		v.ModelRound = &v1alpha1.VerificationModelRound{Round: v.Round, HeadSetDigest: v.HeadSetDigest,
			OutcomesDigest: cur.Status.Acceptance.OutcomesDigest, Clear: true}
		v.Disputed = []string{"sha256:" + strings.Repeat("d", 64)} // a disputed check's digest
		v.Automatic = most
		v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanStop, Since: &now,
			Stop: &v1alpha1.AutoFixStop{By: approver.Login, ByID: approver.ID, At: now, CommentID: 4242}}
		old = *v.DeepCopy()
		return e.cl.client.Status().Update(context.Background(), cur)
	}); err != nil {
		t.Fatalf("rewrite the record as 0.14.15 leaves it: %v", err)
	}
	return old
}

// checkSuperseded: in evaluates its fresh round, round's first attempt
// started by the supersede, the withdrawal recorded, the old judgement
// cleared, and old's bounds and stop kept.
func (e *evalEnv) checkSuperseded(t *testing.T, in *v1alpha1.Intent, old v1alpha1.IntentVerification,
	round int32) {
	t.Helper()
	v := in.Status.Verification
	if !meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded) {
		t.Errorf("conditions %+v: the withdrawal is not recorded", in.Status.Conditions)
	}
	if v.Automatic != old.Automatic || v.AutoFix.State != old.AutoFix.State ||
		v.AutoFix.Reason != old.AutoFix.Reason || v.AutoFix.Stop == nil ||
		v.AutoFix.Stop.CommentID != old.AutoFix.Stop.CommentID {
		t.Errorf("verification = %+v: the fresh round was charged, or the approver's stop lost", v)
	}
	if v.Rung != "" || v.ModelRound != nil || len(v.Disputed) != 0 || len(v.Outcomes) != 0 {
		t.Errorf("verification = %+v: the old judgement kept", v)
	}
	pvs := e.verifications(t, in.Name)
	if len(pvs) != 2 {
		t.Errorf("verifications = %d, want the first round's and the fresh one", len(pvs))
	}
	fresh, ok := pvs[v1alpha1.PreviewVerificationName(in.Name, round, 1)]
	if !ok || fresh.Annotations["maul.run/verification-trigger"] != "supersede" {
		t.Errorf("fresh round %+v, want round %d's first attempt, started by the supersede", fresh.ObjectMeta,
			round)
	}
}
