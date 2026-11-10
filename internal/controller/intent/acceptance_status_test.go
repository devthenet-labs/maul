// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// acceptanceStatuses are the maul/acceptance statuses on sha, newest first.
func (e *env) acceptanceStatuses(sha string) []ghclient.CommitStatus {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []ghclient.CommitStatus
	for _, s := range e.gh.statuses[sha] {
		if s.Context == acceptanceContext {
			out = append(out, s)
		}
	}
	return out
}

// latestAcceptance is the newest maul/acceptance status on sha, failing
// when there is none.
func (e *env) latestAcceptance(sha string) ghclient.CommitStatus {
	e.t.Helper()
	ss := e.acceptanceStatuses(sha)
	if len(ss) == 0 {
		e.t.Fatalf("no maul/acceptance status on %s", sha)
	}
	return ss[0]
}

// postStatus plays another actor posting a status on sha.
func (e *env) postStatus(sha string, s ghclient.CommitStatus) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.nextStatusID++
	s.ID = e.gh.nextStatusID
	e.gh.statuses[sha] = append([]ghclient.CommitStatus{s}, e.gh.statuses[sha]...)
}

// evaluating is a new intent under the evaluator in review, its Preview
// live, its version and health checks stored, and its first round created
// and evaluating; it returns the env, the intent, its pull request's head
// and the two checks.
func evaluatingIntent(t *testing.T, mutate ...func(*v1alpha1.Project)) (*env, string, string, v1alpha1.SavedCheck,
	v1alpha1.SavedCheck) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4, mutate...)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	if v := e.verification(name); v.State != v1alpha1.VerificationEvaluating {
		t.Fatalf("verification = %+v, want Evaluating", v)
	}
	return e, name, e.get(name).Status.PullRequests[0].HeadSHA, version, health
}

// TestAcceptanceStatusFollowsTheVerification: maul/acceptance is pending
// while the evaluation runs and settles with it, on the pull request's
// head, recorded on its record and posted once per state: further passes
// post nothing. A pass with a manual outcome open is never green; once an
// approver accepts it, success, naming them.
func TestAcceptanceStatusFollowsTheVerification(t *testing.T) {
	e, name, head, version, health := evaluatingIntent(t)
	e.review(name, 1)
	if s := e.latestAcceptance(head); s.State != statusPending || !strings.HasPrefix(s.Description,
		"maul is checking this") {
		t.Fatalf("while evaluating: %+v", s)
	}
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunPass, v1alpha1.CheckRunPass)...)
	e.review(name, 3)
	s := e.latestAcceptance(head)
	if s.State == statusSuccess || !strings.Contains(s.Description, "2/3 outcomes confirmed") {
		t.Fatalf("once passed with the manual outcome open: %+v", s)
	}
	pr := e.get(name).Status.PullRequests[0]
	if pr.AcceptanceStatusID != s.ID || pr.AcceptanceHead != head || pr.AcceptanceDigest == "" {
		t.Fatalf("record = %+v, want status %d at %s", pr, s.ID, head)
	}
	n := len(e.acceptanceStatuses(head))
	e.review(name, 4)
	if got := len(e.acceptanceStatuses(head)); got != n {
		t.Fatalf("an unchanged status was posted again: %d statuses, had %d", got, n)
	}
	e.gh.comment(approver, "/maul accept "+oManual)
	e.settleActions(name)
	e.review(name, 2)
	if s := e.latestAcceptance(head); s.State != statusSuccess || !strings.Contains(s.Description, approver) {
		t.Fatalf("once the manual outcome is accepted: %+v", s)
	}
}

// TestAcceptanceStatusStates: what a judged result posts. A reproduced
// failure is failure, and stays failure while the automatic fix it starts
// on the next pass runs, numbered as the first of the Project's
// maxVerifyFixes; a pass with a manual outcome open is never green,
// manualGatesStatus or not.
func TestAcceptanceStatusStates(t *testing.T) {
	t.Run("failure, fix running", func(t *testing.T) {
		e, name, _ := failingEvaluation(t)
		head := e.get(name).Status.PullRequests[0].HeadSHA
		e.reviewUntil(name, func() bool { return len(e.verifyRuns(name)) > 0 })
		e.review(name, 1)
		if s := e.latestAcceptance(head); s.State != statusFailure ||
			!strings.HasSuffix(s.Description, "; automatic fix 1 of 2 running") {
			t.Fatalf("status = %+v, want failure with the first fix running", s)
		}
	})
	for _, gates := range []bool{true, false} {
		t.Run(map[bool]string{true: "manual gates", false: "manual open"}[gates], func(t *testing.T) {
			e, name, head, version, health := evaluatingIntent(t, func(p *v1alpha1.Project) {
				p.Spec.Verification.ManualGatesStatus = gates
			})
			e.evalRound(name, replays(version, health, v1alpha1.CheckRunPass, v1alpha1.CheckRunPass)...)
			e.review(name, 3)
			if s := e.latestAcceptance(head); s.State == statusSuccess {
				t.Fatalf("status = %+v, want never success with the manual outcome open", s)
			}
		})
	}
}

// TestAcceptanceStatusUnavailable: when maul could not run the checks the
// status is error, never success.
func TestAcceptanceStatusUnavailable(t *testing.T) {
	e, name, head, _, _ := evaluatingIntent(t)
	for range v1alpha1.MaxVerificationAttempts {
		e.playTools(e.verification(name).Ref.Name, v1alpha1.VerificationInfra, "", false)
		e.review(name, 3)
	}
	if v := e.verification(name); v.State != v1alpha1.VerificationUnavailable {
		t.Fatalf("verification = %+v, want Unavailable", v)
	}
	e.review(name, 2)
	if s := e.latestAcceptance(head); s.State != statusError {
		t.Fatalf("status = %+v, want error", s)
	}
}

// TestAcceptanceStatusNotPosted: nothing is posted on a Project that does
// not verify, or with the controller's verification off, on an intent maul
// never posted a status for: no status ever claims work maul did not do.
func TestAcceptanceStatusNotPosted(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    func(*testing.T) *env
	}{
		{"project off", func(t *testing.T) *env {
			e := newVerifyEnv(t, outcomesPlan, func(p *v1alpha1.Project) { p.Spec.Verification = nil })
			e.evaluatorOn(0.4)
			return e
		}},
		{"controller off", func(t *testing.T) *env {
			e := newVerifyEnv(t, outcomesPlan)
			e.verifying(false, true)
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.e(t)
			name := e.reviewing()
			e.readyPreview(name)
			e.review(name, 8)
			head := e.get(name).Status.PullRequests[0].HeadSHA
			if ss := e.acceptanceStatuses(head); len(ss) != 0 {
				t.Fatalf("statuses posted: %+v", ss)
			}
		})
	}
}

// manualOnlyPlan is outcomesPlan with its manual outcome alone: nothing maul
// checks.
var manualOnlyPlan = strings.Replace(validPlan, "estimated_token_budget: 200000\n",
	"estimated_token_budget: 200000\n"+
		"acceptance:\n"+
		"  - id: looks-right\n"+
		"    outcome: the version page looks right\n"+
		"    manual: true\n"+
		"    reason: only a human can judge it\n", 1)

// TestAcceptanceStatusWithNothingToCheck is the regression test for a
// required maul/acceptance that blocked an intent forever: a plan with no
// outcomes, or only manual ones, got no status at all. A verifying Project
// now posts one on every open head: pending until an approver accepts what
// only a human can (each manual outcome, or with none "/maul accept all"),
// then success naming them, and never green before.
func TestAcceptanceStatusWithNothingToCheck(t *testing.T) {
	for _, tc := range []struct {
		name, plan, accept, pending, success string
	}{
		{"no outcomes", validPlan, "/maul accept all",
			"no acceptance criteria declared; an approver may /maul accept all",
			"no acceptance criteria declared; accepted by " + approver},
		{"manual only", manualOnlyPlan, "/maul accept looks-right", "0/1 outcomes accepted by hand",
			"1/1 outcomes accepted by hand by " + approver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name := evaluatorReviewing(t, tc.plan)
			e.readyPreview(name)
			e.review(name, 6)
			head := e.get(name).Status.PullRequests[0].HeadSHA
			if s := e.latestAcceptance(head); s.State != statusPending || s.Description != tc.pending {
				t.Fatalf("before acceptance: %+v, want pending %q", s, tc.pending)
			}
			if len(e.verifications(name)) != 0 {
				t.Fatal("a plan with nothing to check was verified")
			}
			e.gh.comment(approver, tc.accept)
			e.settleActions(name)
			e.review(name, 2)
			if s := e.latestAcceptance(head); s.State != statusSuccess || s.Description != tc.success {
				t.Fatalf("after acceptance: %+v, want success %q", s, tc.success)
			}
		})
	}
}

// TestAcceptanceStatusSwitchedOff: when verification is switched off for an
// intent maul posted statuses for (the Project stops verifying, or the
// controller's verification, the evaluator, is turned off), its last status
// is replaced by a final error rather than left pending or green forever.
func TestAcceptanceStatusSwitchedOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  func(*env)
	}{
		{"project", func(e *env) {
			p := e.getProject()
			p.Spec.Verification = nil
			if err := e.c.Update(context.Background(), p); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"controller", func(e *env) { e.verifying(false, true); e.evaluating(false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name, head, _, _ := evaluatingIntent(t)
			e.review(name, 1)
			if s := e.latestAcceptance(head); s.State != statusPending {
				t.Fatalf("while evaluating: %+v", s)
			}
			tc.off(e)
			e.review(name, 4)
			if s := e.latestAcceptance(head); s.State != statusError || s.Description != switchedOff.description {
				t.Fatalf("once switched off: %+v", s)
			}
			n := len(e.acceptanceStatuses(head))
			e.review(name, 3)
			if got := len(e.acceptanceStatuses(head)); got != n {
				t.Fatalf("the final status was posted again: %d, had %d", got, n)
			}
		})
	}
}

// TestAcceptanceStatusSwitchedOffStaysOff: a later green at the head of an
// intent whose verification was switched off, maul's own from a pass that
// lost its record write or someone else's, does not stand: the final error
// is posted again.
func TestAcceptanceStatusSwitchedOffStaysOff(t *testing.T) {
	for _, creator := range []string{testBot, "mallory"} {
		t.Run(creator, func(t *testing.T) {
			e, name, head, _, _ := evaluatingIntent(t)
			e.review(name, 1)
			e.evaluating(false)
			e.review(name, 4)
			if s := e.latestAcceptance(head); s.State != statusError {
				t.Fatalf("once switched off: %+v", s)
			}
			e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: statusSuccess,
				Description: "1/1 outcomes confirmed", Creator: creator})
			e.review(name, 2)
			s := e.latestAcceptance(head)
			if s.Creator != testBot || s.State != statusError || s.Description != switchedOff.description {
				t.Fatalf("a later green stood on a switched-off intent: %+v", s)
			}
			if pr := e.get(name).Status.PullRequests[0]; pr.AcceptanceStatusID != s.ID {
				t.Fatalf("record = %d, want the re-posted %d", pr.AcceptanceStatusID, s.ID)
			}
		})
	}
}

// TestAcceptanceStatusImpostor: a later maul/acceptance someone else posted
// at the head is answered by maul's own state posted again, and the actor
// is named in the verification section; the statuses are listed at most
// once per poll interval, so a second impostor within it stands until the
// next.
func TestAcceptanceStatusImpostor(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.review(name, 2)
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: statusSuccess,
		Description: "all good", Creator: "mallory"})
	e.review(name, 2)
	s := e.latestAcceptance(head)
	if s.Creator == "mallory" || s.State != statusFailure {
		t.Fatalf("the impostor's status was not answered: %+v", s)
	}
	if pr := e.get(name).Status.PullRequests[0]; pr.AcceptanceStatusID != s.ID {
		t.Fatalf("record = %d, want the re-posted %d", pr.AcceptanceStatusID, s.ID)
	}
	n := e.get(name).Status.PullRequests[0].Number
	if body := e.onePreviewComment(name, n).Body; !strings.Contains(body, "`mallory`") {
		t.Fatalf("the preview comment does not name the impostor:\n%s", body)
	}
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: statusSuccess, Creator: "mallory"})
	e.mustIntent(name)
	if s := e.latestAcceptance(head); s.Creator != "mallory" {
		t.Fatalf("re-asserted twice within one poll interval: %+v", s)
	}
	e.review(name, 1)
	if s := e.latestAcceptance(head); s.Creator == "mallory" || s.State != statusFailure {
		t.Fatalf("not re-asserted after the poll interval: %+v", s)
	}
}

// TestAcceptanceStatusOwnLaterStatusIsNoImpostor: a later maul/acceptance
// the App's own bot posted (a status write lost after posting) is maul's,
// not an impostor's: in maul's own state, nothing is re-posted and no one
// is named (in another, TestAcceptanceStatusOwnLostWriteIsRestated).
func TestAcceptanceStatusOwnLaterStatusIsNoImpostor(t *testing.T) {
	e, name, head, _, _ := evaluatingIntent(t)
	e.review(name, 2)
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: e.latestAcceptance(head).State,
		Creator: testBot})
	n := len(e.acceptanceStatuses(head))
	e.review(name, 2)
	if got := len(e.acceptanceStatuses(head)); got != n {
		t.Fatalf("maul's own status was answered as an impostor's: %d statuses, had %d", got, n)
	}
}

// TestAcceptanceStatusOwnLostWriteIsRestated: a later maul/acceptance the
// App's own bot posted in another state than the verification's (a pass
// that posted it and lost its record write, as a restart does) is not left
// standing: maul's state is posted again and recorded, naming no one, so
// GitHub never keeps a stale green nor a stale red.
func TestAcceptanceStatusOwnLostWriteIsRestated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failing     bool
		want, stale string
	}{
		{"stale green", true, statusFailure, statusSuccess},
		{"stale red", false, statusPending, statusFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e *env
			var name string
			if tc.failing {
				e, name, _ = failingEvaluation(t)
			} else {
				e, name, _, _, _ = evaluatingIntent(t)
			}
			head := e.get(name).Status.PullRequests[0].HeadSHA
			e.review(name, 2)
			if s := e.latestAcceptance(head); s.State != tc.want {
				t.Fatalf("judged: %+v, want %s", s, tc.want)
			}
			e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: tc.stale,
				Description: "an earlier pass's", Creator: testBot})
			e.review(name, 2)
			s := e.latestAcceptance(head)
			if s.State != tc.want || s.Creator != testBot {
				t.Fatalf("after maul's own lost write: %+v, want %s restated", s, tc.want)
			}
			if pr := e.get(name).Status.PullRequests[0]; pr.AcceptanceStatusID != s.ID {
				t.Fatalf("record = %d, want the re-posted %d", pr.AcceptanceStatusID, s.ID)
			}
			n := e.get(name).Status.PullRequests[0].Number
			if body := e.onePreviewComment(name, n).Body; strings.Contains(body, "`"+testBot+"`") {
				t.Fatalf("the preview comment names maul's own bot:\n%s", body)
			}
		})
	}
}

// TestAcceptanceStatusRepostsWhenTheHeadSetMoves: the status is keyed by
// the combined change, so a sibling's move re-posts it on a head that did
// not move.
func TestAcceptanceStatusRepostsWhenTheHeadSetMoves(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.review(name, 1)
	n := len(e.acceptanceStatuses(head))
	in := e.get(name)
	in.Status.Verification.HeadSetDigest = digest([]byte("a sibling moved"))
	in.Status.Verification.State = v1alpha1.VerificationWaitingForCI
	clearJudgement(in.Status.Verification)
	p := &pass{r: e.intent, set: e.intent.Settings.withDefaults(), in: in, proj: e.getProject(),
		now: e.clock.Now()}
	if err := p.loadRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.syncAcceptanceStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	ss := e.acceptanceStatuses(head)
	if len(ss) != n+1 || ss[0].State != statusPending || ss[0].Description != headsMoved.description {
		t.Fatalf("statuses = %+v", ss)
	}
}

// TestAcceptanceStatusWithoutStatusesWrite: a verifying Project whose App
// may not post commit statuses stays Ready and verifies as usual; maul
// posts no maul/acceptance (nor lists the statuses for an impostor), and
// the verification comment and the pull request's verification section say
// why.
func TestAcceptanceStatusWithoutStatusesWrite(t *testing.T) {
	refuse := func(e *env) {
		e.gh.refused = map[ghclient.TokenPerms]error{{Statuses: ghclient.PermWrite}: ghError(
			http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")}
	}
	e, name := newEvaluatorEnv(t, 0.4)
	refuse(e)
	e.reconcileProject()
	if conds := e.getProject().Status.Conditions; !meta.IsStatusConditionTrue(conds, v1alpha1.ConditionReady) ||
		!meta.IsStatusConditionTrue(conds, ConditionAcceptanceStatusUnavailable) {
		t.Fatalf("project conditions = %+v", conds)
	}
	e.evaluateReporting(failingReport)
	version, health := savedCheck(t, versionCheck), savedCheck(t, healthCheck)
	e.storeChecks(name, version, health)
	e.review(name, 3)
	e.evalRound(name, replays(version, health, v1alpha1.CheckRunFail, v1alpha1.CheckRunPass)...)
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing && v.State != v1alpha1.VerificationFixing {
		t.Fatalf("not verified: %+v", v)
	}
	head := e.get(name).Status.PullRequests[0].HeadSHA
	if ss := e.acceptanceStatuses(head); len(ss) != 0 {
		t.Fatalf("statuses posted without statuses write: %+v", ss)
	}
	const why = "Commit statuses"
	if cs := e.verifyComments(name, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, why) {
		t.Fatalf("the verification comment does not say why no status is posted: %+v", cs)
	}
	n := e.get(name).Status.PullRequests[0].Number
	if body := e.onePreviewComment(name, n).Body; !strings.Contains(body, why) {
		t.Fatalf("the preview comment does not say why no status is posted:\n%s", body)
	}
}
