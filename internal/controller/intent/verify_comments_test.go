// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/acceptance"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// verifyComments are maul's verification comments of round on the issue.
func (e *env) verifyComments(name string, round int32) []*ghclient.Comment {
	marker := templates.NoticeMarker(testNS, name, templates.VerifyKey(round))
	var out []*ghclient.Comment
	for _, c := range e.gh.ownComments() {
		if markerOf(c.Body) == marker {
			out = append(out, c)
		}
	}
	return out
}

// TestVerifyCommentOncePerRound: the issue gets one verification comment
// per round, once the round is judged (not while it runs), naming each
// criterion's outcome and a failure's evidence in maul's words; passes and
// a restart that change nothing post or edit nothing.
func TestVerifyCommentOncePerRound(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 3)
	if cs := e.verifyComments(name, 1); len(cs) != 0 {
		t.Fatalf("posted while the round runs: %+v", cs)
	}
	e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationObserved,
		func(acceptance.Call) acceptance.Response {
			return acceptance.Response{Status: 500, Body: []byte(`{"error":"boom"}`)}
		})
	e.review(name, 8)
	cs := e.verifyComments(name, 1)
	if len(cs) != 1 || len(e.verifyComments(name, 2)) != 0 {
		t.Fatalf("verification comments of round 1 = %d, want one and none of round 2", len(cs))
	}
	got := cs[0].Body
	for _, want := range []string{"round 1 (attempt 1)", "| `health` | `http` | failed |", "Expected `GET /healthz: " +
		"status 200`", "Observed `status 500`", `{"error":"boom"}`, "sounds-good", "needs a human"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment lacks %q:\n%s", want, got)
		}
	}
	id := cs[0].ID
	e.gh.mu.Lock()
	e.gh.edited[id] = false
	e.gh.mu.Unlock()
	e.review(name, 3)
	e.restart()
	e.intent.Settings.Previews, e.intent.Settings.Verification = true, true
	e.review(name, 3)
	if cs := e.verifyComments(name, 1); len(cs) != 1 || cs[0].Body != got {
		t.Fatalf("after more passes and a restart: %d comments", len(cs))
	}
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	if e.gh.edited[id] {
		t.Fatal("the unchanged verification comment was edited")
	}
}

// TestVerifySectionOnThePreviewComment: the pull request's preview comment
// carries the verification section, and its digest is recorded.
func TestVerifySectionOnThePreviewComment(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	name, _ := e.judged(answering(500))
	pr := e.get(name).Status.PullRequests[0]
	body := e.onePreviewComment(name, pr.Number).Body
	for _, want := range []string{"#### maul: acceptance checks", "- `health` (`http`): failed. the app answers " +
		"its health check", "Verification failing:"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview comment lacks %q:\n%s", want, body)
		}
	}
	if pr.VerifyDigest == "" {
		t.Error("the verification section's digest is not recorded")
	}
}

// TestVerifyLogTailsOnlyWhereAllowed: a component's log tail is shown on
// the issue, and on its pull request only under the Project's postLogs.
func TestVerifyLogTailsOnlyWhereAllowed(t *testing.T) {
	for _, postLogs := range []bool{false, true} {
		e := newVerifyEnv(t, acceptancePlan, func(p *v1alpha1.Project) { p.Spec.Verification.PostLogs = postLogs })
		name := e.reviewing()
		e.readyPreview(name)
		e.review(name, 3)
		e.observe(e.onlyVerification(name).Name, v1alpha1.VerificationEnvFailed, nil, v1alpha1.PartFact{
			Name: "app", Role: v1alpha1.PreviewRoleComponent, Waiting: "CrashLoopBackOff", Restarts: 4,
			LogTail: "panic: nil map"})
		e.review(name, 8)
		cs := e.verifyComments(name, 1)
		if len(cs) != 1 || !strings.Contains(cs[0].Body, "panic: nil map") {
			t.Fatalf("postLogs %t: issue comments %+v", postLogs, cs)
		}
		n := e.get(name).Status.PullRequests[0].Number
		if got := strings.Contains(e.onePreviewComment(name, n).Body, "panic: nil map"); got != postLogs {
			t.Fatalf("postLogs %t: the pull request shows the log tail: %t", postLogs, got)
		}
	}
}

// TestVerifyStatusLine: the status comment's preview line says where
// verification stands.
func TestVerifyStatusLine(t *testing.T) {
	e := newVerifyEnv(t, acceptancePlan)
	e.judged(answering(200))
	if body := e.statusBody(); !strings.Contains(body, "Verified: 1/1 checks passed, 1 for a human.") {
		t.Fatalf("status comment:\n%s", body)
	}
}

// TestVerifyStatusLineNotDeclared: a plan with no criteria says so beside
// the preview, and posts no verification comment.
func TestVerifyStatusLineNotDeclared(t *testing.T) {
	e := newVerifyEnv(t, "")
	name := e.reviewing()
	e.readyPreview(name)
	e.review(name, 8)
	if body := e.statusBody(); !strings.Contains(body, "No acceptance criteria were declared.") {
		t.Fatalf("status comment:\n%s", body)
	}
	if cs := e.verifyComments(name, 1); len(cs) != 0 {
		t.Fatalf("verification comment posted: %+v", cs)
	}
}
