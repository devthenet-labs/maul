// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/command"
	"github.com/devthenet-labs/maul/internal/templates"
)

// expiredWords is how maul says a /maul verify did not run because the
// intent's preview expired at the commits it would check.
const expiredWords = "the preview expired after its time to live"

// passedSlotRound is newEvaluatorEnv with its first round judged Passed at
// the pull requests' heads and settled, maul/acceptance posted for it: what
// beatbattle-60 was before its /maul verify (gate D-B5). It returns the env,
// the intent's name, the round and the pull request's head.
func passedSlotRound(t *testing.T) (*env, string, v1alpha1.PreviewVerification, string) {
	t.Helper()
	e, name := newEvaluatorEnv(t, 0.4)
	e.review(name, 3)
	pv := e.onlyVerification(name)
	e.passAgentsRound(name, pv)
	e.review(name, 2)
	head := e.get(name).Status.PullRequests[0].HeadSHA
	e.reviewUntil(name, func() bool { return e.latestAcceptance(head).State == statusSuccess })
	if v := e.verification(name); v.State != v1alpha1.VerificationPassed || v.Ref == nil || v.Round != 1 {
		t.Fatalf("verification = %+v, want round 1 Passed", v)
	}
	return e, name, pv, head
}

// TestVerifyOnAnExpiredPreviewIsAnsweredNotRun (gate D-B5): an approver's
// /maul verify on an intent whose Preview has Expired at the heads it
// would check is answered at once that it cannot run and why, and that a
// push or a reset deploys the preview again: preview-controller never
// redeploys an Expired Preview at an unchanged spec, so nothing would ever
// run it. Nothing is recorded and the judgement on record stands: the
// state stays Passed and maul/acceptance success. Before: the request was
// acknowledged "recorded", the state waited an hour for a preview that
// never came, then went Unavailable, and maul/acceptance read pending
// "combined change moved" meanwhile though nothing had moved. A reset
// changes the Preview's spec, so it is recorded as before; so is a request
// while the preview is being deployed, or not expired.
func TestVerifyOnAnExpiredPreviewIsAnsweredNotRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   v1alpha1.PreviewPhase
		command string
		notRun  bool
	}{
		{"expired", v1alpha1.PreviewExpired, "/maul verify", true},
		{"expired, fix", v1alpha1.PreviewExpired, "/maul verify fix", true},
		{"expired, reset", v1alpha1.PreviewExpired, "/maul verify --reset", false},
		{"failed", v1alpha1.PreviewFailed, "/maul verify", false},
		{"deploying", v1alpha1.PreviewDeploying, "/maul verify", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, name, pv, head := passedSlotRound(t)
			e.servePreview(name, tc.phase, "")
			e.review(name, 2)
			before := len(e.acceptanceStatuses(head))

			e.clock.Advance(time.Minute)
			id := e.gh.comment(approver, tc.command)
			e.settleActions(name)
			e.review(name, 3)

			v := e.verification(name)
			body := e.onlyReply(id)
			if !tc.notRun {
				if v.Requested == nil && v.Ref.Name == pv.Name {
					t.Fatalf("verification = %+v, want the request recorded", v)
				}
				if strings.Contains(body, "did not run") || strings.Contains(body, expiredWords) {
					t.Fatalf("reply = %s, want the request acknowledged", body)
				}
				return
			}
			e.expiredNotRun(name, pv, tc.command, body)
			if ss := e.acceptanceStatuses(head); len(ss) != before || ss[0].State != statusSuccess {
				t.Fatalf("maul/acceptance after the request = %+v, want success unchanged", ss[0])
			}
		})
	}
}

// expiredNotRun fails unless the request cmd, answered body, did not run
// on the expired preview of name: round 1 (pv) Passed standing, nothing
// recorded or re-armed, and the reply saying why and what deploys it.
func (e *env) expiredNotRun(name string, pv v1alpha1.PreviewVerification, cmd, body string) {
	e.t.Helper()
	v := e.verification(name)
	if v.State != v1alpha1.VerificationPassed || v.Requested != nil || v.Round != 1 ||
		v.PreviewWaitSince != nil || len(v.JudgedOutcomes()) == 0 {
		e.t.Fatalf("verification = %+v, want round 1's Passed standing and nothing recorded", v)
	}
	if vs := e.verifications(name); len(vs) != 1 || vs[0].Name != pv.Name {
		e.t.Fatalf("verifications = %d, want only round 1's", len(vs))
	}
	if v.AutoFix.Rearm != nil {
		e.t.Fatalf("auto-fix = %+v, want nothing re-armed by a request that did not run", v.AutoFix)
	}
	if !strings.Contains(body, "did not run: "+expiredWords) || !strings.Contains(body, "`"+cmd+"`") ||
		!strings.Contains(body, "Evaluation 1's result at these commits stands") ||
		!strings.Contains(body, "`/maul verify --reset`") || strings.Contains(body, "is recorded") {
		e.t.Fatalf("reply = %s", body)
	}
}

// TestPendingRequestOnAnExpiredPreviewIsAnswered (gate D-B5): a request
// recorded before the Preview was found Expired at the heads (it expired
// in between, or its read failed when the command was settled) is answered
// that it did not run, and the judgement on record stands; it never waits
// an hour for a preview that does not come. A request recorded with a
// reset the Preview has yet to copy waits for it.
func TestPendingRequestOnAnExpiredPreviewIsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
	}{{"plain", false}, {"reset not yet copied", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e, name, _, _ := passedSlotRound(t)
			e.servePreview(name, v1alpha1.PreviewExpired, "")
			e.setStatus(name, func(in *v1alpha1.Intent) {
				v := in.Status.Verification
				v.Requested = &v1alpha1.VerifyRequest{By: approver, ByID: 7, At: metav1.NewTime(e.clock.Now()),
					CommentID: 99, Reset: tc.reset}
				if tc.reset {
					v.Reset.Generation++
				}
			})
			// The pass under test, before the preview projection copies a
			// reset onto the Preview.
			e.review(name, 1)
			v := e.verification(name)
			if tc.reset {
				if v.Requested == nil || len(e.notRunReplies()) != 0 {
					t.Fatalf("verification = %+v, replies %q; want the reset request kept", v, e.notRunReplies())
				}
				return
			}
			if v.State != v1alpha1.VerificationPassed || v.Requested != nil || v.PreviewWaitSince != nil {
				t.Fatalf("verification = %+v, want round 1's Passed standing and the request consumed", v)
			}
			replies := e.notRunReplies()
			if len(replies) != 1 || !strings.Contains(replies[0], expiredWords) ||
				!strings.Contains(replies[0], "Evaluation 1's result at these commits stands") {
				t.Fatalf("not-run replies = %q", replies)
			}
		})
	}
}

// TestWaitingAtUnchangedHeadsSaysNothingMoved (gate D-B5): a requested
// round waiting for the preview at the heads the round on record judged
// (a reset redeploying it) is pending in maul/acceptance without claiming
// the combined change moved. A move still says so
// (TestAcceptanceStatusRepostsWhenTheHeadSetMoves).
func TestWaitingAtUnchangedHeadsSaysNothingMoved(t *testing.T) {
	e, name, _, head := passedSlotRound(t)
	e.servePreview(name, v1alpha1.PreviewDeploying, "")
	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.reviewUntil(name, func() bool { return e.latestAcceptance(head).State == statusPending })
	if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForPreview || v.Requested == nil {
		t.Fatalf("verification = %+v, want the request waiting for the preview", v)
	}
	if st := e.latestAcceptance(head); strings.Contains(st.Description, "moved") ||
		!strings.Contains(st.Description, "preview") {
		t.Fatalf("maul/acceptance = %+v, want it pending for the preview, nothing moved", st)
	}
}

// TestRequestWaitingForPreviewThatExpires (gate D-B5 edge): a request
// recorded while the preview was being deployed waits for it
// (WaitingForPreview); the Preview then expires at the heads (queued past
// its time to live) before it ever went live. Nothing deploys it again, so
// the request is answered that it did not run and consumed, at once, no
// judgement claimed (the wait replaced it), and the wait it began ends
// Unavailable (PreviewNotLive) as its bound would, maul/acceptance saying
// no more than that the preview did not become live in time. Before, it
// waited the hour, went Unavailable and stayed recorded, never answered.
// A command made while the wait goes on, on the expired preview, is
// answered at once the same way.
func TestRequestWaitingForPreviewThatExpires(t *testing.T) {
	e, name, _, head := passedSlotRound(t)
	e.servePreview(name, v1alpha1.PreviewDeploying, "")
	e.clock.Advance(time.Minute)
	first := e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	e.review(name, 2)
	if v := e.verification(name); v.State != v1alpha1.VerificationWaitingForPreview || v.Requested == nil ||
		v.Requested.CommentID != first {
		t.Fatalf("verification = %+v, want the request waiting for the preview", v)
	}
	e.servePreview(name, v1alpha1.PreviewExpired, "")
	e.clock.Advance(time.Minute)
	second := e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	if body := e.onlyReply(second); !strings.Contains(body, "did not run: "+expiredWords) ||
		strings.Contains(body, "stands") {
		t.Fatalf("reply to the second request = %s, want it not run, no judgement claimed", body)
	}
	e.review(name, 3)
	v := e.verification(name)
	if v.Requested != nil || !previewNotLive(v) {
		t.Fatalf("verification = %+v, want the waiting request consumed and the wait Unavailable", v)
	}
	if replies := e.notRunReplies(); len(replies) != 1 || !strings.Contains(replies[0], expiredWords) ||
		strings.Contains(replies[0], "stands") {
		t.Fatalf("not-run replies = %q, want the waiting request answered once", replies)
	}
	if st := e.latestAcceptance(head); st.State != statusError || strings.Contains(st.Description, "minutes") ||
		!strings.Contains(st.Description, "did not become live") {
		t.Fatalf("maul/acceptance = %+v, want an error saying only that the preview did not become live", st)
	}
	e.review(name, 3)
	if replies := e.notRunReplies(); len(replies) != 1 {
		t.Fatalf("not-run replies = %q, want still the one", replies)
	}
}

// TestExpiredPreviewWhileRevising (gate D-B5): while a round revises the
// intent, its push deploys the preview again, so a request is recorded to
// run after it, as before; only outside a revision is an expired preview
// final.
func TestExpiredPreviewWhileRevising(t *testing.T) {
	e, name, _, _ := passedSlotRound(t)
	e.servePreview(name, v1alpha1.PreviewExpired, "")
	for _, tc := range []struct {
		phase v1alpha1.IntentPhase
		want  bool
	}{{v1alpha1.IntentInReview, true}, {v1alpha1.IntentRevising, false}} {
		in := e.get(name)
		in.Status.Phase = tc.phase
		p := &pass{r: e.intent, set: e.intent.Settings.withDefaults(), in: in, proj: e.getProject(),
			now: e.clock.Now()}
		if got := p.previewExpired(context.Background(), in.Status.Verification.Reset.Generation); got != tc.want {
			t.Errorf("%s: previewExpired = %v, want %v", tc.phase, got, tc.want)
		}
	}
}

// TestMovedSinceJudged (gate D-B5): a waiting round's status says the
// combined change moved only when it did: the judged head set is on record.
func TestMovedSinceJudged(t *testing.T) {
	ref := &v1alpha1.ObjectReference{Name: "r1"}
	req := &v1alpha1.VerifyRequest{By: approver, CommentID: 9}
	for _, tc := range []struct {
		name string
		v    v1alpha1.IntentVerification
		want bool
	}{
		{"outcomes judged at other heads",
			v1alpha1.IntentVerification{HeadSetDigest: "b", OutcomesHeadSetDigest: "a"}, true},
		{"outcomes judged at these heads, a request",
			v1alpha1.IntentVerification{HeadSetDigest: "a", OutcomesHeadSetDigest: "a", Ref: ref, Requested: req}, false},
		{"outcomes judged at these heads, a retry or resume",
			v1alpha1.IntentVerification{HeadSetDigest: "a", OutcomesHeadSetDigest: "a"}, false},
		{"outcomes never judged", v1alpha1.IntentVerification{HeadSetDigest: "a"}, false},
	} {
		if got := movedSinceJudged(&tc.v); got != tc.want {
			t.Errorf("%s: moved = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestOutcomeStatusSaysMovedOnAPush (gate D-B5): a push after a judged
// outcome round still reads "combined change moved" while the new head set
// waits.
func TestOutcomeStatusSaysMovedOnAPush(t *testing.T) {
	e, name, _, _ := passedSlotRound(t)
	moved := strings.Repeat("e", 40)
	e.pushHead(e.prNumber(name), moved)
	e.reviewUntil(name, func() bool { return len(e.acceptanceStatuses(moved)) > 0 })
	if v := e.verification(name); v.HeadSetDigest == v.OutcomesHeadSetDigest {
		t.Fatalf("verification = %+v, want it at the pushed head set", v)
	}
	if st := e.latestAcceptance(moved); st.State != statusPending || st.Description != headsMoved.description {
		t.Fatalf("maul/acceptance on the pushed head = %+v, want %q", st, headsMoved.description)
	}
}

// TestVerifyFixAtMovedHeadsIsRecorded (#396 review): a /maul verify fix
// settled after a push to the pull request was recorded but before the
// head set followed it (a pass later, syncHeadSet), on a Failing judgement
// of the old heads and a Preview reading Expired, is recorded: a moved head
// is a redeploy pending, and the judgement on record is about other
// commits, so nothing at the moved heads is known yet. Before, it was
// answered that the preview expired. Once the head set follows, the gate
// decides it, and it never waits for nothing.
func TestVerifyFixAtMovedHeadsIsRecorded(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	moved := strings.Repeat("e", 40)
	e.pushHead(e.prNumber(name), moved)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.PullRequests[0].HeadSHA = moved // the poll recorded the push
	})
	e.servePreview(name, v1alpha1.PreviewExpired, "")
	e.clock.Advance(time.Minute)
	p := e.prVerifyPass(name)
	if p.atHeads(p.in.Status.Verification) {
		t.Fatal("the head set already follows the push; nothing under test")
	}
	n, err := p.applyVerifyCommand(context.Background(), verifyCommand{surface: command.IntentIssue,
		key: "comment-77", id: 77, at: e.clock.Now(), actor: actorOf(approver), verb: action.VerbVerify,
		body: "/maul verify fix"})
	if err != nil {
		t.Fatal(err)
	}
	v := e.verification(name)
	if n.Outcome != templates.VerifyRequested || v.Requested == nil || v.Requested.CommentID != 77 {
		t.Fatalf("reply %s (why %q), request %+v; want the request recorded at the moved heads", n.Outcome,
			n.NotRunWhy, v.Requested)
	}
	// Here the Preview reads Expired at the moved heads too, so once the
	// head set follows, the gate answers that the request did not run.
	e.review(name, 4)
	v = e.verification(name)
	if replies := e.notRunReplies(); v.Requested != nil || v.HeadSetDigest == v.OutcomesHeadSetDigest ||
		len(replies) != 1 || !strings.Contains(replies[0], expiredWords) {
		t.Fatalf("verification = %+v, not-run replies %q; want the request decided at the moved heads", v, replies)
	}
}

// TestVerifyFixOnAnExpiredPreviewAfterExhausted: an Exhausted judgement
// (a fix round found no defect, and the round after it still failed) starts
// no fix round of its own (startVerifyFix fixes only a Failing one): the
// approver's /maul verify fix needs a new round, which nothing can run on a
// Preview expired at the heads. It is answered that it did not run, the
// judgement standing and nothing re-armed, and says what deploys the
// preview again; a reset, which deploys it again, is recorded.
func TestVerifyFixOnAnExpiredPreviewAfterExhausted(t *testing.T) {
	for _, tc := range []struct {
		command string
		notRun  bool
	}{{"/maul verify fix", true}, {"/maul verify --reset", false}} {
		t.Run(tc.command, func(t *testing.T) {
			e, name, _ := failingEvaluation(t)
			e.setStatus(name, func(in *v1alpha1.Intent) {
				v := in.Status.Verification
				v.State = v1alpha1.VerificationExhausted
				v.NoDefect = &v1alpha1.NoDefectRecord{Run: "x-rev-r2-a1", At: metav1.NewTime(e.clock.Now()),
					HeadSetDigest: v.HeadSetDigest}
				v.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixFixFoundNoDefect}
			})
			e.servePreview(name, v1alpha1.PreviewExpired, "")
			e.review(name, 2)
			e.clock.Advance(time.Minute)
			id := e.gh.comment(approver, tc.command)
			e.settleActions(name)
			body, v := e.onlyReply(id), e.verification(name)
			if !tc.notRun {
				if v.Requested == nil || strings.Contains(body, "did not run") {
					t.Fatalf("reply = %s, verification %+v; want the reset recorded", body, v)
				}
				return
			}
			if !strings.Contains(body, "did not run: "+expiredWords) || v.Requested != nil ||
				v.AutoFix.Rearm != nil || v.State != v1alpha1.VerificationExhausted {
				t.Fatalf("reply = %s, verification %+v; want it not run and the judgement standing", body, v)
			}
		})
	}
}

// TestVerifyFixOnAnExpiredPreviewFixesAStandingFailure (gate D-B5 review):
// a /maul verify fix at heads whose failing judgement stands re-arms
// automatic fixes, and the fix round it starts pushes, which deploys the
// expired preview again: so the request is recorded and the round starts,
// never answered that nothing would run it.
func TestVerifyFixOnAnExpiredPreviewFixesAStandingFailure(t *testing.T) {
	e, name, _ := failingEvaluation(t)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted,
			Reason: v1alpha1.AutoFixHumanReview}
	})
	e.servePreview(name, v1alpha1.PreviewExpired, "")
	e.review(name, 2)
	if n := len(e.verifyRuns(name)); n != 0 {
		t.Fatalf("verify runs before the request = %d, want none while halted", n)
	}
	e.clock.Advance(time.Minute)
	id := e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	if body := e.onlyReply(id); strings.Contains(body, "did not run") {
		t.Fatalf("reply = %s, want the fix request recorded", body)
	}
	e.review(name, 3)
	if n := len(e.verifyRuns(name)); n != 1 {
		t.Fatalf("verify runs after /maul verify fix = %d, want the fix round started", n)
	}
}
