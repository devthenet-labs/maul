// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

const (
	manualID     = "sounds-good"
	automaticID  = "render"
	quarantineID = "leaderboard"
)

// setStatus writes the Intent's status as mutate leaves it, as another of the
// intent reconciler's steps would have.
func (e *env) setStatus(name string, mutate func(*v1alpha1.Intent)) {
	e.t.Helper()
	in := e.get(name)
	mutate(in)
	if err := e.c.Status().Update(context.Background(), in); err != nil {
		e.t.Fatal(err)
	}
}

// newAcceptEnv is an env whose intents are verified: the Project previews
// its app and verifies, and the controller's verification switch is on.
func newAcceptEnv(t *testing.T, mutate ...func(*v1alpha1.Project)) *env {
	t.Helper()
	return newVerifyEnv(t, "", mutate...)
}

// reviewingWithCriteria drives a new intent to InReview and records an
// approved plan's criteria (one manual, two automatic, one of them
// quarantined) and a verification of the current commits, as the loop
// would. The Preview exists but is not live yet (acceptReady).
func (e *env) reviewingWithCriteria() string {
	e.t.Helper()
	name := e.reviewing()
	e.previewOf(name)
	hs, ok := previewHeadSet(e.getProject(), e.get(name))
	if !ok {
		e.t.Fatal("the intent has no preview head set")
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance = &v1alpha1.IntentAcceptance{PlanRevision: in.Status.Approval.PlanRevision,
			Digest: hs.digest, Criteria: []v1alpha1.AcceptanceCriterion{
				{ID: manualID, Kind: v1alpha1.AcceptanceManual, Statement: "the beat sounds good"},
				{ID: automaticID, Kind: v1alpha1.AcceptanceHTTP, Statement: "a render returns audio", Target: "app"},
				{ID: quarantineID, Kind: v1alpha1.AcceptanceHTTP, Statement: "the leaderboard lists votes",
					Target: "app"},
			}}
		in.Status.Verification = &v1alpha1.IntentVerification{State: v1alpha1.VerificationWaitingForPreview,
			HeadSetDigest: hs.digest, QuarantinedIDs: []string{quarantineID}, Heads: hs.heads,
			AutoFix: v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanReview}}
	})
	e.settleActions(name)
	return name
}

// acceptReady makes the Preview live at the recorded commits, a minute
// before the commands that follow are made.
func (e *env) acceptReady(name string) {
	e.t.Helper()
	e.readyPreview(name)
	e.clock.Advance(time.Minute)
}

// headSet is the digest and the short commits verification records.
func (e *env) headSet(name string) (string, []string) {
	e.t.Helper()
	v := e.verification(name)
	return v.HeadSetDigest, shortHeads(v.Heads)
}

// verification is the Intent's verification status, never nil.
func (e *env) verification(name string) *v1alpha1.IntentVerification {
	if v := e.get(name).Status.Verification; v != nil {
		return v
	}
	return &v1alpha1.IntentVerification{}
}

// onlyReply is the one reply to comment id on the intent issue.
func (e *env) onlyReply(id int64) string {
	e.t.Helper()
	r := e.gh.withMarker("comment-" + itoa(id))
	if len(r) != 1 {
		e.t.Fatalf("replies to comment %d = %d, want one: %+v", id, len(r), r)
	}
	return r[0].Body
}

// prVerifyPass is a pass over the intent as it stands, its runs loaded, for
// settling the acceptance commands on its pull request directly.
func (e *env) prVerifyPass(name string) *pass {
	e.t.Helper()
	p := &pass{r: e.intent, set: e.intent.Settings.withDefaults(), in: e.get(name), proj: e.getProject(),
		now: e.clock.Now()}
	if err := p.loadRuns(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// prCommand records login commenting on pull request n now, with an id of
// the fake's own.
func (e *env) prCommand(n int64, login, body string) int64 {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	a := actorOf(login)
	id := e.gh.id()
	e.gh.prComments[n] = append(e.gh.prComments[n], &ghclient.Comment{ID: id, NodeID: fmt.Sprintf("IC_%d", id),
		Body: body, UserLogin: a.Login, UserID: a.ID, UserType: a.Type,
		CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()})
	e.gh.version++
	return id
}

// prReplies are maul's replies to comment id on pull request n.
func (e *env) prReplies(n, id int64) []string {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	var out []string
	for _, c := range e.gh.prComments[n] {
		if c.UserLogin == testBot && strings.Contains(markerOf(c.Body), "comment-"+itoa(id)) {
			out = append(out, c.Body)
		}
	}
	return out
}

// dropReplies deletes maul's replies on pull request n, as anyone with
// write access to its repository can.
func (e *env) dropReplies(n int64) {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	e.gh.prComments[n] = slices.DeleteFunc(e.gh.prComments[n], func(c *ghclient.Comment) bool {
		return c.UserLogin == testBot
	})
	e.gh.version++
}

// settlePR settles the acceptance commands on the intent's pull request in
// one pass.
func (e *env) settlePR(name string) {
	e.t.Helper()
	p := e.prVerifyPass(name)
	pr := p.in.Status.PullRequests[0]
	if err := p.settlePRVerifyCommands(context.Background(), &pr); err != nil {
		e.t.Fatal(err)
	}
}

// TestRememberAppliedKeepsTheLatest: the applied commands are each kept
// once, the latest MaxIntentAppliedCommands of them, as the schema bounds
// them.
func TestRememberAppliedKeepsTheLatest(t *testing.T) {
	in := &v1alpha1.Intent{}
	for id := int64(1); id <= v1alpha1.MaxIntentAppliedCommands+5; id++ {
		rememberApplied(in, id)
		rememberApplied(in, id)
	}
	got := in.Status.Commands.Applied
	if len(got) != v1alpha1.MaxIntentAppliedCommands || got[0] != 6 ||
		got[len(got)-1] != v1alpha1.MaxIntentAppliedCommands+5 {
		t.Errorf("applied = %v", got)
	}
}

// The outcomes of reviewingWithOutcomes, by what the evaluator made of them.
const (
	passedOutcome  = "render"
	failedOutcome  = "kick-onsets"
	unsureOutcome  = "deep-link"
	manualOutcome  = "sounds-good"
	pendingOutcome = "leaderboard"
)

// reviewingWithOutcomes drives a new intent to InReview and records an
// approved plan's outcomes (the evaluator's) and a verification of the
// current commits that decided them: one passed, one failed, one unsure, one
// manual and one not decided yet, the preview link held. The Preview exists
// but is not live yet (acceptReady).
func (e *env) reviewingWithOutcomes() string {
	e.t.Helper()
	name := e.reviewing()
	e.previewOf(name)
	hs, ok := previewHeadSet(e.getProject(), e.get(name))
	if !ok {
		e.t.Fatal("the intent has no preview head set")
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance = &v1alpha1.IntentAcceptance{PlanRevision: in.Status.Approval.PlanRevision,
			Digest: hs.digest, OutcomesDigest: hs.digest, Outcomes: []v1alpha1.AcceptanceOutcome{
				{ID: passedOutcome, Outcome: "a render returns audio"},
				{ID: failedOutcome, Outcome: "kicks land on the beat"},
				{ID: unsureOutcome, Outcome: "a shared link opens the battle"},
				{ID: manualOutcome, Outcome: "the beat sounds good", Manual: true},
				{ID: pendingOutcome, Outcome: "the leaderboard lists votes"},
			}}
		in.Status.Verification = &v1alpha1.IntentVerification{State: v1alpha1.VerificationUnconfirmed,
			HeadSetDigest: hs.digest, Heads: hs.heads, OutcomesHeadSetDigest: hs.digest,
			Outcomes: []v1alpha1.OutcomeResult{
				{ID: passedOutcome, Result: v1alpha1.OutcomePassed},
				{ID: failedOutcome, Result: v1alpha1.OutcomeFailed, Reason: "the kick lands late"},
				{ID: unsureOutcome, Result: v1alpha1.OutcomeUnsure, Reason: "no check confirmed it"},
			},
			Handoff: &v1alpha1.HandoffState{Mode: v1alpha1.HandoffGated},
			AutoFix: v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanReview}}
	})
	e.settleActions(name)
	return name
}

// TestAcceptOutcome: an approver closes an outcome the evaluator found
// failing or could not confirm, or a manual one, at the commits the preview
// ran; one that passed, or is not decided yet, is never closed by hand, and
// an id the plan does not declare names no outcome.
func TestAcceptOutcome(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithOutcomes()
	e.acceptReady(name)
	digest, heads := e.headSet(name)
	failed := e.gh.comment(approver, "/maul accept "+failedOutcome+"\nthe kick is late by design")
	unsure := e.gh.comment(approver, "/maul accept "+unsureOutcome)
	manual := e.gh.comment(approver, "/maul accept "+manualOutcome)
	passed := e.gh.comment(approver, "/maul accept "+passedOutcome)
	pending := e.gh.comment(approver, "/maul accept "+pendingOutcome)
	unknown := e.gh.comment(approver, "/maul accept no-such-thing")
	e.settleActions(name)
	acc := e.get(name).Status.Acceptance
	got := make([]string, 0, len(acc.Accepted))
	for _, r := range acc.Accepted {
		got = append(got, r.ID)
		if r.HeadSetDigest != digest || !slices.Equal(r.Heads, heads) || r.ByID != actorOf(approver).ID {
			t.Errorf("accepted %+v, want it bound to %s", r, digest)
		}
	}
	if want := []string{failedOutcome, unsureOutcome, manualOutcome}; !slices.Equal(got, want) {
		t.Fatalf("accepted = %v, want %v", got, want)
	}
	if body := e.onlyReply(failed); !strings.Contains(body, "the outcome is accepted at `"+heads[0]+"`") {
		t.Errorf("reply = %s", body)
	}
	for _, id := range []int64{passed, pending} {
		body := e.onlyReply(id)
		if !strings.Contains(body, "only a manual outcome") || strings.Contains(body, "`"+passedOutcome+"`,") ||
			!strings.Contains(body, "`"+unsureOutcome+"`") {
			t.Errorf("reply to %d = %s", id, body)
		}
	}
	if body := e.onlyReply(unknown); !strings.Contains(body, "names no outcome") {
		t.Errorf("reply = %s", body)
	}
	for _, id := range []int64{unsure, manual} {
		if body := e.onlyReply(id); !strings.Contains(body, "is done") {
			t.Errorf("reply to %d = %s", id, body)
		}
	}
}

// TestAcceptAllOutcomes: /maul accept all closes every outcome an approver
// could close one by one, each recorded under the one comment, and is
// answered with what it closed, once, even after a crash between the write
// and the reply; with nothing left to close it records nothing.
func TestAcceptAllOutcomes(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithOutcomes()
	e.acceptReady(name)
	all := e.gh.comment(approver, "/maul accept all\nchecked these on my phone")
	e.gh.failNext("CreateIssueComment", errTransient)
	if err := e.reconcileIntent(name); err == nil {
		t.Fatal("the failed reply returned no error")
	}
	e.clock.Advance(time.Minute)
	e.settleActions(name)
	var got []string
	for _, r := range e.get(name).Status.Acceptance.Accepted {
		if r.CommentID != all {
			t.Errorf("accepted %+v, want it recorded under comment %d", r, all)
		}
		got = append(got, r.ID)
	}
	if want := []string{failedOutcome, unsureOutcome, manualOutcome}; !slices.Equal(got, want) {
		t.Fatalf("accepted = %v, want %v", got, want)
	}
	body := e.onlyReply(all)
	for _, id := range []string{failedOutcome, unsureOutcome, manualOutcome} {
		if !strings.Contains(body, "`"+id+"`") {
			t.Errorf("reply does not name %s: %s", id, body)
		}
	}
	if !strings.Contains(body, "`/maul accept all`") {
		t.Errorf("reply = %s", body)
	}

	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Accepted = nil
		in.Status.Acceptance.Outcomes = in.Status.Acceptance.Outcomes[:1]
	})
	none := e.gh.comment(approver, "/maul accept all")
	e.settleActions(name)
	if got := e.get(name).Status.Acceptance.Accepted; len(got) != 0 {
		t.Fatalf("accept all with nothing to close recorded %+v", got)
	}
	if body := e.onlyReply(none); !strings.Contains(body, "No outcome can be accepted now") {
		t.Errorf("reply = %s", body)
	}
}

// TestPreviewCommand: an approver's /maul preview releases the held preview
// link, recording the command as the reason, and changes nothing else; a
// second is told it was released already, and the command applied is
// answered once.
func TestPreviewCommand(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithOutcomes()
	before := e.verification(name).DeepCopy()
	id := e.gh.comment(approver, "/maul preview\nI want to look at it now")
	e.settleActions(name)
	v := e.verification(name)
	h := v.Handoff
	if h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonCommand || h.Mode != v1alpha1.HandoffGated ||
		h.ReleasedAt == nil {
		t.Fatalf("hand-off = %+v", h)
	}
	if v.Requested != nil || v.AutoFix.State != before.AutoFix.State || v.Reset.Generation != 0 ||
		!slices.Equal(v.Outcomes, before.Outcomes) || v.State != before.State {
		t.Errorf("/maul preview changed more than the hand-off: %+v", v)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "the preview link is shown now") {
		t.Errorf("reply = %s", body)
	}
	e.settleActions(name)
	if n := len(e.gh.withMarker("comment-" + itoa(id))); n != 1 {
		t.Errorf("the command was answered %d times", n)
	}
	e.clock.Advance(time.Minute)
	again := e.gh.comment(approver, "/maul preview")
	e.settleActions(name)
	if body := e.onlyReply(again); !strings.Contains(body, "already") {
		t.Errorf("reply = %s", body)
	}
	if got := e.verification(name).Handoff; got.ReleasedAt == nil || !got.ReleasedAt.Equal(h.ReleasedAt) {
		t.Errorf("a second /maul preview moved the release: %+v", got)
	}
	bad := e.gh.comment(approver, "/maul preview now")
	e.settleActions(name)
	if body := e.onlyReply(bad); !strings.Contains(body, "not understood") ||
		!strings.Contains(body, "- `/maul preview`") {
		t.Errorf("reply = %s", body)
	}
}

// TestPreviewCommandNothingHeld: where nothing holds the link (a plan with
// criteria, slice 1's verification, or a Project whose hand-off is early)
// /maul preview records nothing and says so.
func TestPreviewCommandNothingHeld(t *testing.T) {
	tests := []struct {
		name   string
		mutate []func(*v1alpha1.Project)
		setup  func(e *env) string
	}{
		{"criteria", nil, func(e *env) string { return e.reviewingWithCriteria() }},
		{"an early project", []func(*v1alpha1.Project){func(p *v1alpha1.Project) {
			p.Spec.Verification.Handoff = v1alpha1.HandoffEarly
		}}, func(e *env) string {
			name := e.reviewingWithOutcomes()
			e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification.Handoff = nil })
			return name
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAcceptEnv(t, tt.mutate...)
			name := tt.setup(e)
			id := e.gh.comment(approver, "/maul preview")
			e.settleActions(name)
			if h := e.verification(name).Handoff; h != nil {
				t.Errorf("hand-off = %+v, want none recorded", h)
			}
			if body := e.onlyReply(id); !strings.Contains(body, "is not held") {
				t.Errorf("reply = %s", body)
			}
		})
	}
}

// TestPreviewCommandOnThePullRequest: /maul preview on an intent's pull
// request releases the link as on the issue, answered there once.
func TestPreviewCommandOnThePullRequest(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithOutcomes()
	pr := e.get(name).Status.PullRequests[0]
	id := e.prCommand(pr.Number, approver, "/maul preview")
	e.clock.Advance(time.Second)
	e.settlePR(name)
	e.settlePR(name)
	if h := e.verification(name).Handoff; h == nil || !h.Released || h.Reason != v1alpha1.HandoffReasonCommand {
		t.Fatalf("hand-off = %+v", h)
	}
	if r := e.prReplies(pr.Number, id); len(r) != 1 || !strings.Contains(r[0], "the preview link is shown now") {
		t.Errorf("replies = %q", r)
	}
}
