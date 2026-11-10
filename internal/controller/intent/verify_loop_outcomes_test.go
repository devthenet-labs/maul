// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// The outcome twins of verify_loop_test.go (S4 PR 1): the verification
// loop's gate, bounds and supersession for an intent whose approved plan
// states outcomes, under the agent evaluator. Each test is named after the
// criteria test it twins.

// evaluatorReviewing is an env over plan (outcomesPlan when empty) with the
// evaluator on and a new intent in review, no Preview served yet. It
// returns the env and the intent's name.
func evaluatorReviewing(t *testing.T, plan string, mutate ...func(*v1alpha1.Project)) (*env, string) {
	t.Helper()
	if plan == "" {
		plan = outcomesPlan
	}
	e := newVerifyEnv(t, plan, mutate...)
	e.evaluatorOn(0.4)
	return e, e.reviewing()
}

// TestOutcomeVerificationOffWritesNothing twins
// TestVerificationOffWritesNothing: an approved outcomes plan under a
// controller with verification, and so the evaluator, off.
func TestOutcomeVerificationOffWritesNothing(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan)
	e.intent.Settings.Verification = false
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 4)
	in := e.get(name)
	if in.Status.Acceptance != nil || in.Status.Verification != nil || len(e.verifications(name)) != 0 {
		t.Fatalf("acceptance %+v verification %+v verifications %d", in.Status.Acceptance,
			in.Status.Verification, len(e.verifications(name)))
	}
	if ss := e.acceptanceStatuses(in.Status.PullRequests[0].HeadSHA); len(ss) != 0 {
		t.Fatalf("statuses posted with verification off: %+v", ss)
	}
}

// TestOutcomeVerificationOffOnAProjectThatDoesNotVerify twins
// TestVerificationOffOnAProjectThatDoesNotVerify.
func TestOutcomeVerificationOffOnAProjectThatDoesNotVerify(t *testing.T) {
	e, name := evaluatorReviewing(t, "", func(p *v1alpha1.Project) { p.Spec.Verification = nil })
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification = &v1alpha1.IntentVerification{Requested: &v1alpha1.VerifyRequest{By: approver,
			ByID: 7, At: metav1.NewTime(e.clock.Now()), CommentID: 99}}
	})
	e.readyPreview(name)
	e.review(name, 3)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationOff || v.Requested != nil || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v, verifications %d", v, len(e.verifications(name)))
	}
}

// TestOutcomeVerificationNotDeclared twins TestVerificationNotDeclared: a
// plan stating no outcomes under the evaluator is NotDeclared, nothing
// runs, and maul/acceptance is never green.
func TestOutcomeVerificationNotDeclared(t *testing.T) {
	e, name := evaluatorReviewing(t, validPlan)
	e.readyPreview(name)
	e.review(name, 3)
	in := e.get(name)
	if in.Status.Acceptance == nil || len(in.Status.Acceptance.Outcomes) != 0 ||
		in.Status.Verification.State != v1alpha1.VerificationNotDeclared || len(e.verifications(name)) != 0 {
		t.Fatalf("acceptance %+v verification %+v", in.Status.Acceptance, in.Status.Verification)
	}
	if st := e.latestAcceptance(in.Status.PullRequests[0].HeadSHA); st.State == statusSuccess {
		t.Fatalf("status = %+v, want never success with nothing declared", st)
	}
}

// TestOutcomeVerificationWithoutPreviews twins
// TestVerificationWithoutPreviews.
func TestOutcomeVerificationWithoutPreviews(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan)
	e.evaluatorOn(0.4)
	e.reconcileProject()
	if c := meta.FindStatusCondition(e.getProject().Status.Conditions, ConditionVerificationOff); c != nil {
		t.Fatalf("%s with previews on: %+v", ConditionVerificationOff, c)
	}
	e.intent.Settings.Previews = false
	e.project.Settings.Verification, e.project.Settings.Previews = true, false
	e.reconcileProject()
	conds := e.getProject().Status.Conditions
	c := meta.FindStatusCondition(conds, ConditionVerificationOff)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonPreviewsDisabled ||
		!meta.IsStatusConditionTrue(conds, v1alpha1.ConditionReady) {
		t.Fatalf("project conditions = %+v", conds)
	}
	name := e.reviewing()
	e.review(name, 6)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationOff || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v, verifications %d", v, len(e.verifications(name)))
	}
	if ss := e.acceptanceStatuses(e.get(name).Status.PullRequests[0].HeadSHA); len(ss) != 0 {
		t.Fatalf("statuses posted with verification off: %+v", ss)
	}
	if body := e.statusBody(); !strings.Contains(body, "**Verification:** off") {
		t.Fatalf("status comment does not say verification is off:\n%s", body)
	}
}

// TestOutcomeVerificationWaitsForThePreview twins
// TestVerificationWaitsForThePreview.
func TestOutcomeVerificationWaitsForThePreview(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.previewOf(name)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForPreview || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v", v)
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageEvaluate)); n != 0 {
		t.Fatalf("evaluate runs = %d before the preview is live", n)
	}
}

// TestOutcomeVerificationGivesUpWaitingForThePreview twins
// TestVerificationGivesUpWaitingForThePreview.
func TestOutcomeVerificationGivesUpWaitingForThePreview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase v1alpha1.PreviewPhase
	}{{"failed", v1alpha1.PreviewFailed}, {"expired", v1alpha1.PreviewExpired}, {"never served", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := evaluatorReviewing(t, "")
			if tc.phase != "" {
				e.servePreview(name, tc.phase, "")
			} else {
				e.previewOf(name)
			}
			e.review(name, 6)
			v := e.verification(name)
			if v.State != v1alpha1.VerificationWaitingForPreview || v.PreviewWaitSince == nil {
				t.Fatalf("verification = %+v, want waiting with its start recorded", v)
			}
			head := e.get(name).Status.PullRequests[0].HeadSHA
			if st := e.latestAcceptance(head); st.State != statusPending {
				t.Fatalf("status while waiting = %+v", st)
			}
			e.clock.Advance(previewWaitMax - 10*time.Minute)
			e.review(name, 2)
			if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForPreview {
				t.Fatalf("gave up before the bound: %+v", v)
			}
			e.review(name, 6)
			v = e.verification(name)
			if v.State != v1alpha1.VerificationUnavailable ||
				v.UnavailableReason != v1alpha1.VerificationUnavailablePreviewNotLive || len(e.verifications(name)) != 0 {
				t.Fatalf("verification after the bound = %+v, verifications %d", v, len(e.verifications(name)))
			}
			// The status follows the state at the next pass.
			e.reviewUntil(name, func() bool { return e.latestAcceptance(head).State != statusPending })
			st := e.latestAcceptance(head)
			if st.State != statusError || !strings.Contains(st.Description, "preview") || len(st.Description) > 140 {
				t.Fatalf("status after the bound = %+v", st)
			}
			e.review(name, 3)
			if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable {
				t.Fatalf("did not stay unavailable: %+v", v)
			}
			e.readyPreview(name)
			e.review(name, 3)
			v = e.verification(name)
			if v.State != v1alpha1.VerificationEvaluating || v.UnavailableReason != "" || len(e.verifications(name)) != 1 {
				t.Fatalf("verification once the preview is live = %+v, verifications %d", v,
					len(e.verifications(name)))
			}
		})
	}
}

// TestOutcomeRequestAfterARoundWaitsForThePreview twins
// TestRequestAfterARoundWaitsForThePreview.
func TestOutcomeRequestAfterARoundWaitsForThePreview(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.readyPreview(name)
	e.review(name, 3)
	first := e.onlyVerification(name)
	outcomeRequest(t, &first)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.State, v.UnavailableReason = v1alpha1.VerificationUnavailable, ""
		v.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()),
			CommentID: 99}
	})
	e.servePreview(name, v1alpha1.PreviewFailed, "")
	e.review(name, 3)
	v := e.verification(name)
	if v.State != v1alpha1.VerificationWaitingForPreview || v.PreviewWaitSince == nil || v.Requested == nil {
		t.Fatalf("verification = %+v, want the request waiting for the preview, its start recorded", v)
	}
	e.clock.Advance(previewWaitMax)
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable ||
		v.UnavailableReason != v1alpha1.VerificationUnavailablePreviewNotLive || v.Requested == nil {
		t.Fatalf("verification after the bound = %+v, want Unavailable (PreviewNotLive), the request kept", v)
	}
	e.readyPreview(name)
	e.review(name, 3)
	vs := e.verifications(name)
	if v := e.verification(name); len(vs) != 2 || v.Requested != nil || v.Ref == nil || v.Ref.Name == first.Name {
		t.Fatalf("verification once the preview is live = %+v, verifications %d; want the request run", v, len(vs))
	}
}

// TestOutcomeVerificationWaitsForCI twins TestVerificationWaitsForCI.
func TestOutcomeVerificationWaitsForCI(t *testing.T) {
	e, name := evaluatorReviewing(t, "", func(p *v1alpha1.Project) { p.Spec.Checks.Fix = []string{"test"} })
	e.readyPreview(name)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.gh.mu.Lock()
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 5, Name: "test", HeadSHA: head, Status: "in_progress"}}
	e.gh.mu.Unlock()
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForCI || len(e.verifications(name)) != 0 {
		t.Fatalf("verification = %+v", v)
	}
	e.gh.mu.Lock()
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 5, Name: "test", HeadSHA: head, Status: "completed",
		Conclusion: "success"}}
	e.gh.mu.Unlock()
	e.review(name, 2)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("verifications once green = %d, want 1", n)
	}
}

// evaluationWallClock is an outcome round's wall clock under evaluatorOn:
// the evaluate run's timeout plus EvaluationToolsMargin, within a
// verification's hour (createEvaluation), never the Project's verification
// timeout.
func evaluationWallClock() time.Duration {
	return max(min(DefaultEvaluateTimeout+EvaluationToolsMargin, maxVerificationTimeout), minVerificationTimeout)
}

// TestOutcomeVerificationCreatesTheRequest twins
// TestVerificationCreatesTheRequest: one outcome round, owned by the
// Intent, pinned to it and its Preview by UID, carrying the heads, the
// approved outcomes and their digest, its wall clock the evaluate run's
// (the Project's verification timeout bounds slice 1's tester only).
func TestOutcomeVerificationCreatesTheRequest(t *testing.T) {
	e, name := evaluatorReviewing(t, "", func(p *v1alpha1.Project) {
		p.Spec.Verification.Timeout = &metav1.Duration{Duration: 45 * time.Minute}
	})
	preview := e.readyPreview(name)
	e.review(name, 3)
	in := e.get(name)
	pv := e.onlyVerification(name)
	if pv.Name != v1alpha1.PreviewVerificationName(name, 1, 1) || !controlledBy(pv.OwnerReferences, in.UID) {
		t.Fatalf("verification %s owners %+v", pv.Name, pv.OwnerReferences)
	}
	s := pv.Spec
	head := in.Status.PullRequests[0].HeadSHA
	wantHeads := []v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: head}}
	if s.IntentRef.UID != in.UID || s.PreviewRef.UID != preview.UID || s.PreviewRef.Name != preview.Name ||
		s.PreviewGeneration != preview.Generation || !slices.Equal(s.Heads, wantHeads) ||
		s.HeadSetDigest != headSetDigest(wantHeads) || s.Round != 1 || s.Attempt != 1 ||
		!regexp.MustCompile(`^[0-9a-f]{6}$`).MatchString(s.Run) || s.Timeout.Duration != evaluationWallClock() {
		t.Fatalf("spec = %+v", s)
	}
	outcomeRequest(t, &pv)
	if s.OutcomesDigest != in.Status.Acceptance.OutcomesDigest {
		t.Fatalf("outcomes digest %s, want the approved %s", s.OutcomesDigest, in.Status.Acceptance.OutcomesDigest)
	}
	v := in.Status.Verification
	if v.State != v1alpha1.VerificationEvaluating || v.Round != 1 || v.Attempt != 1 || v.Ref == nil ||
		v.Ref.UID != pv.UID || v.HeadSetDigest != pv.Spec.HeadSetDigest || v.Automatic != 1 || v.LastAutomaticAt == nil {
		t.Fatalf("verification = %+v", v)
	}
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("an in-flight verification was created again: %d", n)
	}
}

// TestOutcomeVerificationInfraRetriesThenUnavailable twins
// TestVerificationInfraRetriesThenUnavailable: the tool pod's Infra is
// retried as new attempts of the same round, uncounted, and after the last
// the verification is unavailable, never a verdict.
func TestOutcomeVerificationInfraRetriesThenUnavailable(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.readyPreview(name)
	e.review(name, 3)
	for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
		pvName := v1alpha1.PreviewVerificationName(name, 1, attempt)
		e.playTools(pvName, v1alpha1.VerificationInfra, "", false)
		e.review(name, 3)
	}
	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnavailable || v.Round != 1 || v.Attempt != 3 || v.Automatic != 1 {
		t.Fatalf("verification = %+v", v)
	}
	if n := len(e.verifications(name)); n != 3 {
		t.Fatalf("verifications = %d, want 3 attempts", n)
	}
	if st := e.latestAcceptance(e.get(name).Status.PullRequests[0].HeadSHA); st.State == statusSuccess {
		t.Fatalf("status = %+v after maul's own failures", st)
	}
}

// TestOutcomeVerificationSupersededByAMovedHead twins
// TestVerificationSupersededByAMovedHead.
func TestOutcomeVerificationSupersededByAMovedHead(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.readyPreview(name)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	outcomeRequest(t, &pv)
	n := e.get(name).Status.PullRequests[0].Number
	moved := strings.Repeat("f", 40)
	e.pushHead(n, moved)
	e.review(name, 3)
	var gone v1alpha1.PreviewVerification
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(&pv), &gone); !kerrors.IsNotFound(err) {
		t.Fatalf("the superseded verification is still there: %v", err)
	}
	v := e.verification(name)
	want := headSetDigest([]v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: moved}})
	if v.HeadSetDigest != want || v.Ref != nil || v.AutoFix.State != v1alpha1.AutoFixHalted ||
		v.AutoFix.Reason != v1alpha1.AutoFixHumanPush {
		t.Fatalf("verification = %+v", v)
	}
}

// TestOutcomeVerificationAutomaticBounds twins
// TestVerificationAutomaticBounds.
func TestOutcomeVerificationAutomaticBounds(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.readyPreview(name)
	e.review(name, 2)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		at := metav1.NewTime(e.clock.Now())
		in.Status.Verification.Automatic = 20
		in.Status.Verification.LastAutomaticAt = &at
	})
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("maxVerifications spent, yet %d verifications", n)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		at := metav1.NewTime(e.clock.Now())
		in.Status.Verification.Automatic = 1
		in.Status.Verification.LastAutomaticAt = &at
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted,
			Reason: v1alpha1.AutoFixHumanPush}
	})
	e.review(name, 3) // three minutes
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("halted and within five minutes, yet %d verifications", n)
	}
	e.review(name, 3)
	if n := len(e.verifications(name)); n != 1 {
		t.Fatalf("halted past five minutes: %d verifications, want 1", n)
	}
}

// TestOutcomeVerificationRequestRunsAgain twins
// TestVerificationRequestRunsAgain: an approver's /maul verify runs a new
// round at a head set already judged, uncounted, and consumes the request;
// a second request within five minutes of the first waits.
func TestOutcomeVerificationRequestRunsAgain(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunPass, v1alpha1.CheckRunPass)...)
	if v := e.verification(name); v.State != v1alpha1.VerificationPassed && v.State != v1alpha1.VerificationUnconfirmed {
		t.Fatalf("round 1 = %s %+v, want judged", v.State, v.Outcomes)
	}
	e.review(name, 2)
	request := func(id int64) {
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
				At: metav1.NewTime(e.clock.Now()), CommentID: id}
		})
	}
	request(101)
	e.review(name, 1)
	v := e.verification(name)
	if v.Requested != nil || v.Round != 2 || v.Automatic != 1 || len(e.verifications(name)) != 2 {
		t.Fatalf("after a request: %+v, %d verifications", v, len(e.verifications(name)))
	}
	created := e.clock.Now()
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunPass, v1alpha1.CheckRunPass)...)
	request(102)
	e.review(name, 1)
	if since := e.clock.Now().Sub(created); since >= verifyInterval {
		t.Fatalf("the second request was judged %s after round 2: the spacing is not under test", since)
	}
	if v := e.verification(name); v.Requested == nil || len(e.verifications(name)) != 2 {
		t.Fatalf("a request within five minutes ran: %+v", v)
	}
	e.review(name, 3)
	if v := e.verification(name); v.Requested != nil || v.Round != 3 {
		t.Fatalf("the request did not run once five minutes passed: %+v", v)
	}
}

// TestOutcomeVerificationStopsWhenTheApprovedOutcomesCannotBeRead twins
// TestVerificationStopsWhenTheApprovedCriteriaCannotBeRead: when the
// approved plan revision cannot be read (its plan no longer matches the
// approval's digest) and the outcomes on record are another revision's,
// nothing is evaluated against outcomes the approver no longer approved,
// and maul/acceptance is never a verdict from the old ones.
func TestOutcomeVerificationStopsWhenTheApprovedOutcomesCannotBeRead(t *testing.T) {
	e, name := evaluatorReviewing(t, "")
	e.review(name, 2)
	in := e.get(name)
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: testNS, Name: planConfigMapName(name, in.Status.Approval.PlanRevision)}
	if err := e.c.Get(context.Background(), key, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[keyPlan] += "\n"
	if err := e.c.Update(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.PlanRevision = in.Status.Approval.PlanRevision + 1
	})
	e.readyPreview(name)
	e.review(name, 6)
	if n := len(e.verifications(name)); n != 0 {
		t.Fatalf("evaluated %d times against outcomes the approver no longer approved", n)
	}
	in = e.get(name)
	if v := in.Status.Verification; v == nil || v.State != v1alpha1.VerificationUnavailable {
		t.Fatalf("verification = %+v, want Unavailable", v)
	}
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionVerifyFailing)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", c)
	}
	s := e.latestAcceptance(in.Status.PullRequests[0].HeadSHA)
	if s.State != "error" || !strings.Contains(s.Description, "cannot be read") {
		t.Fatalf("status = %+v", s)
	}
}
