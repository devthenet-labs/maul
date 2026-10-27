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
	"github.com/devthenet-labs/maul/internal/acceptance"
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

// judged drives a new verifying intent to InReview, its Preview live, its
// first verification observed with do, and judged; it returns the intent
// and its pull request's head.
func (e *env) judged(do acceptance.Doer) (string, string) {
	e.t.Helper()
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved, do)
	e.review(name, 8)
	return name, e.get(name).Status.PullRequests[0].HeadSHA
}

// TestAcceptanceStatusFollowsTheVerification: maul/acceptance is pending
// while the verification runs and settles with it, on the pull request's
// head, recorded on its record and posted once per state: further passes
// post nothing.
func TestAcceptanceStatusFollowsTheVerification(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 6)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	if s := e.latestAcceptance(head); s.State != "pending" || !strings.HasPrefix(s.Description, "verifying") {
		t.Fatalf("while verifying: %+v", s)
	}
	e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved, answering(200))
	e.review(name, 8)
	s := e.latestAcceptance(head)
	if s.State != "success" || s.Description != "automated part verified, 1 manual open" {
		t.Fatalf("once passed: %+v", s)
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
}

// TestAcceptanceStatusStates: what each judged result posts. A failure is
// failure, and stays failure while the automatic fix it starts on the next
// pass runs, numbered as the first of the Project's maxVerifyFixes; a pass
// only on retry stays pending (never green); with manualGatesStatus an open
// manual criterion keeps it pending.
func TestAcceptanceStatusStates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		do          acceptance.Doer
		gates       bool
		state, desc string
	}{
		{"failure, fix running", answering(500), false, "failure", "0/1 automated passed; automatic fix 1 of 2 running"},
		{"quarantined", flaky, false, "pending", "1 passed only on retry"},
		{"manual gates", answering(200), true, "pending", "1/1 automated passed, 1 manual open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) {
				p.Spec.Verification.ManualGatesStatus = tc.gates
			})
			_, head := e.judged(tc.do)
			if s := e.latestAcceptance(head); s.State != tc.state || s.Description != tc.desc {
				t.Fatalf("status = %+v, want %s %q", s, tc.state, tc.desc)
			}
		})
	}
}

// TestAcceptanceStatusUnavailable: when maul could not run the checks the
// status is error, never success.
func TestAcceptanceStatusUnavailable(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	for attempt := int32(1); attempt <= v1alpha1.MaxVerificationAttempts; attempt++ {
		e.observe(v1alpha1.PreviewVerificationName(name, 1, attempt), v1alpha1.VerificationInfra, nil)
		e.review(name, 3)
	}
	e.review(name, 6)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	if s := e.latestAcceptance(head); s.State != "error" {
		t.Fatalf("status = %+v, want error", s)
	}
}

// TestAcceptanceStatusNotPosted: nothing is posted for a plan with nothing
// to check, on a Project that does not verify, or with the controller's
// verification off: no status ever claims work maul did not do.
func TestAcceptanceStatusNotPosted(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    func(*testing.T) *env
	}{
		{"not declared", func(t *testing.T) *env { return newVerifyEnv(t, "") }},
		{"manual only", func(t *testing.T) *env {
			return newVerifyEnv(t, strings.Replace(acceptancePlan,
				"  - id: health\n    kind: http\n    statement: the app answers its health check\n    target: app\n"+
					"    request: {method: GET, path: /healthz}\n    expect: {status: 200}\n", "", 1))
		}},
		{"project off", func(t *testing.T) *env {
			return newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) { p.Spec.Verification = nil })
		}},
		{"controller off", func(t *testing.T) *env {
			e := newVerifyEnv(t, acceptancePlan)
			e.intent.Settings.Verification = false
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

// TestAcceptanceStatusImpostor: a later maul/acceptance someone else posted
// at the head is answered by maul's own state posted again, and the actor
// is named in the verification section; the statuses are listed at most
// once per poll interval, so a second impostor within it stands until the
// next.
func TestAcceptanceStatusImpostor(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, head := e.judged(answering(500))
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: "success",
		Description: "all good", Creator: "mallory"})
	e.review(name, 2)
	s := e.latestAcceptance(head)
	if s.Creator == "mallory" || s.State != "failure" {
		t.Fatalf("the impostor's status was not answered: %+v", s)
	}
	if pr := e.get(name).Status.PullRequests[0]; pr.AcceptanceStatusID != s.ID {
		t.Fatalf("record = %d, want the re-posted %d", pr.AcceptanceStatusID, s.ID)
	}
	n := e.get(name).Status.PullRequests[0].Number
	if body := e.onePreviewComment(name, n).Body; !strings.Contains(body, "`mallory`") {
		t.Fatalf("the preview comment does not name the impostor:\n%s", body)
	}
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: "success", Creator: "mallory"})
	e.mustIntent(name)
	if s := e.latestAcceptance(head); s.Creator != "mallory" {
		t.Fatalf("re-asserted twice within one poll interval: %+v", s)
	}
	e.review(name, 1)
	if s := e.latestAcceptance(head); s.Creator == "mallory" || s.State != "failure" {
		t.Fatalf("not re-asserted after the poll interval: %+v", s)
	}
}

// TestAcceptanceStatusOwnLaterStatusIsNoImpostor: a later maul/acceptance
// the App's own bot posted (a status write lost after posting) is maul's,
// not an impostor's: nothing is re-posted and no one is named.
func TestAcceptanceStatusOwnLaterStatusIsNoImpostor(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, head := e.judged(answering(200))
	e.postStatus(head, ghclient.CommitStatus{Context: acceptanceContext, State: "success", Creator: testBot})
	n := len(e.acceptanceStatuses(head))
	e.review(name, 2)
	if got := len(e.acceptanceStatuses(head)); got != n {
		t.Fatalf("maul's own status was answered as an impostor's: %d statuses, had %d", got, n)
	}
}

// TestAcceptanceStatusRepostsWhenTheHeadSetMoves: the status is keyed by
// the combined change, so a sibling's move re-posts it on a head that did
// not move.
func TestAcceptanceStatusRepostsWhenTheHeadSetMoves(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, head := e.judged(answering(200))
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
	if len(ss) != n+1 || ss[0].State != "pending" || ss[0].Description != "combined change moved: re-verifying" {
		t.Fatalf("statuses = %+v", ss)
	}
}

// TestAcceptanceStatusWithoutStatusesWrite: a verifying Project whose App
// may not post commit statuses stays Ready and verifies as usual; maul
// posts no maul/acceptance (nor lists the statuses for an impostor), and
// the verification comment and the pull request's verification section say
// why.
func TestAcceptanceStatusWithoutStatusesWrite(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.gh.refused = map[ghclient.TokenPerms]error{{Statuses: ghclient.PermWrite}: ghError(http.StatusUnprocessableEntity,
		"The permissions requested are not granted to this installation.")}
	e.reconcileProject()
	if conds := e.getProject().Status.Conditions; !meta.IsStatusConditionTrue(conds, v1alpha1.ConditionReady) ||
		!meta.IsStatusConditionTrue(conds, ConditionAcceptanceStatusUnavailable) {
		t.Fatalf("project conditions = %+v", conds)
	}
	name, head := e.judged(answering(500))
	if v := e.verification(name); v.State != v1alpha1.VerificationFailing && v.State != v1alpha1.VerificationFixing {
		t.Fatalf("not verified: %+v", v)
	}
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
