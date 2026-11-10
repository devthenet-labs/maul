// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/templates"
)

// notRunReplies are maul's replies saying an approver's /maul verify did
// not run.
func (e *env) notRunReplies() []string {
	e.t.Helper()
	cs := e.gh.withMarker(notRunKeyPrefix)
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

// passAgentsRound records the open agents round of name as judged Passed
// at its heads, every evaluable outcome confirmed and the hand-off released
// ready, with its verification settled: what a round that passed leaves.
func (e *env) passAgentsRound(name string, pv v1alpha1.PreviewVerification) {
	e.t.Helper()
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.State = v1alpha1.VerificationPassed
		v.Outcomes = nil
		for _, o := range in.Status.Acceptance.Outcomes {
			if !o.Manual {
				v.Outcomes = append(v.Outcomes, v1alpha1.OutcomeResult{ID: o.ID, Result: v1alpha1.OutcomePassed})
			}
		}
		v.Passed = int32(len(v.Outcomes))
		v.OutcomesRound, v.OutcomesHeadSetDigest = v.Round, v.HeadSetDigest
		at := metav1.NewTime(e.clock.Now())
		v.Handoff = &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated, Released: true, ReleasedAt: &at,
			Reason: v1alpha1.HandoffReasonReady}
	})
	e.playTools(pv.Name, v1alpha1.VerificationObserved, "", false)
}

// keepRunnerImages saves every Repository's accepted runner image and
// returns what puts them back.
func keepRunnerImages(t *testing.T, e *env) func() {
	t.Helper()
	var list v1alpha1.RepositoryList
	if err := e.c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	saved := map[string]*v1alpha1.RunnerImage{}
	for _, r := range list.Items {
		if r.Status.RunnerImage != nil {
			saved[r.Name] = r.Status.RunnerImage.DeepCopy()
		}
	}
	return func() {
		t.Helper()
		for name, img := range saved {
			var r v1alpha1.Repository
			if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, &r); err != nil {
				t.Fatal(err)
			}
			r.Status.RunnerImage = img
			if err := e.c.Status().Update(context.Background(), &r); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestAgentsRequestThatCannotRunKeepsTheJudgement (S2-B8): an approver's
// /maul verify at heads a round already judged, when the round cannot run
// (no accepted image, no fetchable tree), leaves that judgement standing,
// so the state and every surface agree (Passed, Ready), and answers the
// request that it did not run and why; once what was missing is back, the
// request sent again runs. Before: the state became Unconfirmed while every
// surface still read "Ready ... passed", the approver had only the
// acknowledgement, and nothing ran until the intent was cancelled.
func TestAgentsRequestThatCannotRunKeepsTheJudgement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		remove  func(t *testing.T, e *env) func()
		words   string
		restore bool
	}{
		{"no accepted image", func(t *testing.T, e *env) func() {
			restore := keepRunnerImages(t, e)
			dropRunnerImages(t, e)
			return restore
		}, "the repository has no accepted runner image to build the pull request in", true},
		{"no fetchable tree", func(_ *testing.T, e *env) func() {
			e.gh.failNext("MergeBase", ghError(404, "No common ancestor"))
			return func() {}
		}, "it could not fetch the source of the pull request or of its base", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := newAgentsEnv(t, repoImage)
			e.reviewReady(name, 4)
			pv := e.onlyVerification(name)
			e.passAgentsRound(name, pv)
			e.review(name, 2)
			restore := tc.remove(t, e)

			e.clock.Advance(time.Minute)
			id := e.gh.comment(approver, "/maul verify")
			e.settleActions(name)
			e.review(name, 2)

			v := e.verification(name)
			if v.State != v1alpha1.VerificationPassed || v.UnavailableReason != "" || v.Requested != nil ||
				v.Round != 1 || len(v.JudgedOutcomes()) == 0 {
				t.Fatalf("verification = %+v, want round 1's Passed standing and the request consumed", v)
			}
			if vs := e.verifications(name); len(vs) != 1 || vs[0].Name != pv.Name {
				t.Fatalf("verifications = %+v, want only round 1's", vs)
			}
			replies := e.notRunReplies()
			// Found after the acknowledgement: the answer replaces it.
			said := "was recorded, but did not run after all: " + tc.words
			if tc.restore {
				// Known when acknowledged (S2-B10): the one reply is the
				// answer to the comment itself.
				replies, said = []string{e.onlyReply(id)}, "did not run: "+tc.words
			}
			if len(replies) != 1 || !strings.Contains(replies[0], said) ||
				!strings.Contains(replies[0], "Evaluation 1's result at these commits stands") {
				t.Fatalf("not-run replies = %q", replies)
			}
			if body := e.statusBody(); !strings.Contains(body, "Ready") || strings.Contains(body, "couldn't confirm") {
				t.Fatalf("status comment disagrees with Passed:\n%s", body)
			}
			if !tc.restore {
				return
			}

			restore()
			e.clock.Advance(time.Minute)
			e.gh.comment(approver, "/maul verify")
			e.settleActions(name)
			e.reviewReady(name, 4)
			if vs := e.verifications(name); len(vs) != 2 {
				t.Fatalf("verifications = %d, want the request's round 2 once the image is back", len(vs))
			}
			if v := e.verification(name); v.Round != 2 || v.State != v1alpha1.VerificationEvaluating {
				t.Fatalf("verification = %+v, want round 2 evaluating", v)
			}
		})
	}
}

// TestAgentsUnrunnableStateNeverReadsPassed (S2-B8): a verification whose
// state records that its round could not run (Unconfirmed,
// SandboxImageUnavailable, as 0.13.17 left one over round 5's confirmed
// outcomes at the same heads) is never shown as passed or ready: the
// status comment, the pull request's comment, the commit status and the
// hand-off say maul couldn't confirm it, and why. An approver's /maul
// verify then runs once the image is there.
func TestAgentsUnrunnableStateNeverReadsPassed(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.passAgentsRound(name, pv)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.State, v.UnavailableReason = v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationUnavailableSandboxImage
	})
	e.review(name, 3)

	body := e.statusBody()
	if strings.Contains(body, "Ready") || strings.Contains(body, " passed") ||
		!strings.Contains(body, "no accepted runner image") {
		t.Fatalf("status comment:\n%s", body)
	}
	pr := e.get(name).Status.PullRequests[0]
	var sticky string
	for _, c := range e.gh.prComments[pr.Number] {
		if strings.Contains(c.Body, templates.NoticeMarker(testNS, name, templates.PreviewKey)) {
			sticky = c.Body
		}
	}
	if strings.Contains(sticky, "Ready") || strings.Contains(sticky, " passed") ||
		!strings.Contains(sticky, "no accepted runner image") {
		t.Fatalf("pull request comment:\n%s", sticky)
	}
	p := &pass{r: e.intent, set: e.intent.Settings.withDefaults(), in: e.get(name), proj: e.getProject(),
		now: e.clock.Now()}
	if err := p.loadRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, ok := p.evaluationStatus(); !ok || st.state == statusSuccess {
		t.Fatalf("commit status = %+v, want no success", st)
	}
	if h := p.handoffView(); h == nil || h.State != templates.HandoffUnconfirmed ||
		h.Why != templates.HandoffWhySandboxImage {
		t.Fatalf("hand-off view = %+v, want couldn't confirm: no accepted image", h)
	}

	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.reviewReady(name, 4)
	if v := e.verification(name); v.Round != 2 || v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("verification = %+v, want the request's round 2", v)
	}
}

// TestVerifyWhileOffIsAnsweredNotRun (S2-B4 review, S2-B8): an approver's
// /maul verify on an intent whose verification is off (the Project's, or
// the agents tool host switched off) is answered that it did not run and
// why, never acknowledged as recorded, and records no request.
func TestVerifyWhileOffIsAnsweredNotRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*env, string)
		words string
	}{
		{"agents host off", func(t *testing.T) (*env, string) {
			e, name := newAgentsEnv(t, repoImage)
			e.agentsOn(false)
			e.review(name, 2)
			return e, name
		}, "the tool pod that checks a project with no preview is not enabled"},
		{"project does not verify", func(t *testing.T) (*env, string) {
			e := newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) { p.Spec.Verification = nil })
			name := e.reviewing()
			e.readyPreview(name)
			e.review(name, 2)
			return e, name
		}, "verification is not enabled for this project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := tc.setup(t)
			if v := e.verification(name); v.State != v1alpha1.VerificationOff {
				t.Fatalf("verification = %s, want Off", v.State)
			}
			id := e.gh.comment(approver, "/maul verify")
			e.settleActions(name)
			reply := e.onlyReply(id)
			if !strings.Contains(reply, "did not run: "+tc.words) || strings.Contains(reply, "is recorded") {
				t.Fatalf("reply = %s", reply)
			}
			if v := e.verification(name); v.Requested != nil {
				t.Fatalf("verification = %+v, want no request recorded", v)
			}
		})
	}
}

// TestPendingRequestDroppedByTheRollbackIsAnswered (S2-B4 review): a
// request waiting on an open agents round when the host is switched off is
// consumed as the round settles, and answered that it did not run, never
// dropped in silence.
func TestPendingRequestDroppedByTheRollbackIsAnswered(t *testing.T) {
	e, name, _ := evaluatingAgentsRound(t)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7,
			At: metav1.NewTime(e.clock.Now()), CommentID: 99}
	})
	e.agentsOn(false)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable || v.Requested != nil {
		t.Fatalf("verification = %+v, want Unavailable with the request consumed", v)
	}
	replies := e.notRunReplies()
	if len(replies) != 1 || !strings.Contains(replies[0], "was recorded, but did not run after all: the tool pod") ||
		!strings.Contains(replies[0], "This replaces maul's earlier reply.") {
		t.Fatalf("not-run replies = %q", replies)
	}
}

// TestOutcomesReadyNeverOverAnUnrunnableRound: outcomesReady, which every
// surface's "ready" and "passed" reads, is false while the verification
// records that its round could not run, whatever outcomes stand.
func TestOutcomesReadyNeverOverAnUnrunnableRound(t *testing.T) {
	acc := &v1alpha1.IntentAcceptance{}
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, reason := range []v1alpha1.VerificationUnavailableReason{"", v1alpha1.VerificationUnavailableSandboxImage,
		v1alpha1.VerificationUnavailableTree, v1alpha1.VerificationUnavailableToolsBuildFailed} {
		v := &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed, UnavailableReason: reason,
			Round: 5, HeadSetDigest: digest, OutcomesRound: 5, OutcomesHeadSetDigest: digest,
			OutcomesEnvironment: string(acceptance.EnvOK),
			Outcomes:            []v1alpha1.OutcomeResult{{ID: oVersion, Result: v1alpha1.OutcomePassed}}}
		if got, want := outcomesReady(acc, v), reason == ""; got != want {
			t.Errorf("reason %q: outcomesReady = %v, want %v", reason, got, want)
		}
	}
}

// TestOutcomesReadyNeedsACleanEnvironment (review H2): a judgement whose
// every outcome passed is ready only in the environment Decide hands off
// ready in; one that failed, maul's machinery's trouble, or a judgement
// recorded with no environment at all, is never ready.
func TestOutcomesReadyNeedsACleanEnvironment(t *testing.T) {
	acc := &v1alpha1.IntentAcceptance{}
	digest := "sha256:" + strings.Repeat("a", 64)
	for env, want := range map[acceptance.EnvClass]bool{acceptance.EnvOK: true, acceptance.EnvAppFailure: false,
		acceptance.EnvInfra: false, "": false} {
		v := &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed, Round: 5, HeadSetDigest: digest,
			OutcomesRound: 5, OutcomesHeadSetDigest: digest, OutcomesEnvironment: string(env),
			Outcomes: []v1alpha1.OutcomeResult{{ID: oVersion, Result: v1alpha1.OutcomePassed}}}
		if got := outcomesReady(acc, v); got != want {
			t.Errorf("environment %q: outcomesReady = %v, want %v", env, got, want)
		}
	}
}

// TestAgentsUnrunnableStateIsNoStandingJudgement (S2-B8 review): a state
// that records its round could not run (as 0.13.17 left one over round 1's
// confirmed outcomes) is no judgement that stands: a /maul verify that
// still cannot run is answered without "result stands", which every other
// surface contradicts ("couldn't confirm"), and the state keeps its reason.
func TestAgentsUnrunnableStateIsNoStandingJudgement(t *testing.T) {
	e, name := newAgentsEnv(t, repoImage)
	e.reviewReady(name, 4)
	pv := e.onlyVerification(name)
	e.passAgentsRound(name, pv)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		v := in.Status.Verification
		v.State, v.UnavailableReason = v1alpha1.VerificationUnconfirmed, v1alpha1.VerificationUnavailableSandboxImage
	})
	e.review(name, 3)
	dropRunnerImages(t, e)

	e.clock.Advance(time.Minute)
	id := e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.review(name, 2)

	v := e.verification(name)
	if v.State != v1alpha1.VerificationUnconfirmed ||
		v.UnavailableReason != v1alpha1.VerificationUnavailableSandboxImage || v.Requested != nil {
		t.Fatalf("verification = %+v, want Unconfirmed for no image, the request consumed", v)
	}
	replies := []string{e.onlyReply(id)}
	if !strings.Contains(replies[0], "did not run: the repository has no accepted runner image") ||
		strings.Contains(replies[0], "stands") {
		t.Fatalf("not-run replies = %q, want the reason and no standing result", replies)
	}
}
