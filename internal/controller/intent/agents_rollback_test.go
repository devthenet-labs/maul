// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// evaluatingAgentsRound drives an agents intent to an open round whose
// evaluator runs: the tool pod's address and key in place, the evaluate
// Job launched and held running. It returns the intent and the round's
// verification.
func evaluatingAgentsRound(t *testing.T) (*env, string, v1alpha1.PreviewVerification) {
	t.Helper()
	e, name := newAgentsEnv(t, repoImage)
	e.jobs.statusFn = func(spec jobs.Spec) (jobs.Status, bool) {
		return jobs.Status{}, spec.Phase == "evaluate" // held running
	}
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.toolsSecret(&pv, "")
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	runs := e.runsOf(name, v1alpha1.IntentStageEvaluate)
	if len(runs) != 1 || runs[0].Status.Phase != v1alpha1.RunRunning || len(e.specsOf("evaluate")) != 1 {
		t.Fatalf("evaluate runs = %+v, want one running", runs)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Handoff == nil ||
		v.Handoff.Released {
		t.Fatalf("verification = %+v, want Evaluating with the hand-off held", v)
	}
	return e, name, pv
}

// TestAgentsRollbackSettlesTheOpenRound (S2-B4): with the agents host
// switched off while a round is open, nothing will ever settle its
// verification (the host is its status's only writer and is not
// registered), so the intent settles the round itself: Unavailable, never a
// pass or a fail and no fix round, the hand-off released as couldn't
// confirm, the unsettled verification deleted, the issue told, and the
// running evaluator cancelled. Later passes with the host still off keep
// it Unavailable (never Off).
func TestAgentsRollbackSettlesTheOpenRound(t *testing.T) {
	e, name, pv := evaluatingAgentsRound(t)
	e.agentsOn(false)
	e.review(name, 4)

	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnavailable || v.UnavailableReason != "" || v.Requested != nil {
		t.Fatalf("verification = %+v, want Unavailable", v)
	}
	if v.Passed != 0 || v.Failed != 0 || len(v.Failing) != 0 {
		t.Fatalf("verification = %+v, want no judgement", v)
	}
	if h := v.Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonUnconfirmed {
		t.Fatalf("hand-off = %+v, want released as couldn't confirm", h)
	}
	var gone v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&pv), &gone); !kerrors.IsNotFound(err) {
		t.Fatalf("the stranded verification is still there (%s): %v", gone.Status.Phase, err)
	}
	if runs := e.runsOf(name, v1alpha1.IntentStageRevise); len(runs) != 0 {
		t.Fatalf("revise runs = %d, want no fix round", len(runs))
	}
	if cs := e.verifyComments(name, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "unavailable") {
		t.Fatalf("verification comments = %+v, want one saying it is unavailable", cs)
	}

	e.runRuns()
	run := e.runsOf(name, v1alpha1.IntentStageEvaluate)[0]
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeAborted {
		t.Fatalf("evaluate run = %s/%s (%s), want aborted", run.Status.Phase, run.Status.Outcome, run.Status.Detail)
	}
	if run.Status.JobRef == nil || !slices.Contains(e.jobs.deleted, run.Status.JobRef.Name) {
		t.Fatalf("the evaluator's Job was not deleted: %v", e.jobs.deleted)
	}

	e.review(name, 4)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable {
		t.Fatalf("verification = %s while the host stays off, want Unavailable", v.State)
	}
}

// TestAgentsBackOnAfterRollback (S2-B4): once the agents host returns, an
// intent whose round the rollback settled is not stuck: no round starts by
// itself at the same heads, and an approver's /maul verify starts one.
func TestAgentsBackOnAfterRollback(t *testing.T) {
	e, name, pv := evaluatingAgentsRound(t)
	e.agentsOn(false)
	e.review(name, 4)
	e.runRuns()
	e.agentsOn(true)
	e.reviewReady(name, 4)
	if vs := e.verifications(name); len(vs) != 0 {
		t.Fatalf("verifications = %+v, want none started by itself", vs)
	}
	e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.reviewReady(name, 6)
	vs := e.verifications(name)
	if len(vs) != 1 || vs[0].Name == pv.Name || vs[0].Spec.Round != 2 {
		t.Fatalf("verifications = %+v, want the request's round 2", vs)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating || v.Requested != nil {
		t.Fatalf("verification = %+v, want the request's round Evaluating", v)
	}
}

// TestAgentsOffIntentResumesWhenTheHostReturns (S2-B4): an intent that was
// Off at recorded heads while the host was off starts over at those heads
// once the host returns (a Project that verifies again never leaves it Off,
// where neither a round nor an approver's /maul verify would ever start).
func TestAgentsOffIntentResumesWhenTheHostReturns(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.State, v.Passed = v1alpha1.VerificationPassed, 3
	})
	e.playTools(pv.Name, v1alpha1.VerificationObserved, "", false)
	e.agentsOn(false)
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationOff {
		t.Fatalf("verification = %s with the host off and no round open, want Off", v.State)
	}
	e.agentsOn(true)
	e.reviewReady(name, 6)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationEvaluating || v.Ref == nil || v.Ref.Name == pv.Name || v.Round != 2 {
		t.Fatalf("verification = %+v, want round 2 evaluating at the same heads", v)
	}
	if v.Passed != 0 {
		t.Fatalf("verification = %+v, want the judgement from before the rollback dropped", v)
	}
}

// TestOffResumesWhenAProjectVerifiesAgain: the resume is the verification
// loop's, not the agents host's: a slot intent left Off at its recorded
// heads, on a pass that verifies it again, starts over at those heads.
func TestOffResumesWhenAProjectVerifiesAgain(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.judged(answering(200))
	pv := e.onlyVerification(name)
	if v := e.verification(name); v.State != v1alpha1.VerificationPassed {
		t.Fatalf("verification = %s, want Passed", v.State)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification.State = v1alpha1.VerificationOff })
	e.review(name, 4)
	v := e.verification(name)
	if v.Round != 2 || v.State != v1alpha1.VerificationVerifying || v.Ref == nil || v.Ref.Name == pv.Name ||
		v.Passed != 0 {
		t.Fatalf("verification = %+v, want round 2 verifying at the same heads", v)
	}
}
