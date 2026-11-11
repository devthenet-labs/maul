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

// The outcome twins of verify_commands_test.go (S4 PR 1): the acceptance
// commands on an intent whose approved plan states outcomes, under the
// agent evaluator. Each test is named after the criteria test it twins.

// manualOutcomeID is outcomesPlan's manual outcome, the one an approver
// accepts by hand.
const manualOutcomeID = "looks-right"

// outcomeReviewing is an env over outcomesPlan with the evaluator on and a
// new intent in review whose approved outcomes the loop recorded; its
// Preview exists but is not live yet (acceptReady). It returns the env and
// the intent's name.
func outcomeReviewing(t *testing.T, mutate ...func(*v1alpha1.Project)) (*env, string) {
	t.Helper()
	e := newVerifyEnv(t, outcomesPlan, mutate...)
	e.evaluatorOn(0.4)
	name := e.reviewing()
	e.previewOf(name)
	e.settleActions(name)
	acc := e.get(name).Status.Acceptance
	if acc == nil || len(acc.Outcomes) != 3 || acc.OutcomesDigest == "" {
		t.Fatalf("acceptance = %+v, want the approved plan's three outcomes and no criteria", acc)
	}
	return e, name
}

// TestOutcomeVerifyCommandOnTheIssue twins TestVerifyCommandOnTheIssue.
func TestOutcomeVerifyCommandOnTheIssue(t *testing.T) {
	e, name := outcomeReviewing(t)
	before := e.verification(name).AutoFix
	id := e.gh.comment(approver, "/maul verify\nthe render looked slow to me")
	e.settleActions(name)
	v := e.verification(name)
	if r := v.Requested; r == nil || r.By != approver || r.ByID != actorOf(approver).ID || r.CommentID != id ||
		r.Fix || r.Reset {
		t.Fatalf("requested = %+v", v.Requested)
	}
	if v.AutoFix.State != before.State || v.AutoFix.Rearm != nil || v.Reset.Generation != 0 {
		t.Fatalf("a plain verify changed auto-fix %+v (was %+v) or reset %+v", v.AutoFix, before, v.Reset)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "is recorded") {
		t.Errorf("reply = %s", body)
	}
	if e.gh.reactions[id] == 0 {
		t.Error("the command got no reaction")
	}
	e.settleActions(name)
	if n := len(e.gh.withMarker("comment-" + itoa(id))); n != 1 {
		t.Errorf("the request was answered %d times", n)
	}
}

// TestOutcomeVerifyFixRearms twins TestVerifyFixRearms, from halted.
func TestOutcomeVerifyFixRearms(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted,
			Reason: v1alpha1.AutoFixHumanReview}
	})
	e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	fix := e.gh.comment(approver, "/maul verify fix")
	e.settleActions(name)
	in := e.get(name)
	v := in.Status.Verification
	if r := v.Requested; r == nil || r.CommentID != fix || !r.Fix {
		t.Fatalf("requested after fix = %+v", v.Requested)
	}
	if a := v.AutoFix; a.State != v1alpha1.AutoFixArmed || a.Reason != "" || a.Rearm == nil ||
		a.Rearm.ByID != actorOf(approver).ID || a.Rearm.Round != in.Status.Rounds {
		t.Fatalf("auto-fix after fix = %+v", v.AutoFix)
	}
	if body := e.onlyReply(fix); !strings.Contains(body, "joins a verification already waiting") ||
		!strings.Contains(body, "`/maul verify fix`") {
		t.Errorf("reply = %s", body)
	}
	plain := e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	if r := e.verification(name).Requested; r == nil || r.CommentID != plain || !r.Fix {
		t.Fatalf("a later plain request dropped the waiting fix: %+v", r)
	}
}

// TestOutcomeVerifyReset twins TestVerifyReset.
func TestOutcomeVerifyReset(t *testing.T) {
	e, name := outcomeReviewing(t)
	first := e.gh.comment(approver, "/maul verify --reset")
	e.settleActions(name)
	v := e.verification(name)
	if v.Reset.Generation != 1 || v.Reset.ByID != actorOf(approver).ID || v.Reset.At == nil ||
		v.Requested == nil || !v.Requested.Reset || v.Requested.CommentID != first {
		t.Fatalf("after a reset: reset %+v requested %+v", v.Reset, v.Requested)
	}
	if body := e.onlyReply(first); !strings.Contains(body, "restarts with fresh data") {
		t.Errorf("reply = %s", body)
	}
	if p := e.previewOf(name); p.Spec.ResetGeneration != 1 {
		t.Errorf("preview reset generation = %d, want the reset carried to the Preview", p.Spec.ResetGeneration)
	}
	soon := e.gh.comment(approver, "/maul verify --reset")
	e.settleActions(name)
	if v := e.verification(name); v.Reset.Generation != 1 || v.Requested.CommentID != first {
		t.Fatalf("a reset within five minutes applied: reset %+v requested %+v", v.Reset, v.Requested)
	}
	if body := e.onlyReply(soon); !strings.Contains(body, "less than 5 minutes") {
		t.Errorf("reply = %s", body)
	}
	e.clock.Advance(5 * time.Minute)
	later := e.gh.comment(approver, "/maul verify --reset")
	e.settleActions(name)
	if v := e.verification(name); v.Reset.Generation != 2 || v.Requested.CommentID != later {
		t.Fatalf("a later reset: reset %+v requested %+v", v.Reset, v.Requested)
	}
}

// TestOutcomeVerifyCommandAuthority twins TestVerifyCommandAuthority.
func TestOutcomeVerifyCommandAuthority(t *testing.T) {
	type command struct {
		actor, perm, body string
		edit              bool
		want              string
	}
	const usage = "/maul verify [fix|stop|--reset]"
	tests := map[string]command{
		"not an approver":          {"mallory", "", "/maul verify --reset", false, "only the project"},
		"read access only":         {approver, ghclient.PermissionRead, "/maul verify fix", false, "only the project"},
		"a bot":                    {"renovate[bot]", "", "/maul accept " + manualOutcomeID, false, "bot account"},
		"edited":                   {approver, "", "/maul verify --reset", true, "was edited"},
		"an unknown argument":      {approver, "", "/maul verify everything", false, "not understood"},
		"reset as a verb":          {approver, "", "/maul reset", false, usage},
		"two arguments":            {approver, "", "/maul verify fix --reset", false, "not understood"},
		"stop from a non-approver": {"mallory", "", "/maul verify stop", false, "only the project"},
		"stop edited":              {approver, "", "/maul verify stop", true, "was edited"},
		"stop as a verb":           {approver, "", "/maul stop", false, usage},
		"accept without an id":     {approver, "", "/maul accept", false, "not understood"},
	}
	for what, tt := range tests {
		t.Run(what, func(t *testing.T) {
			e, name := outcomeReviewing(t, func(p *v1alpha1.Project) {
				p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, "renovate[bot]")
			})
			before := e.get(name).Status
			if tt.perm != "" {
				e.gh.perms[approver] = tt.perm
			}
			if strings.HasSuffix(tt.actor, "[bot]") {
				e.gh.perms[tt.actor] = ghclient.PermissionWrite
			}
			var id int64
			if tt.edit {
				id = e.gh.comment(tt.actor, "ok, looking")
				e.gh.editWithinTheSecond(id, tt.body)
			} else {
				id = e.gh.comment(tt.actor, tt.body)
			}
			e.settleActions(name)
			after := e.get(name).Status
			if fmt.Sprint(after.Verification) != fmt.Sprint(before.Verification) ||
				fmt.Sprint(after.Acceptance) != fmt.Sprint(before.Acceptance) {
				t.Fatalf("the command changed the intent:\nverification %+v\nacceptance %+v",
					after.Verification, after.Acceptance)
			}
			if body := e.onlyReply(id); !strings.Contains(body, tt.want) {
				t.Errorf("reply = %s, want it to say %q", body, tt.want)
			}
		})
	}
}

// TestOutcomeVerifyCommandOutsideReview twins TestVerifyCommandOutsideReview.
func TestOutcomeVerifyCommandOutsideReview(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan)
	e.evaluatorOn(0.4)
	name := e.awaiting()
	verify := e.gh.comment(approver, "/maul verify")
	accept := e.gh.comment(approver, "/maul accept "+manualOutcomeID)
	e.settleActions(name)
	in := e.get(name)
	if in.Status.Verification != nil || in.Status.Acceptance != nil {
		t.Fatalf("verification %+v acceptance %+v, want none", in.Status.Verification, in.Status.Acceptance)
	}
	for _, id := range []int64{verify, accept} {
		if body := e.onlyReply(id); !strings.Contains(body, "does nothing while this intent is `AwaitingApproval`") ||
			!strings.Contains(body, "/maul approve") {
			t.Errorf("reply = %s", body)
		}
	}
}

// TestOutcomeVerifyCommandReplyAfterCrash twins
// TestVerifyCommandReplyAfterCrash.
func TestOutcomeVerifyCommandReplyAfterCrash(t *testing.T) {
	e, name := outcomeReviewing(t)
	id := e.gh.comment(approver, "/maul verify --reset")
	e.gh.failNext("CreateIssueComment", errTransient)
	if err := e.reconcileIntent(name); err == nil {
		t.Fatal("the failed reply returned no error")
	}
	if v := e.verification(name); v.Reset.Generation != 1 {
		t.Fatalf("reset = %+v, want applied before the reply", v.Reset)
	}
	e.clock.Advance(time.Minute)
	e.settleActions(name)
	if v := e.verification(name); v.Reset.Generation != 1 || v.Requested == nil || v.Requested.CommentID != id {
		t.Fatalf("after the retry: reset %+v requested %+v", v.Reset, v.Requested)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "restarts with fresh data") {
		t.Errorf("reply = %s", body)
	}
}

// TestOutcomeVerifyCommandsOnThePullRequest twins
// TestVerifyCommandsOnThePullRequest, from halted.
func TestOutcomeVerifyCommandsOnThePullRequest(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.AutoFix = v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted,
			Reason: v1alpha1.AutoFixHumanReview}
	})
	e.acceptReady(name)
	in := e.get(name)
	pr := in.Status.PullRequests[0]
	stranger := e.prCommand(pr.Number, "mallory", "/maul verify --reset")
	verify := e.prCommand(pr.Number, approver, "/maul verify fix")
	accept := e.prCommand(pr.Number, approver, "/maul accept "+manualOutcomeID)
	bad := e.prCommand(pr.Number, approver, "/maul verify now")
	revise := e.prCommand(pr.Number, approver, "/maul revise rename the handler")
	e.clock.Advance(time.Second)
	for range 2 {
		p := e.prVerifyPass(name)
		if err := p.settlePRVerifyCommands(context.Background(), &pr); err != nil {
			t.Fatal(err)
		}
	}
	in = e.get(name)
	v := in.Status.Verification
	if r := v.Requested; r == nil || r.CommentID != verify || !r.Fix || r.ByID != actorOf(approver).ID {
		t.Fatalf("requested = %+v", v.Requested)
	}
	if v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Errorf("auto-fix = %+v, want re-armed", v.AutoFix)
	}
	if v.Reset.Generation != 0 {
		t.Errorf("a non-approver's reset applied: %+v", v.Reset)
	}
	if got := in.Status.Acceptance.Accepted; len(got) != 1 || got[0].ID != manualOutcomeID ||
		got[0].CommentID != accept {
		t.Errorf("accepted = %+v", got)
	}
	for id, want := range map[int64]string{verify: "is recorded", accept: "is done", bad: "not understood"} {
		if r := e.prReplies(pr.Number, id); len(r) != 1 || !strings.Contains(r[0], want) {
			t.Errorf("replies to %d = %q, want one saying %q", id, r, want)
		}
	}
	for _, id := range []int64{stranger, revise} {
		if r := e.prReplies(pr.Number, id); len(r) != 0 {
			t.Errorf("comment %d was answered: %q", id, r)
		}
	}
	if e.gh.reactions[stranger] != 0 {
		t.Error("a non-approver's command on the pull request got a reaction")
	}
}

// TestOutcomeVerifyCommandsOnThePullRequestNeedWriteAccess twins
// TestVerifyCommandsOnThePullRequestNeedWriteAccess.
func TestOutcomeVerifyCommandsOnThePullRequestNeedWriteAccess(t *testing.T) {
	e, name := outcomeReviewing(t)
	pr := e.get(name).Status.PullRequests[0]
	e.gh.perms[approver] = ghclient.PermissionRead
	id := e.prCommand(pr.Number, approver, "/maul verify --reset")
	e.clock.Advance(time.Second)
	for range 2 {
		if err := e.prVerifyPass(name).settlePRVerifyCommands(context.Background(), &pr); err != nil {
			t.Fatal(err)
		}
	}
	if v := e.verification(name); v.Requested != nil || v.Reset.Generation != 0 {
		t.Fatalf("refused command applied: %+v", v)
	}
	if r := e.prReplies(pr.Number, id); len(r) != 1 || !strings.Contains(r[0], "only the project") {
		t.Errorf("replies = %q", r)
	}
}

// TestOutcomeVerifyCommandsNeedTheSwitch twins TestVerifyCommandsNeedTheSwitch:
// an approved outcomes plan with the controller's verification (and so the
// evaluator) off.
func TestOutcomeVerifyCommandsNeedTheSwitch(t *testing.T) {
	e := newVerifyEnv(t, outcomesPlan)
	e.intent.Settings.Verification = false
	name := e.reviewing()
	reset := e.gh.comment(approver, "/maul verify --reset")
	accept := e.gh.comment(approver, "/maul accept "+manualOutcomeID)
	e.settleActions(name)
	if v := e.get(name).Status.Verification; v != nil {
		t.Fatalf("verification = %+v, want none written", v)
	}
	for _, id := range []int64{reset, accept} {
		body := e.onlyReply(id)
		if !strings.Contains(body, "does nothing while this intent is `InReview`") ||
			strings.Contains(body, "- `/maul verify") || strings.Contains(body, "- `/maul accept") ||
			!strings.Contains(body, "- `/maul cancel`") {
			t.Errorf("reply = %s", body)
		}
	}
}

// TestOutcomeVerifyCommandAppliedOnce twins TestVerifyCommandAppliedOnce.
func TestOutcomeVerifyCommandAppliedOnce(t *testing.T) {
	e, name := outcomeReviewing(t)
	id := e.gh.comment(approver, "/maul verify fix")
	e.gh.failNext("CreateIssueComment", errTransient)
	if err := e.reconcileIntent(name); err == nil {
		t.Fatal("the failed reply returned no error")
	}
	if v := e.verification(name); v.Requested == nil || v.AutoFix.State != v1alpha1.AutoFixArmed ||
		v.AutoFix.Rearm == nil {
		t.Fatalf("verification = %+v, want the request applied before the reply", v)
	}
	// The loop consumed the request, and a human's round halted fixes since.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = nil
		in.Status.Verification.AutoFix.State = v1alpha1.AutoFixHalted
		in.Status.Verification.AutoFix.Reason = v1alpha1.AutoFixHumanReview
	})
	e.clock.Advance(time.Minute)
	e.settleActions(name)
	if v := e.verification(name); v.Requested != nil || v.AutoFix.State != v1alpha1.AutoFixHalted {
		t.Fatalf("the command applied again: requested %+v auto-fix %+v", v.Requested, v.AutoFix)
	}
	if body := e.onlyReply(id); !strings.Contains(body, "applied already") {
		t.Errorf("reply = %s", body)
	}
}

// TestOutcomeVerifyCommandAtTheCap twins TestVerifyCommandAtTheCap.
func TestOutcomeVerifyCommandAtTheCap(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Automatic = v1alpha1.DefaultMaxVerifications
	})
	id := e.gh.comment(approver, "/maul verify")
	e.settleActions(name)
	if r := e.verification(name).Requested; r != nil {
		t.Fatalf("a request past the cap was recorded: %+v", r)
	}
	body := e.onlyReply(id)
	if strings.Contains(body, "is recorded") || !strings.Contains(body, "spec.limits.maxVerifications") ||
		!strings.Contains(body, fmt.Sprintf("all %d", v1alpha1.DefaultMaxVerifications)) {
		t.Errorf("reply = %s", body)
	}
}

// TestOutcomePRVerifyCommandsNeverReplay twins TestPRVerifyCommandsNeverReplay.
func TestOutcomePRVerifyCommandsNeverReplay(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.acceptReady(name)
	n := e.prNumber(name)
	fix := e.prCommand(n, approver, "/maul verify fix")
	first := e.prCommand(n, approver, "/maul accept "+manualOutcomeID)
	e.clock.Advance(time.Second)
	second := e.prCommand(n, approver, "/maul accept "+manualOutcomeID)
	e.clock.Advance(time.Second)
	e.settlePR(name)
	if v := e.verification(name); v.Requested == nil || v.AutoFix.State != v1alpha1.AutoFixArmed {
		t.Fatalf("verification = %+v, want the fix applied", v)
	}
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.Requested = nil
		in.Status.Verification.AutoFix.State = v1alpha1.AutoFixHalted
		in.Status.Verification.AutoFix.Reason = v1alpha1.AutoFixHumanReview
	})
	e.dropReplies(n)
	e.clock.Advance(time.Minute)
	e.settlePR(name)
	if v := e.verification(name); v.Requested != nil || v.AutoFix.State != v1alpha1.AutoFixHalted {
		t.Fatalf("the old fix replayed: requested %+v auto-fix %+v", v.Requested, v.AutoFix)
	}
	if acc := e.get(name).Status.Acceptance.Accepted; len(acc) != 1 || acc[0].CommentID != second {
		t.Fatalf("accepted = %+v, want the newer acceptance only", acc)
	}
	for _, id := range []int64{fix, first} {
		if r := e.prReplies(n, id); len(r) != 1 || !strings.Contains(r[0], "applied already") {
			t.Errorf("replies to %d = %q", id, r)
		}
	}
}

// movedHeads moves intent name's pull request, and the head set its
// verification records, to another commit the Preview does not run.
func (e *env) movedHeads(name string) {
	e.t.Helper()
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.PullRequests[0].HeadSHA = "fedcba9876543210fedcba9876543210fedcba98"
	})
	hs, _ := previewHeadSet(e.getProject(), e.get(name))
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.HeadSetDigest, in.Status.Verification.Heads = hs.digest, hs.heads
	})
	e.previewOf(name)
}

// acceptManualOnPR comments /maul accept of the manual outcome on intent
// name's pull request now, and returns the comment's id.
func (e *env) acceptManualOnPR(name string) int64 {
	return e.prCommand(e.prNumber(name), approver, "/maul accept "+manualOutcomeID)
}

// TestOutcomeAcceptBindsToWhatTheApproverSaw twins
// TestAcceptBindsToWhatTheApproverSaw on the manual outcome.
func TestOutcomeAcceptBindsToWhatTheApproverSaw(t *testing.T) {
	cases := map[string]func(e *env, name string) int64{
		"before the preview was live": func(e *env, name string) int64 {
			id := e.acceptManualOnPR(name)
			e.clock.Advance(time.Minute)
			e.readyPreview(name)
			return id
		},
		"the commits moved after it": func(e *env, name string) int64 {
			e.acceptReady(name)
			id := e.acceptManualOnPR(name)
			e.clock.Advance(time.Minute)
			e.movedHeads(name)
			e.readyPreview(name)
			return id
		},
		"the preview does not run the commits": func(e *env, name string) int64 {
			e.acceptReady(name)
			e.movedHeads(name)
			e.servePreview(name, v1alpha1.PreviewDeploying, "")
			e.clock.Advance(time.Minute)
			return e.acceptManualOnPR(name)
		},
	}
	for what, run := range cases {
		t.Run(what, func(t *testing.T) {
			e, name := outcomeReviewing(t)
			id := run(e, name)
			e.clock.Advance(time.Second)
			e.settlePR(name)
			if acc := e.get(name).Status.Acceptance.Accepted; len(acc) != 0 {
				t.Fatalf("accepted = %+v, want nothing", acc)
			}
			if r := e.prReplies(e.prNumber(name), id); len(r) != 1 || !strings.Contains(r[0], "accept again") {
				t.Errorf("replies = %q", r)
			}
		})
	}
}

// TestOutcomePRCommentsListedOncePerPass twins TestPRCommentsListedOncePerPass.
func TestOutcomePRCommentsListedOncePerPass(t *testing.T) {
	e, name := outcomeReviewing(t)
	n := e.prNumber(name)
	e.prCommand(n, approver, "/maul verify")
	e.clock.Advance(time.Second)
	p := e.prVerifyPass(name)
	pr := p.in.Status.PullRequests[0]
	ctx := context.Background()
	e.gh.mu.Lock()
	before := e.gh.calls["ListPullRequestComments"]
	e.gh.mu.Unlock()
	if err := p.settlePRVerifyCommands(ctx, &pr); err != nil {
		t.Fatal(err)
	}
	if _, err := p.commandRound(ctx, &pr); err != nil {
		t.Fatal(err)
	}
	if _, err := p.humanPending(ctx, p.in.Status.PullRequests); err != nil {
		t.Fatal(err)
	}
	e.gh.mu.Lock()
	got := e.gh.calls["ListPullRequestComments"] - before
	e.gh.mu.Unlock()
	if got != 1 {
		t.Errorf("listed the pull request's comments %d times in one pass, want once", got)
	}
}

// TestOutcomeAcceptOnlyTheManualOutcome is TestAcceptCommand's outcome
// half on a plan the loop recorded (TestAcceptOutcome covers the rest on a
// synthesised record): the manual outcome is accepted at the commits the
// preview ran, an evaluated outcome not yet decided is not, and accepting
// again replaces the record.
func TestOutcomeAcceptOnlyTheManualOutcome(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.acceptReady(name)
	digest, heads := e.headSet(name)
	manual := e.gh.comment(approver, "/maul accept "+manualOutcomeID+"\nchecked it on my phone")
	evaluated := e.gh.comment(approver, "/maul accept version-shown")
	e.settleActions(name)
	acc := e.get(name).Status.Acceptance
	if len(acc.Accepted) != 1 {
		t.Fatalf("accepted = %+v, want the manual outcome alone", acc.Accepted)
	}
	r := acc.Accepted[0]
	if r.ID != manualOutcomeID || r.By != approver || r.CommentID != manual || r.HeadSetDigest != digest ||
		!slices.Equal(r.Heads, heads) || r.Note != "checked it on my phone" {
		t.Errorf("accepted = %+v", r)
	}
	if body := e.onlyReply(evaluated); !strings.Contains(body, "only a manual outcome") {
		t.Errorf("reply = %s", body)
	}
	again := e.gh.comment(approver, "/maul accept "+manualOutcomeID)
	e.settleActions(name)
	if acc := e.get(name).Status.Acceptance.Accepted; len(acc) != 1 || acc[0].CommentID != again {
		t.Fatalf("accepted after a re-accept = %+v", acc)
	}
}

// TestOutcomeAcceptCountsTheManualOutcome twins
// TestAcceptCountsTheManualCriterion: an accepted manual outcome stops
// counting as open at once, without waiting for another round.
func TestOutcomeAcceptCountsTheManualOutcome(t *testing.T) {
	e, name := outcomeReviewing(t)
	e.acceptReady(name)
	if v := e.verification(name); v.ManualOpen != 1 || v.ManualAccepted != 0 {
		t.Fatalf("before: manual open %d accepted %d, want 1 and 0", v.ManualOpen, v.ManualAccepted)
	}
	e.gh.comment(approver, "/maul accept "+manualOutcomeID)
	e.settleActions(name)
	if v := e.verification(name); v.ManualOpen != 0 || v.ManualAccepted != 1 {
		t.Errorf("after: manual open %d accepted %d, want 0 and 1", v.ManualOpen, v.ManualAccepted)
	}
}
