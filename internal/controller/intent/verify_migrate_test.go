// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/templates"
)

// The supersede at upgrade (verify_migrate.go): an open intent the evaluator
// verifies whose verification 0.14.15 recorded (no judge marker) has its
// earlier result withdrawn once, its in-flight round dropped and its
// checkreview run aborted, and gets one fresh evaluation that spends none of
// maxVerifications; what the humans and the spend hold is kept.

// checkReviewJob is the Job a 0.14.15 checkreview run left running.
const checkReviewJob = "old-checkreview-job"

// oldRelease drives a new intent under the evaluator through a judged first
// round, then rewrites what is on record as 0.14.15 leaves it mid-way
// through a replay round: a round-2 replay PreviewVerification running, the
// cost ladder's records, a suspected defect, quarantined and disputed
// outcomes, an approver's /maul verify stop, maxVerifications spent, a
// released hand-off, a no-defect record, no judge marker, and a checkreview
// run whose Job still runs. mutate shapes the old verification further.
func oldRelease(t *testing.T, mutate ...func(*v1alpha1.IntentVerification)) (*env, string) {
	t.Helper()
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
	if v := e.verification(name); v.Round != 1 || v.Judge != v1alpha1.JudgeObserved {
		t.Fatalf("first round: %+v", v)
	}

	// The replay round 0.14.15 started at the same heads.
	replay := first.DeepCopy()
	replay.ObjectMeta = metav1.ObjectMeta{Name: v1alpha1.PreviewVerificationName(name, 2, 1), Namespace: testNS,
		Labels: first.Labels, Annotations: map[string]string{annotationVerificationTrigger: verificationByAutomatic},
		OwnerReferences: first.OwnerReferences}
	replay.Spec.Round, replay.Spec.Attempt, replay.Spec.Run = 2, 1, "abc123"
	replay.Spec.Rung = v1alpha1.RungReplay
	replay.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning,
		Tools: &v1alpha1.ToolsStatus{PodIP: toolPodIP, Ready: true}}
	if err := e.c.Create(context.Background(), replay); err != nil {
		t.Fatal(err)
	}

	// A checkreview run of round 1, its Job still running.
	evaluate := e.runsOf(name, v1alpha1.IntentStageEvaluate)[0]
	review := &v1alpha1.IntentRun{ObjectMeta: metav1.ObjectMeta{
		Name:      v1alpha1.IntentRunName(name, v1alpha1.IntentStageCheckReview, 1, "", 1),
		Namespace: testNS, Labels: evaluate.Labels, OwnerReferences: evaluate.OwnerReferences},
		Spec: *evaluate.Spec.DeepCopy()}
	review.Spec.Stage, review.Spec.Evaluation = v1alpha1.IntentStageCheckReview, nil
	if err := e.c.Create(context.Background(), review); err != nil {
		t.Fatal(err)
	}
	review.Status.Phase = v1alpha1.RunRunning
	review.Status.JobRef = &v1alpha1.JobReference{Name: checkReviewJob}
	if err := e.c.Status().Update(context.Background(), review); err != nil {
		t.Fatal(err)
	}
	e.jobs.mu.Lock()
	e.jobs.status[checkReviewJob] = jobs.Status{}
	e.jobs.mu.Unlock()

	now := metav1.NewTime(e.clock.Now().Truncate(time.Second))
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.Judge = ""
		v.State, v.Round, v.Attempt = v1alpha1.VerificationEvaluating, 2, 1
		v.Ref = &v1alpha1.ObjectReference{Name: replay.Name, UID: e.pvNamed(replay.Name).UID}
		v.Rung = v1alpha1.RungReplay
		v.ModelRound = &v1alpha1.VerificationModelRound{Round: 1, HeadSetDigest: v.HeadSetDigest,
			OutcomesDigest: in.Status.Acceptance.OutcomesDigest, Clear: true}
		v.Escalation = &v1alpha1.VerificationEscalation{Round: 1, HeadSetDigest: v.HeadSetDigest}
		v.Disputed = []string{"sha256:" + strings.Repeat("d", 64)} // a disputed check's digest
		v.Quarantined, v.QuarantinedIDs = 1, []string{oHealthy}
		v.Suspected = &v1alpha1.SuspectedDefect{Verification: first.Name, HeadSetDigest: v.HeadSetDigest,
			Outcomes: []string{oVersion}, RecordedAt: now}
		v.Anomalies = []v1alpha1.AnomalyRecord{{Summary: "a stray 500", Ledger: []int32{2}, Round: 1}}
		v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanStop, Since: &now,
			Stop: &v1alpha1.AutoFixStop{By: approver, ByID: 7, At: now, CommentID: 41}}
		v.Automatic, v.LastAutomaticAt = e.project0().Spec.Limits.MaxVerificationsLimit(), &now
		v.EvaluationCostMicroUSD = 400000
		v.Handoff = &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated, Released: true, ReleasedAt: &now,
			Reason: v1alpha1.HandoffReasonHuman}
		v.FirstGreenAt = &now
		v.NoDefect = &v1alpha1.NoDefectRecord{Run: "a-fix-run", Reason: "nothing to fix", At: now,
			HeadSetDigest: v.HeadSetDigest, Round: 1}
		v.CancelRun = "a-cancelled-run"
		v.Reset = v1alpha1.ResetState{Generation: 0}
		for _, m := range mutate {
			m(v)
		}
	})
	return e, name
}

// preserved is what the supersede keeps of a verification and its
// acceptance, as JSON: byte-equal before and after.
func preserved(t *testing.T, in *v1alpha1.Intent) string {
	t.Helper()
	v := in.Status.Verification
	b, err := json.Marshal(struct {
		Round                  int32
		Requested              *v1alpha1.VerifyRequest
		Reset                  v1alpha1.ResetState
		CancelRun              string
		Handoff                *v1alpha1.HandoffState
		FirstGreenAt           *metav1.Time
		AutoFix                v1alpha1.AutoFix
		Automatic              int32
		LastAutomaticAt        *metav1.Time
		EvaluationCostMicroUSD int64
		NoDefect               *v1alpha1.NoDefectRecord
		HeadSetDigest          string
		Heads                  []v1alpha1.VerificationHead
		Acceptance             *v1alpha1.IntentAcceptance
	}{v.Round, v.Requested, v.Reset, v.CancelRun, v.Handoff, v.FirstGreenAt, v.AutoFix, v.Automatic,
		v.LastAutomaticAt, v.EvaluationCostMicroUSD, v.NoDefect, v.HeadSetDigest, v.Heads, in.Status.Acceptance})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// superseded reports the Intent's VerificationSuperseded condition true.
func superseded(in *v1alpha1.Intent) bool {
	return meta.IsStatusConditionTrue(in.Status.Conditions, v1alpha1.ConditionVerificationSuperseded)
}

// TestSupersedeWithdrawsTheOldResult: the first pass over a 0.14.15 record
// deletes its in-flight replay round, clears the old judgement and the cost
// ladder's records, keeps every field the humans and the spend hold
// byte-equal, and records the withdrawal; the run scheduler aborts the
// checkreview run and deletes its Job; the next passes create exactly one
// fresh round, a new round's first attempt, spending none of
// maxVerifications, with the marker in the same write; later passes do
// nothing more.
func TestSupersedeWithdrawsTheOldResult(t *testing.T) {
	e, name := oldRelease(t)
	before := preserved(t, e.get(name))
	counted := verificationsSuperseded.count()

	e.review(name, 1)
	checkWithdrawn(t, e, name, before)
	e.runRuns()
	checkRetiredRun(t, e, name)

	e.reviewUntil(name, func() bool { return len(e.verifications(name)) == 2 })
	automatic := checkFreshRound(t, e, name)
	if got := verificationsSuperseded.count() - counted; got != 1 {
		t.Errorf("superseded gauge moved by %d, want 1", got)
	}

	e.review(name, 4)
	if pvs := e.verifications(name); len(pvs) != 2 {
		t.Errorf("verifications = %v after more passes, want the one fresh round", pvNames(pvs))
	}
	if got := e.verification(name).Automatic; got != automatic {
		t.Errorf("automatic moved to %d on later passes", got)
	}
	if got := verificationsSuperseded.count() - counted; got != 1 {
		t.Errorf("superseded gauge moved by %d after more passes, want 1", got)
	}
}

// checkWithdrawn: intent name's earlier result is withdrawn, its in-flight
// replay round deleted and the withdrawal recorded, the marker not yet
// written, and what the supersede keeps byte-equal to before.
func checkWithdrawn(t *testing.T, e *env, name, before string) {
	t.Helper()
	in := e.get(name)
	v := in.Status.Verification
	if got := preserved(t, in); got != before {
		t.Errorf("preserved fields changed:\n got %s\nwant %s", got, before)
	}
	if v.Judge == v1alpha1.JudgeObserved {
		t.Errorf("marker written before the fresh round exists")
	}
	if !superseded(in) {
		t.Errorf("conditions %+v: the withdrawal is not recorded", in.Status.Conditions)
	}
	if oldJudgementKept(v) {
		t.Errorf("old judgement kept: %+v", v)
	}
	if v.State != v1alpha1.VerificationWaitingForCI {
		t.Errorf("state = %s, want WaitingForCI until the fresh round starts", v.State)
	}
	if pvs := e.verifications(name); len(pvs) != 1 || pvs[0].Spec.Round != 1 {
		t.Errorf("verifications = %d (%v), want the in-flight replay round deleted", len(pvs), pvNames(pvs))
	}
}

// oldJudgementKept reports any field of an earlier release's judgement, or
// of its round in flight, still on v, listed apart from the code's own
// withdrawable.
func oldJudgementKept(v *v1alpha1.IntentVerification) bool {
	return v.Ref != nil || v.Attempt != 0 || v.RetryOwed || v.InfraRetries != 0 || len(v.Outcomes) != 0 ||
		v.OutcomesRound != 0 || v.OutcomesHeadSetDigest != "" || v.OutcomesEnvironment != "" || v.Passed != 0 ||
		v.Failed != 0 || len(v.Disputed) != 0 || v.Rung != "" || v.ModelRound != nil || v.Escalation != nil ||
		v.Suspected != nil || v.Quarantined != 0 || len(v.QuarantinedIDs) != 0 || len(v.Anomalies) != 0
}

// checkRetiredRun: intent name's checkreview run is aborted as retired and
// its Job deleted.
func checkRetiredRun(t *testing.T, e *env, name string) {
	t.Helper()
	review := e.runsOf(name, v1alpha1.IntentStageCheckReview)[0]
	if review.Status.Phase != v1alpha1.RunFailed || review.Status.Outcome != OutcomeAborted ||
		!strings.Contains(review.Status.Detail, "retired") {
		t.Errorf("checkreview run = %s %s %q, want aborted as retired", review.Status.Phase, review.Status.Outcome,
			review.Status.Detail)
	}
	if !slices.Contains(e.jobs.deleted, checkReviewJob) {
		t.Errorf("deleted Jobs %v, want the checkreview run's %s", e.jobs.deleted, checkReviewJob)
	}
}

// checkFreshRound: intent name evaluates one fresh round, round 3's first
// attempt, started by the supersede, marked, charged to no bound, with the
// approver's stop and the released hand-off kept. It returns the automatic
// count.
func checkFreshRound(t *testing.T, e *env, name string) int32 {
	t.Helper()
	v := e.verification(name)
	fresh := e.verifications(name)[1]
	if fresh.Spec.Round != 3 || fresh.Spec.Attempt != 1 || fresh.Spec.Rung != v1alpha1.RungFull {
		t.Errorf("fresh round %s: round %d attempt %d rung %s, want round 3's first, full", fresh.Name,
			fresh.Spec.Round, fresh.Spec.Attempt, fresh.Spec.Rung)
	}
	if got := fresh.Annotations[annotationVerificationTrigger]; got != verificationBySupersede {
		t.Errorf("fresh round's trigger = %q, want %q", got, verificationBySupersede)
	}
	if v.Judge != v1alpha1.JudgeObserved || v.State != v1alpha1.VerificationEvaluating || v.Ref == nil ||
		v.Ref.UID != fresh.UID {
		t.Errorf("verification = %+v, want Evaluating the fresh round, marked", v)
	}
	if v.Automatic != e.project0().Spec.Limits.MaxVerificationsLimit() {
		t.Errorf("automatic = %d: the fresh round was charged to maxVerifications", v.Automatic)
	}
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.AutoFix.Reason != v1alpha1.AutoFixHumanStop {
		t.Errorf("auto-fix = %+v, want the approver's stop kept", v.AutoFix)
	}
	if v.Handoff == nil || !v.Handoff.Released {
		t.Errorf("hand-off = %+v, want it still released", v.Handoff)
	}
	return v.Automatic
}

// TestSupersedeIsShownUntilJudged: while the fresh round runs, the issue's
// status comment says the earlier result was withdrawn and maul/acceptance
// is pending; once the fresh round is judged, neither says so.
func TestSupersedeIsShownUntilJudged(t *testing.T) {
	e, name := oldRelease(t)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.review(name, 1)
	e.runRuns()
	e.reviewUntil(name, func() bool { return len(e.verifications(name)) == 2 })
	// The status comment is edited on a pass that changes nothing else.
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "**Phase:** `InReview`") })
	if body := e.statusBody(); !strings.Contains(body, templates.WithdrawnSentence) {
		t.Errorf("status comment does not state the withdrawal:\n%s", body)
	}
	if s := e.latestAcceptance(head); s.State != statusPending {
		t.Errorf("maul/acceptance = %+v, want pending", s)
	}

	fresh := e.verifications(name)[1]
	e.playTools(fresh.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.playTools(fresh.Name, v1alpha1.VerificationObserved, toolPodIP, true)
	e.review(name, 3)
	in := e.get(name)
	if v := in.Status.Verification; v.OutcomesRound != 3 || len(v.Outcomes) == 0 {
		t.Fatalf("fresh round not judged: %+v", v)
	}
	if superseded(in) {
		t.Errorf("conditions %+v: the withdrawal outlived the fresh judgement", in.Status.Conditions)
	}
	// reviewUntil fails if the comment never stops stating it.
	e.reviewUntil(name, func() bool { return !strings.Contains(e.statusBody(), templates.WithdrawnSentence) })
}

// TestSupersedeAtTheEvaluationCeiling: with the evaluation ceiling already
// spent, the supersede starts no round: the verification is Unconfirmed
// with the marker set and the withdrawal stated, and no retry of the
// withdrawn round is owed, so a raised limit never resumes it.
func TestSupersedeAtTheEvaluationCeiling(t *testing.T) {
	e, name := oldRelease(t)
	proj := e.project0()
	proj.Spec.Limits.MaxEvaluateCostMicroUSD = new(int64(100000))
	if err := e.c.Update(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	e.review(name, 4)
	e.reviewUntil(name, func() bool { return strings.Contains(e.statusBody(), "**Phase:** `InReview`") })
	in := e.get(name)
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationUnconfirmed || v.Judge != v1alpha1.JudgeObserved {
		t.Fatalf("verification = %+v, want Unconfirmed and marked", v)
	}
	if !superseded(in) {
		t.Errorf("conditions %+v: the withdrawal is not recorded", in.Status.Conditions)
	}
	if pvs := e.verifications(name); len(pvs) != 1 {
		t.Errorf("verifications = %v, want no round past the ceiling", pvNames(pvs))
	}
	if body := e.statusBody(); !strings.Contains(body, templates.WithdrawnSentence) {
		t.Errorf("status comment does not state the withdrawal:\n%s", body)
	}
	p := &pass{r: e.intent, set: e.intent.Settings, in: in, proj: e.project0(), now: e.clock.Now()}
	p.runs = e.runPtrs(name)
	if kind, err := p.verifyDue(context.Background(), v); err != nil || kind == verifyRetry ||
		kind == verifySupersede {
		t.Errorf("verifyDue = %v, %v: the withdrawn round is still owed", kind, err)
	}
	e.review(name, 3)
	if got := e.get(name).Status.Verification; got.State != v1alpha1.VerificationUnconfirmed ||
		len(e.verifications(name)) != 1 {
		t.Errorf("a second pass moved it: %+v", got)
	}
}

// TestSupersedeServesAWaitingRequest: an approver's /maul verify waiting
// when the upgrade lands is kept through the withdrawal, then served by the
// fresh round, which is not charged to the request spacing.
func TestSupersedeServesAWaitingRequest(t *testing.T) {
	e, name := oldRelease(t, func(v *v1alpha1.IntentVerification) {
		v.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7, At: *v.LastAutomaticAt,
			CommentID: 99}
	})
	e.review(name, 1)
	if v := e.verification(name); v.Requested == nil || v.Requested.CommentID != 99 {
		t.Fatalf("request = %+v after the withdrawal, want it kept", v.Requested)
	}
	e.runRuns()
	e.reviewUntil(name, func() bool { return len(e.verifications(name)) == 2 })
	v := e.verification(name)
	if v.Requested != nil {
		t.Errorf("request = %+v, want the fresh round to serve it", v.Requested)
	}
	if got := e.verifications(name)[1].Annotations[annotationVerificationTrigger]; got == verificationByRequest {
		t.Errorf("fresh round's trigger = %q: charged to the request spacing", got)
	}
}

// TestNothingToWithdrawIsOnlyMarked: a 0.14.15 record that never started a
// round has nothing to withdraw: it is marked, no withdrawal is recorded,
// and its first round is the automatic one, counted as always.
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

// pvNames are the verifications' names.
func pvNames(pvs []v1alpha1.PreviewVerification) []string {
	out := make([]string, 0, len(pvs))
	for _, pv := range pvs {
		out = append(out, pv.Name)
	}
	return out
}
