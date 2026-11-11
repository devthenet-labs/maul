// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The outcome twins of verify_comments_test.go, verify_numbering_test.go
// and verify_mismatch_test.go (S4): the issue's verification comment, the
// preview comment's section and the status line of an outcome round. Each
// test is named after the criteria test it twins.

// TestOutcomeVerifyCommentOncePerRound twins TestVerifyCommentOncePerRound.
func TestOutcomeVerifyCommentOncePerRound(t *testing.T) {
	e, name, _, _ := outcomeEnv(t)
	if cs := e.verifyComments(name, 1); len(cs) != 0 {
		t.Fatalf("posted while the round runs: %+v", cs)
	}
	e.evalRound(name, versionFails(t, healthCheck)...)
	e.review(name, 8)
	cs := e.verifyComments(name, 1)
	if len(cs) != 1 || len(e.verifyComments(name, 2)) != 0 {
		t.Fatalf("verification comments of round 1 = %d, want one and none of round 2", len(cs))
	}
	got := cs[0].Body
	for _, want := range []string{"verification 1, attempt 1", "| `version-shown` | `change` | failed",
		"| `still-healthy` | `keep` | confirmed", "| `looks-right` |", "only a human can close it"} {
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
	output := e.jobs.output
	e.evaluatorOn(0.4)
	e.jobs.output = output
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

// TestOutcomeVerifySectionOnThePreviewComment twins
// TestVerifySectionOnThePreviewComment.
func TestOutcomeVerifySectionOnThePreviewComment(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 3)
	pr := e.get(name).Status.PullRequests[0]
	body := e.onePreviewComment(name, pr.Number).Body
	for _, want := range []string{"#### maul: acceptance checks", "- `version-shown` (`change`): failed; " +
		"maul's check: `http to app: POST /notes, GET /version?q={{run}}; 3 expectations`. GET /version answers " +
		"with the commit it runs", "Evaluation 1 failing:"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview comment lacks %q:\n%s", want, body)
		}
	}
	if pr.VerifyDigest == "" {
		t.Error("the verification section's digest is not recorded")
	}
}

// TestOutcomeVerifyLogTailsOnlyWhereAllowed twins
// TestVerifyLogTailsOnlyWhereAllowed: the tail is the outcome round's own
// capture of its environment.
func TestOutcomeVerifyLogTailsOnlyWhereAllowed(t *testing.T) {
	for _, postLogs := range []bool{false, true} {
		e, name := newEvaluatorEnv(t, 0.4, func(p *v1alpha1.Project) { p.Spec.Verification.PostLogs = postLogs })
		e.evaluateReporting(failingReport)
		e.storeChecks(name, savedCheck(t, versionCheck), savedCheck(t, healthCheck))
		e.review(name, 3)
		pv := e.pvNamed(e.onlyVerification(name).Name)
		// The tool pod's own part is never a component's log: never shown.
		pv.Status.Environment = []v1alpha1.PartFact{{Name: "app", Role: v1alpha1.PreviewRoleComponent,
			Waiting: "CrashLoopBackOff", Restarts: 4, LogTail: "panic: nil map"},
			{Name: "tools", Role: v1alpha1.PreviewRoleTools, Ready: true, LogTail: "tool pod line"}}
		if err := e.c.Update(t.Context(), pv); err != nil {
			t.Fatal(err)
		}
		e.evalRound(name, versionFails(t, healthCheck)...)
		e.review(name, 8)
		cs := e.verifyComments(name, 1)
		if len(cs) != 1 || !strings.Contains(cs[0].Body, "panic: nil map") {
			t.Fatalf("postLogs %t: issue comments %+v", postLogs, cs)
		}
		n := e.get(name).Status.PullRequests[0].Number
		pr := e.onePreviewComment(name, n).Body
		if got := strings.Contains(pr, "panic: nil map"); got != postLogs {
			t.Fatalf("postLogs %t: the pull request shows the log tail: %t", postLogs, got)
		}
		if strings.Contains(cs[0].Body, "tool pod line") || strings.Contains(pr, "tool pod line") {
			t.Fatalf("postLogs %t: a part that is no component's log is shown", postLogs)
		}
	}
}

// TestOutcomeVerifyStatusLine twins TestVerifyStatusLine on a failing
// evaluation; TestEvaluationConfirmsAndHandsOver holds a passing one's.
func TestOutcomeVerifyStatusLine(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.review(name, 8)
	if body := e.statusBody(); !strings.Contains(body,
		"Evaluation 1 failing: 1/3 outcomes confirmed, 1 of 2 automatic fixes used, 1 for a human") {
		t.Fatalf("status comment:\n%s", body)
	}
}

// TestOutcomeVerifyStatusLineNotDeclared twins
// TestVerifyStatusLineNotDeclared: a plan stating no outcomes under the
// evaluator.
func TestOutcomeVerifyStatusLineNotDeclared(t *testing.T) {
	e, name := evaluatorReviewing(t, validPlan)
	e.readyPreview(name)
	e.review(name, 8)
	if v := e.verification(name); v.State != v1alpha1.VerificationNotDeclared {
		t.Fatalf("verification = %s, want NotDeclared", v.State)
	}
	if body := e.statusBody(); !strings.Contains(body, "No acceptance outcomes were declared.") {
		t.Fatalf("status comment:\n%s", body)
	}
	if cs := e.verifyComments(name, 1); len(cs) != 0 {
		t.Fatalf("verification comment posted: %+v", cs)
	}
}

// TestOutcomeVerifyCommentNumbering twins TestVerifyCommentNumbering.
func TestOutcomeVerifyCommentNumbering(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.fixedAndFailingAgain(name, versionFails(t, healthCheck)...)
	comments := e.gh.withMarker("verify-r2")
	if len(comments) != 1 {
		t.Fatalf("verification 2 comments = %d, want one", len(comments))
	}
	body := comments[0].Body
	for _, want := range []string{"### maul: verification 2, attempt 1", "Evaluation 2 failing",
		"1 of 2 automatic fixes used"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment omits %q:\n%s", want, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "round") {
		t.Errorf("comment numbers something as a round:\n%s", body)
	}
	first := e.gh.withMarker("verify-r1")
	if len(first) != 1 || !strings.Contains(first[0].Body, "### maul: verification 1, attempt 1") ||
		!strings.Contains(first[0].Body, "Evaluation 1 failing") ||
		!strings.Contains(first[0].Body, "1 of 2 automatic fixes used") {
		t.Errorf("verification 1 comment = %+v", first)
	}
}

// TestOutcomeVerifyCommentNamesWhatFailed twins
// TestVerifyCommentNamesWhatFailed: an outcome's failure is named by the
// failing check's step as the tools ledger recorded it, fenced and with its
// mention made inert; a passing check's steps are not listed. (The criteria
// test's per-matcher lines are criteria-only rendering, stepMismatches.)
func TestOutcomeVerifyCommentNamesWhatFailed(t *testing.T) {
	e, name := newEvaluatorEnv(t, 0.4)
	e.failRound(name, failingReport, func(version string) []toolwire.LedgerEntry {
		return []toolwire.LedgerEntry{
			checkRunEntry(2, version, "version-shown", 1, `/note: expected "t1-v", got "p_4e1013 @octocat"`),
			checkRunEntry(3, savedCheck(t, healthCheck).Digest, "still-healthy", 0, "never shown"),
		}
	})
	e.review(name, 3)
	pv := e.pvNamed(e.verification(name).Ref.Name)
	outcomeRequest(t, pv)
	cs := e.verifyComments(name, 1)
	if len(cs) != 1 {
		t.Fatalf("verification comments = %d, want one", len(cs))
	}
	got := cs[0].Body
	want := `attempt 1, step 2: /note: expected "t1-v", got "p_4e1013 @ octocat"`
	i := strings.Index(got, want)
	if i < 0 {
		t.Fatalf("comment lacks %q:\n%s", want, got)
	}
	if strings.Count(got[:i], "```")%2 != 1 {
		t.Errorf("the application's value is not fenced:\n%s", got)
	}
	if strings.Contains(got, "@octocat") || strings.Contains(got, "never shown") {
		t.Errorf("comment carries a live mention or lists the passing check's step:\n%s", got)
	}
}
