// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

// judgedAgentsRound drives an agents intent to a settled round 1 recorded
// in state at its heads, an outcome judged there, its verification
// Observed. It returns the intent and the round's verification.
func judgedAgentsRound(t *testing.T, state v1alpha1.IntentVerificationState) (*env, string,
	v1alpha1.PreviewVerification) {
	t.Helper()
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.State, v.Passed = state, 3
		v.Outcomes = []v1alpha1.OutcomeResult{{ID: "version-shown", Result: v1alpha1.OutcomePassed}}
		v.OutcomesRound, v.OutcomesHeadSetDigest = v.Round, v.HeadSetDigest
	})
	e.playTools(pv.Name, v1alpha1.VerificationObserved, "", false)
	return e, name, pv
}

// TestAgentsReturnKeepsAJudgedRound (S2-B7): a round already settled at
// the current heads when the agents host is switched off stays as it was
// recorded while the host is off, and once it returns nothing starts by
// itself at those heads: the rollback costs no round it did not strand. An
// approver's /maul verify still starts the next one.
func TestAgentsReturnKeepsAJudgedRound(t *testing.T) {
	for _, state := range []v1alpha1.IntentVerificationState{v1alpha1.VerificationPassed,
		v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationExhausted} {
		t.Run(string(state), func(t *testing.T) {
			e, name, pv := judgedAgentsRound(t, state)
			e.agentsOn(false)
			e.review(name, 2)
			if v := e.verification(name); v.State != state || v.Passed != 3 || len(v.JudgedOutcomes()) != 1 {
				t.Fatalf("verification = %+v with the host off, want %s kept", v, state)
			}
			e.agentsOn(true)
			e.reviewReady(name, 6)
			e.runRuns()
			if vs := e.verifications(name); len(vs) != 1 || vs[0].Name != pv.Name {
				t.Fatalf("verifications = %+v, want none started by itself", vs)
			}
			if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
				t.Fatalf("evaluate runs = %d, want none", len(runs))
			}
			v := e.verification(name)
			if v.State != state || v.Round != 1 || v.Ref == nil || v.Ref.Name != pv.Name || v.Passed != 3 ||
				len(v.JudgedOutcomes()) != 1 {
				t.Fatalf("verification = %+v, want round 1's %s kept", v, state)
			}
			e.gh.comment(approver, "/maul verify")
			e.settleActions(name)
			e.reviewReady(name, 6)
			if v := e.verification(name); v.Round != 2 || v.Ref == nil || v.Ref.Name == pv.Name || v.Requested != nil {
				t.Fatalf("verification = %+v, want the request's round 2", v)
			}
		})
	}
}

// TestAgentsHeadMoveWhileOffVerifiesOnReturn (S2-B7): a judged round whose
// heads move while the agents host is off no longer describes them: the
// intent is Off, and once the host returns the moved heads are verified.
func TestAgentsHeadMoveWhileOffVerifiesOnReturn(t *testing.T) {
	e, name, pv := judgedAgentsRound(t, v1alpha1.VerificationPassed)
	e.agentsOn(false)
	e.review(name, 2)
	moved := strings.Repeat("f", 40)
	e.pushHead(e.get(name).Status.PullRequests[0].Number, moved)
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationOff {
		t.Fatalf("verification = %s once the heads moved with the host off, want Off", v.State)
	}
	e.agentsOn(true)
	e.reviewReady(name, 8)
	want := headSetDigest([]v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: moved}})
	v := e.verification(name)
	if v.HeadSetDigest != want || v.Ref == nil || v.Ref.Name == pv.Name || v.Passed != 0 ||
		len(v.JudgedOutcomes()) != 0 {
		t.Fatalf("verification = %+v, want a round at the moved heads", v)
	}
}

// TestAgentsOffIntentResumesWhenTheHostReturns (S2-B4): an intent left Off
// at recorded heads while the host was off (by a release before S2-B7)
// starts over at those heads once the host returns (a Project that
// verifies again never leaves it Off, where neither a round nor an
// approver's /maul verify would ever start).
func TestAgentsOffIntentResumesWhenTheHostReturns(t *testing.T) {
	e, name, pv := judgedAgentsRound(t, v1alpha1.VerificationPassed)
	e.agentsOn(false)
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification.State = v1alpha1.VerificationOff })
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationOff {
		t.Fatalf("verification = %s with the host off, want Off kept", v.State)
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

// prEvaluationComment is the body of the sticky comment under the preview
// marker on name's first pull request, "" when there is none.
func (e *env) prEvaluationComment(name string) string {
	pr := e.get(name).Status.PullRequests[0]
	cs := e.previewComments(name, pr.Number)
	if len(cs) != 1 {
		return ""
	}
	return cs[0].Body
}

// TestAgentsRollbackKeepsTheLinklessViews (S2-B6): with the agents host
// switched off, an agents Project still has no preview, so its status
// comment and its pull request's comment stay the link-less evaluation
// views and say the stranded round's real outcome (couldn't confirm: maul
// could not run its checks): never the slot preview's "There is no preview
// of this pull request now.", never a /maul preview, never a Preview line.
func TestAgentsRollbackKeepsTheLinklessViews(t *testing.T) {
	for _, previews := range []bool{false, true} {
		t.Run(map[bool]string{false: "without previews", true: "with the preview projection on"}[previews],
			func(t *testing.T) {
				e, name, _ := evaluatingAgentsRound(t)
				e.review(name, 2)
				if c := e.prEvaluationComment(name); !strings.Contains(c, "maul holds its hand-off") {
					t.Fatalf("pull request comment before the flip = %q", c)
				}
				e.agentsOn(false)
				for _, s := range []*Settings{&e.project.Settings, &e.intent.Settings, &e.runs.Settings} {
					s.Previews = previews
				}
				e.review(name, 4)
				e.runRuns()
				e.review(name, 4)

				const unconfirmed = "maul couldn't confirm this: it could not run its checks."
				status := e.statusBody()
				if !strings.Contains(status, "**Evaluation:** "+unconfirmed) ||
					strings.Contains(status, "/maul preview") || strings.Contains(status, "**Preview:**") {
					t.Errorf("status comment with the host off:\n%s", status)
				}
				c := e.prEvaluationComment(name)
				if !strings.Contains(c, unconfirmed) || strings.Contains(strings.ToLower(c), "no preview") ||
					strings.Contains(c, "/maul preview") ||
					!strings.Contains(c, "Verification unavailable: maul could not run the checks") {
					t.Errorf("pull request comment with the host off:\n%s", c)
				}
				id := e.gh.comment(approver, "/maul preview")
				e.settleActions(name)
				if body := e.onlyReply(id); !strings.Contains(body, "has no preview") {
					t.Errorf("/maul preview reply with the host off = %s", body)
				}
			})
	}
}

// TestAgentsRollbackKeepsAReadyRoundReady (S2-B6 review): a round the
// agents host settled before it was switched off, every outcome confirmed
// or accepted, keeps its state and judgement at the same heads (S2-B7), so
// the link-less views keep saying it is ready, never "maul couldn't
// confirm this".
func TestAgentsRollbackKeepsAReadyRoundReady(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		applyJudgement(in, roundJudgement{decision: confirmedTwo}, time.Now())
	})
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul accept "+oManual)
	e.settleActions(name)
	e.review(name, 6)
	const ready = "Ready: maul checked the approved outcomes."
	if p := e.prVerifyPass(name); !outcomesReady(p.in.Status.Acceptance, p.in.Status.Verification) ||
		p.in.Status.Verification.Handoff == nil || !p.in.Status.Verification.Handoff.Released {
		t.Fatalf("verification = %+v, want every outcome confirmed or accepted, the hand-off released",
			p.in.Status.Verification)
	}
	if status := e.statusBody(); !strings.Contains(status, ready) {
		t.Fatalf("status comment before the flip:\n%s", status)
	}
	settled := e.verification(name).State
	e.agentsOn(false)
	e.review(name, 4)
	if v := e.verification(name); v.State != settled || !outcomesReady(e.get(name).Status.Acceptance, v) {
		t.Fatalf("verification = %+v with the host off, want its %s round kept (S2-B7)", v, settled)
	}
	if status := e.statusBody(); !strings.Contains(status, "**Evaluation:** "+ready) ||
		strings.Contains(status, "couldn't confirm") {
		t.Errorf("status comment with the host off:\n%s", status)
	}
	if c := e.prEvaluationComment(name); !strings.Contains(c, ready) || strings.Contains(c, "couldn't confirm") {
		t.Errorf("pull request comment with the host off:\n%s", c)
	}

	// A push while the host is off moves the heads past what was judged:
	// nothing checked them, so nothing says ready.
	e.pushHead(e.get(name).Status.PullRequests[0].Number, strings.Repeat("f", 40))
	// One pass more than before S2-B7: the moved heads first take the kept
	// round to Off, then the views are written.
	e.review(name, 8)
	if head := e.get(name).Status.PullRequests[0].HeadSHA; head != strings.Repeat("f", 40) {
		t.Fatalf("pull request head = %s, want the push", head)
	}
	if status := e.statusBody(); strings.Contains(status, ready) {
		t.Errorf("status comment after a push with the host off:\n%s", status)
	}
	if c := e.prEvaluationComment(name); strings.Contains(c, ready) {
		t.Errorf("pull request comment after a push with the host off:\n%s", c)
	}
}

// TestAgentsRollbackNeverSaysMaulChecks (S2-B6 review): with the agents
// host switched off nothing checks an agents Project's intents, so no view
// says maul checks them, also under an early hand-off, whose views carry
// no hand-off while the host is on: they say maul couldn't confirm it.
func TestAgentsRollbackNeverSaysMaulChecks(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage, func(p *v1alpha1.Project) {
		if p.Spec.Verification == nil {
			p.Spec.Verification = &v1alpha1.ProjectVerification{}
		}
		p.Spec.Verification.Handoff = v1alpha1.HandoffEarly
	})
	e.reviewReady(name, 4)
	if v := e.verification(name); v.Handoff == nil || v.Handoff.Mode != v1alpha1.HandoffEarly {
		t.Fatalf("verification = %+v, want an early hand-off", v)
	}
	e.agentsOn(false)
	e.review(name, 4)
	e.runRuns()
	e.review(name, 4)
	const unconfirmed = "maul couldn't confirm this: it could not run its checks."
	if status := e.statusBody(); !strings.Contains(status, "**Evaluation:** "+unconfirmed) ||
		strings.Contains(status, "maul checks") {
		t.Errorf("status comment with the host off:\n%s", status)
	}
	if c := e.prEvaluationComment(name); !strings.Contains(c, unconfirmed) || strings.Contains(c, "maul checks") {
		t.Errorf("pull request comment with the host off:\n%s", c)
	}
}
