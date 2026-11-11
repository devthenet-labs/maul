// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/httpstep"
)

// The cost ladder's replay rung (minimal S3, rung.go), end to end through
// the intent reconciler and the run scheduler, preview-controller played.

// anomalyReport is an evaluator's report that both outcomes hold, with a
// look-around anomaly it cites.
const anomalyReport = `---
verdicts:
  - id: version-shown
    verdict: pass
    ledger: [3]
  - id: still-healthy
    verdict: pass
    ledger: [4]
anomalies:
  - summary: the console logs an uncaught TypeError on /
    route: /
    ledger: [2]
---
Both outcomes hold; the home page throws in the console.
`

// modelRound drives intent name's next round, a full one, to its
// judgement: the stored checks version and health replay with results, the
// evaluator reports as e's evaluate Jobs do. It returns the round's
// PreviewVerification.
func (e *env) modelRound(name string, round int32,
	version v1alpha1.CheckRunResult) v1alpha1.PreviewVerification {
	e.t.Helper()
	pv := e.roundOf(name, round)
	if pv.Spec.Rung != v1alpha1.RungFull {
		e.t.Fatalf("round %d is %s, want full", pv.Spec.Round, pv.Spec.Rung)
	}
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.runRuns()
	e.runRuns()
	e.replayedBoth(name, pv.Name, version)
	e.settleReviews(name)
	return pv
}

// replayedBoth plays preview-controller collecting round pvName with the
// stored version check replayed with its result version, the health check
// green.
func (e *env) replayedBoth(name, pvName string, version v1alpha1.CheckRunResult) {
	e.t.Helper()
	_, stored := e.checkStore(name)
	runs := make([]v1alpha1.CheckRun, 0, len(stored))
	for i, c := range stored {
		res := v1alpha1.CheckRunPass
		if c.Outcome == oVersion {
			res = version
		}
		runs = append(runs, v1alpha1.CheckRun{CheckDigest: c.Digest, Result: res, Token: "t" + c.Digest[7:13],
			Seq: int32(1 + i)})
	}
	e.replayed(pvName, runs...)
}

// roundOf runs review passes until the intent's round want has its first
// attempt, and returns it.
func (e *env) roundOf(name string, want int32) v1alpha1.PreviewVerification {
	e.t.Helper()
	var out v1alpha1.PreviewVerification
	e.reviewUntil(name, func() bool {
		for _, pv := range e.verifications(name) {
			if pv.Spec.Round == want && pv.Spec.Attempt == 1 {
				out = pv
				return true
			}
		}
		return false
	})
	return out
}

// replayRound drives intent name's next round, which must be a replay, to
// its judgement, and returns it.
func (e *env) replayRound(name string, round int32,
	version v1alpha1.CheckRunResult) v1alpha1.PreviewVerification {
	e.t.Helper()
	pv := e.roundOf(name, round)
	if pv.Spec.Rung != v1alpha1.RungReplay {
		e.t.Fatalf("round %d is %s, want replay", pv.Spec.Round, pv.Spec.Rung)
	}
	if v := e.verification(name); v.Rung != v1alpha1.RungReplay {
		e.t.Errorf("status rung %q, want replay", v.Rung)
	}
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 2)
	e.replayedBoth(name, pv.Name, version)
	e.reviewUntil(name, func() bool { return e.verification(name).State != v1alpha1.VerificationEvaluating })
	return pv
}

// passedModelRound is an intent whose round 1, a full round, passed: both
// outcomes on their stored checks, the manual one accepted, the evaluator
// agreeing with every citation and raising nothing.
func passedModelRound(t *testing.T, cost float64) (*env, string) {
	t.Helper()
	e, name := newEvaluatorEnv(t, cost)
	e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
	e.acceptManual(name)
	e.modelRound(name, 1, v1alpha1.CheckRunPass)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationPassed {
		t.Fatalf("round 1 = %s %+v, want Passed", v.State, v.Outcomes)
	}
	if m := v.ModelRound; m == nil || m.Round != 1 || m.HeadSetDigest != v.HeadSetDigest || !m.Clear ||
		m.OutcomesDigest != e.get(name).Status.Acceptance.OutcomesDigest {
		t.Fatalf("model round = %+v, want round 1's, clear, at its heads and outcomes", m)
	}
	return e, name
}

// acceptManual records an approver's acceptance of the manual outcome at
// the intent's heads.
func (e *env) acceptManual(name string) {
	e.t.Helper()
	e.review(name, 3)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Accepted = append(in.Status.Acceptance.Accepted, v1alpha1.AcceptanceRecord{ID: oManual,
			By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99,
			HeadSetDigest: in.Status.Verification.HeadSetDigest})
	})
}

// request has an approver comment body on surface ("issue" or "pull
// request") and settles it; it returns maul's replies to it.
func (e *env) request(name, surface, body string) []string {
	e.t.Helper()
	e.clock.Advance(verifyInterval)
	id := e.commandOn(name, surface, body)
	e.settleActions(name)
	if surface != "issue" {
		return e.prReplies(e.get(name).Status.PullRequests[0].Number, id)
	}
	var out []string
	for _, c := range e.gh.ownComments() {
		if strings.Contains(markerOf(c.Body), "comment-"+itoa(id)) {
			out = append(out, c.Body)
		}
	}
	return out
}

// stageCount is the number of intent name's runs of stage.
func (e *env) stageCount(name string, stage v1alpha1.IntentStage) int {
	return len(e.runsOf(name, stage))
}

// TestRequestAtUnchangedHeadsReplays: after a clear model round, an
// approver's /maul verify at the same heads is a replay round: the saved
// checks only, no evaluate and no checkreview run (no model spend), judged
// by Decide on the store and the replay, so it passes; the status records
// the rung, and the verification comment says no model ran.
func TestRequestAtUnchangedHeadsReplays(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	evaluates, reviews := e.stageCount(name, v1alpha1.IntentStageEvaluate),
		e.stageCount(name, v1alpha1.IntentStageCheckReview)
	spent := e.prVerifyPass(name).evaluationSpend()

	e.request(name, "issue", "/maul verify")
	pv := e.replayRound(name, 2, v1alpha1.CheckRunPass)
	if len(pv.Spec.Checks) != 2 {
		t.Errorf("replay round checks %+v, want both stored ones", pv.Spec.Checks)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationPassed || v.Round != 2 {
		t.Fatalf("round 2 = %s %+v, want Passed on the replay", v.State, v.Outcomes)
	}
	if n, m := e.stageCount(name, v1alpha1.IntentStageEvaluate),
		e.stageCount(name, v1alpha1.IntentStageCheckReview); n != evaluates || m != reviews {
		t.Errorf("evaluate runs %d->%d, checkreview runs %d->%d: a replay round runs no model", evaluates, n, reviews,
			m)
	}
	if got := e.prVerifyPass(name).evaluationSpend(); got != spent {
		t.Errorf("evaluation spend %d->%d: a replay round costs nothing", spent, got)
	}
	if v.ModelRound == nil || v.ModelRound.Round != 1 {
		t.Errorf("model round %+v, want round 1's still", v.ModelRound)
	}
	e.reviewUntil(name, func() bool { return len(e.verifyComments(name, 2)) == 1 })
	if body := e.verifyComments(name, 2)[0].Body; !strings.Contains(body,
		"### maul: verification 2, attempt 1 (replayed saved checks, no model)") {
		t.Errorf("round 2's comment does not say it replayed with no model:\n%s", body)
	}
	for _, c := range e.verifyComments(name, 1) {
		if strings.Contains(c.Body, "no model") {
			t.Errorf("round 1's comment says no model:\n%s", c.Body)
		}
	}
}

// escalatedRound is an intent whose round 1 passed, whose round 2, a replay
// an approver's /maul verify fix asked for, found the version check failing
// (unsure: no evaluator agreed, never failed, no fix), and whose round 3 is
// the one full round that escalation asked for at the same heads, its
// evaluator reporting report; judged.
func escalatedRound(t *testing.T, report string) (*env, string) {
	t.Helper()
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify fix")
	e.replayRound(name, 2, v1alpha1.CheckRunFail)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || v.Failed != 0 {
		t.Fatalf("replay round = %s, %d failed; want Unconfirmed, nothing failed", v.State, v.Failed)
	}
	if r := v.Outcomes[0]; r.Result != v1alpha1.OutcomeUnsure || r.Reason != acceptance.ReasonEvaluatorDisagrees {
		t.Errorf("version outcome %+v, want unsure: no evaluator agreed", r)
	}
	if esc := v.Escalation; esc == nil || esc.Round != 2 || esc.HeadSetDigest != v.HeadSetDigest {
		t.Fatalf("escalation %+v, want round 2's at its heads", esc)
	}
	if why := e.prVerifyPass(name).gaveUp(e.get(name).Status.Acceptance, v); why != "" {
		t.Errorf("gave up (%s) with the escalation due", why)
	}
	if e.stageCount(name, v1alpha1.IntentStageRevise) != 0 {
		t.Fatal("a replay round started a fix")
	}
	e.evaluateReporting(report)
	pv := e.modelRound(name, 3, v1alpha1.CheckRunFail)
	if pv.Spec.HeadSetDigest != v.HeadSetDigest || pv.Annotations[annotationVerificationTrigger] !=
		verificationByAutomatic {
		t.Fatalf("escalation at %s by %q, want round 3 at the same heads, automatic", pv.Spec.HeadSetDigest,
			pv.Annotations[annotationVerificationTrigger])
	}
	if n := e.stageCount(name, v1alpha1.IntentStageEvaluate); n != 2 {
		t.Errorf("evaluate runs = %d, want round 1's and the escalation's", n)
	}
	e.reviewUntil(name, func() bool { return e.verification(name).State != v1alpha1.VerificationEvaluating })
	return e, name
}

// TestUnsureReplayEscalatesToAFix: a replay round whose saved check fails
// is unsure, never failed; it asks for one automatic full round at the same
// heads, and the hand-off waits for it. That round's evaluator agrees the
// outcome fails: Failing, and the fix round starts as it does today.
func TestUnsureReplayEscalatesToAFix(t *testing.T) {
	e, name := escalatedRound(t, failingReport)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing && v.State != v1alpha1.VerificationFixing {
		t.Fatalf("escalated round = %s, want Failing", v.State)
	}
	e.reviewUntil(name, func() bool { return e.stageCount(name, v1alpha1.IntentStageRevise) > 0 })
}

// TestEscalationWaitsForAHuman: the human always wins. An approver's
// changes-requested review pending (its quiet window included) when an
// unsure replay round has asked for its escalation starts no model round,
// although round 1 released the link (so only the follow-up guard holds
// it); the human's round comes first.
func TestEscalationWaitsForAHuman(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify")
	pv := e.roundOf(name, 2)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.replayedBoth(name, pv.Name, v1alpha1.CheckRunFail)
	asked := func() bool { v := e.verification(name); return v.Escalation != nil && v.Escalation.Round == 2 }
	for range 12 {
		if asked() {
			break
		}
		e.clock.Advance(10 * time.Second)
		e.mustIntent(name)
	}
	if v := e.verification(name); !asked() || v.Round != 2 || v.Handoff == nil || !v.Handoff.Released {
		t.Fatalf("verification %+v, want round 2 judged, its escalation asked, the link released", v)
	}
	// The review arrives with the escalation due; within its quiet window
	// (reviewQuiet) the passes poll the pull request and start nothing.
	e.reviewOn(e.prNumber(name), 4401, "Please also log the version.")
	for range 10 {
		e.clock.Advance(10 * time.Second)
		e.mustIntent(name)
	}
	e.review(name, 6)
	for _, p := range e.verifications(name) {
		if p.Spec.Round > 2 {
			t.Fatalf("round %d (%s) started over a human's pending review", p.Spec.Round, p.Spec.Rung)
		}
	}
	if n := e.stageCount(name, v1alpha1.IntentStageEvaluate); n != 1 {
		t.Errorf("evaluate runs = %d, want round 1's alone while a human's feedback waits", n)
	}
	if e.stageCount(name, v1alpha1.IntentStageRevise) == 0 {
		t.Error("the human's round never started")
	}
}

// TestRequestWhileEscalationDueIsFull: an approver's /maul verify that
// arrives while the escalation an unsure replay round asked for has not
// started yet runs that full round: it never replays again and spends the
// head set's escalation unrun.
func TestRequestWhileEscalationDueIsFull(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify")
	e.replayRound(name, 2, v1alpha1.CheckRunFail)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		if v.Escalation == nil || v.Escalation.Round != 2 {
			t.Fatalf("escalation %+v, want round 2's", v.Escalation)
		}
		v.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: actorOf(approver).ID,
			At: metav1.NewTime(e.clock.Now()), CommentID: 4402}
	})
	if pv := e.roundOf(name, 3); pv.Spec.Rung != v1alpha1.RungFull {
		t.Fatalf("round 3 = %s, want the escalation's full round", pv.Spec.Rung)
	}
}

// TestEscalationNotSpacedWhileFixesHalted: with automatic fixes halted
// (an approver's /maul verify stop), a new head set's automatic round waits
// verifyInterval after the last; the escalation, the replay round's own
// follow-up, does not: it starts on the next passes.
func TestEscalationNotSpacedWhileFixesHalted(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify")
	e.replayRound(name, 2, v1alpha1.CheckRunFail)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v, now := in.Status.Verification, metav1.NewTime(e.clock.Now())
		v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanStop, Since: &now}
		v.LastAutomaticAt = &now
	})
	e.review(name, 2)
	if v := e.verification(name); v.Round != 3 || v.Rung != v1alpha1.RungFull {
		t.Fatalf("round %d %s two minutes after the unsure replay, want the escalation (round 3, full)", v.Round,
			v.Rung)
	}
}

// TestEscalationHeldToMaxVerifications: the escalation is an automatic
// round, so it counts against the Project's maxVerifications. With the
// plan revision's automatic rounds spent, an unsure replay round's
// escalation never starts, and the hand-off does not wait for it.
func TestEscalationHeldToMaxVerifications(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify")
	e.replayRound(name, 2, v1alpha1.CheckRunFail)
	limit := e.prVerifyPass(name).proj.Spec.Limits.MaxVerificationsLimit()
	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification.Automatic = limit })
	e.review(name, 8)
	v := e.verification(name)
	if v.Round != 2 || len(e.verifications(name)) != 2 {
		t.Fatalf("round %d, %d verifications: an escalation started past maxVerifications", v.Round,
			len(e.verifications(name)))
	}
	if why := e.prVerifyPass(name).gaveUp(e.get(name).Status.Acceptance, v); why == "" {
		t.Error("the hand-off waits for an escalation maxVerifications forbids")
	}
}

// TestRoundWithNoEvaluatorReportIsNoModelRound: a full round judged
// with no evaluator report (its preview failed before the evaluator ran)
// had no model look at anything, so it is no model round: a later round at
// its heads is full, never a replay trusting a judgement no model made.
func TestRoundWithNoEvaluatorReportIsNoModelRound(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.playTools(pv.Name, v1alpha1.VerificationEnvFailed, "", false)
	got := e.pvNamed(pv.Name)
	got.Status.Reason = v1alpha1.VerificationReasonComponentCrashed
	got.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent, Ready: true,
		CrashedDuring: true, Restarts: 1}}
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.settleReviews(name)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || len(v.Outcomes) == 0 {
		t.Fatalf("verification = %s %+v, want round 1 judged Unconfirmed", v.State, v.Outcomes)
	}
	if n := e.stageCount(name, v1alpha1.IntentStageEvaluate); n != 0 {
		t.Fatalf("evaluate runs = %d, want none: the preview failed first", n)
	}
	if v.ModelRound != nil {
		t.Errorf("model round %+v recorded for a round no evaluator reported on", v.ModelRound)
	}
}

// TestReplayInAFailingEnvironmentEscalates: a replay round whose every
// outcome passed, but whose environment failed while it ran, is unsure
// too: it asks for its head set's one full round, as any unsure replay.
func TestReplayInAFailingEnvironmentEscalates(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify")
	pv := e.roundOf(name, 2)
	e.playTools(pv.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 1)
	e.replayedBoth(name, pv.Name, v1alpha1.CheckRunPass)
	got := e.pvNamed(pv.Name)
	got.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent, Ready: true,
		CrashedDuring: true, Restarts: 1}}
	if err := e.c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	e.reviewUntil(name, func() bool { return e.verification(name).State != v1alpha1.VerificationEvaluating })
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || v.OutcomesEnvironment == string(httpstep.EnvOK) {
		t.Fatalf("replay round = %s in %q, want Unconfirmed in a failing environment", v.State,
			v.OutcomesEnvironment)
	}
	if esc := v.Escalation; esc == nil || esc.Round != 2 {
		t.Errorf("escalation %+v, want round 2's: a full round may settle what the environment left open", esc)
	}
}

// TestUnsureReplayEscalatesOnce: when the escalated full round is unsure
// too, no further round starts by itself; a later replay at the same
// heads, unsure again, escalates no more (the head set's one escalation is
// spent), and the hand-off no longer waits.
func TestUnsureReplayEscalatesOnce(t *testing.T) {
	e, name := escalatedRound(t, evaluationReport)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("escalated round = %s, want Unconfirmed", v.State)
	}
	e.review(name, 8)
	if n := len(e.verifications(name)); e.verification(name).Round != 3 || n > 3 {
		t.Fatalf("round %d, %d verifications: an unsure full round escalates nothing", e.verification(name).Round, n)
	}
	e.request(name, "issue", "/maul verify")
	e.replayRound(name, 4, v1alpha1.CheckRunFail)
	e.review(name, 8)
	v := e.verification(name)
	if v.Round != 4 || v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("after the second replay: round %d %s, want round 4 Unconfirmed and no escalation", v.Round, v.State)
	}
	if why := e.prVerifyPass(name).gaveUp(e.get(name).Status.Acceptance, v); why == "" {
		t.Error("the hand-off still waits after the head set's escalation was spent")
	}
}

// TestVerifyExploreForcesTheFullRung: /maul verify explore, on the issue
// or on a pull request, makes the next round at unchanged heads full: the
// evaluator runs although a replay could settle it. The reply says so.
func TestVerifyExploreForcesTheFullRung(t *testing.T) {
	for _, surface := range []string{"issue", "pull request"} {
		t.Run(surface, func(t *testing.T) {
			e, name := passedModelRound(t, 0.4)
			replies := e.request(name, surface, "/maul verify explore")
			if !slices.ContainsFunc(replies, func(b string) bool {
				return strings.Contains(b, "`/maul verify explore` from") && strings.Contains(b, "The evaluator looks again")
			}) {
				t.Errorf("replies %q, want the explore request recorded", replies)
			}
			pv := e.modelRound(name, 2, v1alpha1.CheckRunPass)
			if pv.Spec.Round != 2 {
				t.Fatalf("explored round %d, want 2", pv.Spec.Round)
			}
			if n := e.stageCount(name, v1alpha1.IntentStageEvaluate); n != 2 {
				t.Errorf("evaluate runs = %d, want the explored round's evaluator", n)
			}
			if v := e.verification(name); v.State != v1alpha1.VerificationPassed || v.ModelRound.Round != 2 {
				t.Errorf("explored round = %s, model round %+v; want Passed, round 2 the model round", v.State,
					v.ModelRound)
			}
		})
	}
}

// TestVerifyExploreIsApproversOnly: /maul verify explore from someone
// who is not an approver, on the issue or a pull request, records nothing:
// the next round an approver asks for at the same heads still replays.
func TestVerifyExploreIsApproversOnly(t *testing.T) {
	for _, surface := range []string{"issue", "pull request"} {
		t.Run(surface, func(t *testing.T) {
			e, name := passedModelRound(t, 0.4)
			e.clock.Advance(verifyInterval)
			if surface == "issue" {
				e.gh.comment("mallory", "/maul verify explore")
			} else {
				e.prCommand(e.prNumber(name), "mallory", "/maul verify explore")
			}
			e.settleActions(name)
			e.review(name, 2)
			if v := e.verification(name); v.Requested != nil || v.Round != 1 {
				t.Fatalf("a stranger's explore recorded %+v at round %d", v.Requested, v.Round)
			}
			e.request(name, surface, "/maul verify")
			e.replayRound(name, 2, v1alpha1.CheckRunPass)
			if n := e.stageCount(name, v1alpha1.IntentStageEvaluate); n != 1 {
				t.Errorf("evaluate runs = %d, want round 1's alone", n)
			}
		})
	}
}

// TestObjectingModelRoundIsNotReplayed: a full round whose evaluator
// reported an anomaly (or saw an outcome fail) is not clear, so the next
// round at its heads is full, never a replay that would pass what that
// evaluator raised; so is the next round once the heads move.
func TestObjectingModelRoundIsNotReplayed(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.evaluateReporting(anomalyReport)
	e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
	e.acceptManual(name)
	e.modelRound(name, 1, v1alpha1.CheckRunPass)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed || v.ModelRound == nil || v.ModelRound.Clear {
		t.Fatalf("round 1 = %s, model round %+v; want Unconfirmed on the anomaly, not clear", v.State, v.ModelRound)
	}
	e.evaluateReporting(evaluationReport)
	e.request(name, "issue", "/maul verify")
	pv := e.modelRound(name, 2, v1alpha1.CheckRunPass)
	if pv.Spec.Round != 2 {
		t.Fatalf("round %d, want 2", pv.Spec.Round)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationPassed || !v.ModelRound.Clear {
		t.Errorf("round 2 = %s, model round %+v; want Passed, clear", v.State, v.ModelRound)
	}
}

// TestRetryKeepsItsRung: an attempt maul's own machinery failed is retried
// on its round's rung: a replay round's retry replays, an explored round's
// retry is full though a replay could now settle it.
func TestRetryKeepsItsRung(t *testing.T) {
	e, name := passedModelRound(t, 0.4)
	e.request(name, "issue", "/maul verify")
	pv := e.roundOf(name, 2)
	if pv.Spec.Rung != v1alpha1.RungReplay {
		t.Fatalf("round 2 is %s, want replay", pv.Spec.Rung)
	}
	e.playTools(pv.Name, v1alpha1.VerificationInfra, "", false)
	retry := e.nextAttempt(name)
	if retry.Spec.Round != 2 || retry.Spec.Attempt != 2 || retry.Spec.Rung != v1alpha1.RungReplay {
		t.Fatalf("retry = round %d attempt %d %s, want round 2's second attempt, replay", retry.Spec.Round,
			retry.Spec.Attempt, retry.Spec.Rung)
	}
	e.playTools(retry.Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 2)
	e.replayedBoth(name, retry.Name, v1alpha1.CheckRunPass)
	e.reviewUntil(name, func() bool { return e.verification(name).State != v1alpha1.VerificationEvaluating })

	e.request(name, "issue", "/maul verify explore")
	pv = e.roundOf(name, 3)
	if pv.Spec.Rung != v1alpha1.RungFull {
		t.Fatalf("round 3 is %s, want full", pv.Spec.Rung)
	}
	e.playTools(pv.Name, v1alpha1.VerificationInfra, "", false)
	retry = e.nextAttempt(name)
	if retry.Spec.Round != 3 || retry.Spec.Attempt != 2 || retry.Spec.Rung != v1alpha1.RungFull {
		t.Fatalf("retry = round %d attempt %d %s, want round 3's second attempt, full", retry.Spec.Round,
			retry.Spec.Attempt, retry.Spec.Rung)
	}
}

// nextAttempt runs review passes until intent name's round has another
// attempt, and returns it.
func (e *env) nextAttempt(name string) v1alpha1.PreviewVerification {
	e.t.Helper()
	v := e.verification(name)
	var out v1alpha1.PreviewVerification
	e.reviewUntil(name, func() bool {
		for _, pv := range e.verifications(name) {
			if pv.Spec.Round == v.Round && pv.Spec.Attempt == v.Attempt+1 {
				out = pv
				return true
			}
		}
		return false
	})
	return out
}

// TestReplayAtTheEvaluationCeiling: a replay round spends nothing, so the
// evaluation ceiling does not hold it back; an unsure one there escalates
// to nothing (a full round would spend), and the hand-off says the budget
// is why.
func TestReplayAtTheEvaluationCeiling(t *testing.T) {
	e, name := passedModelRound(t, 6)
	if !e.prVerifyPass(name).atEvaluationCeiling() {
		t.Fatal("round 1's evaluator did not spend the ceiling")
	}
	e.request(name, "issue", "/maul verify")
	e.replayRound(name, 2, v1alpha1.CheckRunPass)
	if v := e.verification(name); v.State != v1alpha1.VerificationPassed {
		t.Fatalf("replay round at the ceiling = %s, want Passed", v.State)
	}

	e.request(name, "issue", "/maul verify")
	e.replayRound(name, 3, v1alpha1.CheckRunFail)
	e.review(name, 6)
	v := e.verification(name)
	if v.Round != 3 || v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("round %d %s, want round 3 Unconfirmed, no escalation past the ceiling", v.Round, v.State)
	}
	p := e.prVerifyPass(name)
	if p.escalationDue(v) {
		t.Error("an escalation is due past the ceiling")
	}
	if why := p.gaveUp(e.get(name).Status.Acceptance, v); why == "" {
		t.Error("the hand-off waits for an escalation that cannot run")
	}
}

// TestReplayRoundRunsNoEvaluator: at the heads of a clear model round,
// with every outcome's check in the store and reviewed, the round replays
// them (rung replay, the store's checks inline under its digest) and is
// judged once the verification settles, with no evaluate run at all. A
// store that is not what its ref records starts no round.
func TestReplayRoundRunsNoEvaluator(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 2)
	in := e.get(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.ModelRound = &v1alpha1.VerificationModelRound{Round: 1, HeadSetDigest: v.HeadSetDigest,
			OutcomesDigest: in.Status.Acceptance.OutcomesDigest, Clear: true}
		v.Ref, v.Attempt = nil, 0
	})
	for _, pv := range e.verifications(name) {
		if err := e.c.Delete(context.Background(), &pv); err != nil {
			t.Fatal(err)
		}
	}
	e.review(name, 3)
	pvs := e.verifications(name)
	sum := in.Status.Acceptance.Checks.Digest
	if len(pvs) != 1 || pvs[0].Spec.Rung != v1alpha1.RungReplay || pvs[0].Spec.ChecksDigest != sum ||
		len(pvs[0].Spec.Checks) != 2 {
		t.Fatalf("verifications = %+v, want one replay of the stored checks", pvs)
	}
	e.playTools(pvs[0].Name, v1alpha1.VerificationRunning, toolPodIP, true)
	e.review(name, 2)
	e.replayedBoth(name, pvs[0].Name, v1alpha1.CheckRunPass)
	e.reviewUntil(name, func() bool { return e.verification(name).State != v1alpha1.VerificationEvaluating })
	if runs := e.runsOf(name, v1alpha1.IntentStageEvaluate); len(runs) != 0 {
		t.Fatalf("evaluate runs = %d on a replay round, want none", len(runs))
	}
	if v := e.verification(name); v.State == v1alpha1.VerificationEvaluating {
		t.Fatalf("state = %s, want the replay round judged", v.State)
	}

	// A store whose bytes are not its recorded digest starts nothing.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Checks.Digest = "sha256:" + strings.Repeat("c", 64)
		in.Status.Verification.Ref, in.Status.Verification.Attempt = nil, 0
	})
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Errorf("verifications = %d, want none started over a store not on record", n)
	}
}
