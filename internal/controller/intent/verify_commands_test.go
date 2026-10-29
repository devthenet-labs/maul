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

// TestVerifyCommandOnTheIssue: an approver's /maul verify records a request
// with their account and the comment, once, and is answered once; the note
// below the command line is no argument.
func TestVerifyCommandOnTheIssue(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	id := e.gh.comment(approver, "/maul verify\nthe render looked slow to me")
	e.settleActions(name)
	v := e.verification(name)
	if r := v.Requested; r == nil || r.By != approver || r.ByID != actorOf(approver).ID || r.CommentID != id ||
		r.Fix || r.Reset {
		t.Fatalf("requested = %+v", v.Requested)
	}
	if v.AutoFix.State != v1alpha1.AutoFixHalted || v.Reset.Generation != 0 {
		t.Fatalf("a plain verify changed auto-fix %+v or reset %+v", v.AutoFix, v.Reset)
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

// TestVerifyFixRearms: /maul verify fix re-arms automatic fixes at the
// current round, and a request made while another waits joins it, keeping
// both's modifiers under the newest comment.
func TestVerifyFixRearms(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
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

// TestVerifyReset: --reset bumps the reset generation with the account that
// asked; another within five minutes changes nothing and says so; one after
// them bumps it again.
func TestVerifyReset(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
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

// TestVerifyCommandAuthority: only an approver with write access who is not
// a bot, in an unedited comment, while the intent is in review, records
// anything; everyone else gets the refusal the other intent commands get.
func TestVerifyCommandAuthority(t *testing.T) {
	tests := []struct {
		name  string
		actor string
		perm  string
		body  string
		edit  bool
		want  string
	}{
		{name: "not an approver", actor: "mallory", body: "/maul verify --reset", want: "only the project"},
		{name: "read access only", actor: approver, perm: ghclient.PermissionRead, body: "/maul verify fix",
			want: "only the project"},
		{name: "a bot", actor: "renovate[bot]", body: "/maul accept " + manualID, want: "bot account"},
		{name: "edited", actor: approver, body: "/maul verify --reset", edit: true, want: "was edited"},
		{name: "an unknown argument", actor: approver, body: "/maul verify everything", want: "not understood"},
		{name: "reset as a verb", actor: approver, body: "/maul reset", want: "/maul verify [fix|stop|--reset]"},
		{name: "two arguments", actor: approver, body: "/maul verify fix --reset", want: "not understood"},
		{name: "stop from a non-approver", actor: "mallory", body: "/maul verify stop", want: "only the project"},
		{name: "stop edited", actor: approver, body: "/maul verify stop", edit: true, want: "was edited"},
		{name: "stop as a verb", actor: approver, body: "/maul stop", want: "/maul verify [fix|stop|--reset]"},
		{name: "accept without an id", actor: approver, body: "/maul accept", want: "not understood"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newAcceptEnv(t, func(p *v1alpha1.Project) {
				p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, "renovate[bot]")
			})
			name := e.reviewingWithCriteria()
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

// TestVerifyCommandOutsideReview: before the pull requests are open the
// acceptance commands do nothing and say which commands do.
func TestVerifyCommandOutsideReview(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.awaiting()
	verify := e.gh.comment(approver, "/maul verify")
	accept := e.gh.comment(approver, "/maul accept "+manualID)
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

// TestAcceptCommand: an approver accepts a manual criterion or a quarantined
// one, bound to the commits verified; a criterion checked automatically,
// an id the plan does not have, and an acceptance before any commits are
// recorded are each answered and change nothing.
func TestAcceptCommand(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	e.acceptReady(name)
	digest, heads := e.headSet(name)
	manual := e.gh.comment(approver, "/maul accept "+manualID+"\nchecked it on my phone")
	quarantined := e.gh.comment(approver, "/maul accept "+quarantineID)
	automatic := e.gh.comment(approver, "/maul accept "+automaticID)
	unknown := e.gh.comment(approver, "/maul accept no-such-thing")
	e.settleActions(name)
	acc := e.get(name).Status.Acceptance
	if len(acc.Accepted) != 2 {
		t.Fatalf("accepted = %+v, want the manual and the quarantined criterion", acc.Accepted)
	}
	who := actorOf(approver)
	for i, want := range []struct {
		id      string
		comment int64
	}{{manualID, manual}, {quarantineID, quarantined}} {
		r := acc.Accepted[i]
		if r.ID != want.id || r.By != approver || r.ByID != who.ID || r.CommentID != want.comment ||
			r.HeadSetDigest != digest || !slices.Equal(r.Heads, heads) || r.At.IsZero() {
			t.Errorf("accepted[%d] = %+v", i, r)
		}
	}
	checkAcceptNotes(t, acc.Accepted, "checked it on my phone", "")
	if body := e.onlyReply(manual); !strings.Contains(body, "accepted at `"+heads[0]+"`") {
		t.Errorf("reply = %s", body)
	}
	if body := e.onlyReply(automatic); !strings.Contains(body, "checked automatically") ||
		!strings.Contains(body, "`"+manualID+"`") || strings.Contains(body, "`"+automaticID+"`,") {
		t.Errorf("reply = %s", body)
	}
	if body := e.onlyReply(unknown); !strings.Contains(body, "names no criterion") {
		t.Errorf("reply = %s", body)
	}

	// Accepting again replaces the record: one per criterion, the latest.
	again := e.gh.comment(approver, "/maul accept "+manualID)
	e.settleActions(name)
	acc = e.get(name).Status.Acceptance
	if len(acc.Accepted) != 2 || acc.Accepted[1].ID != manualID || acc.Accepted[1].CommentID != again {
		t.Fatalf("accepted after a re-accept = %+v", acc.Accepted)
	}

	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Verification = nil })
	early := e.gh.comment(approver, "/maul accept "+manualID)
	e.settleActions(name)
	if got := e.get(name).Status.Acceptance.Accepted; len(got) != 2 {
		t.Fatalf("an acceptance with no commits recorded was recorded: %+v", got)
	}
	if body := e.onlyReply(early); !strings.Contains(body, "must wait") {
		t.Errorf("reply = %s", body)
	}
}

// checkAcceptNotes: the note below each command is kept with its record,
// in order (as text, never the id); a command with none records none.
func checkAcceptNotes(t *testing.T, recs []v1alpha1.AcceptanceRecord, notes ...string) {
	t.Helper()
	if len(recs) != len(notes) {
		t.Fatalf("records = %d, want %d", len(recs), len(notes))
	}
	for i, want := range notes {
		if recs[i].Note != want {
			t.Errorf("accepted[%d].Note = %q, want %q", i, recs[i].Note, want)
		}
	}
}

// TestAcceptCountsTheManualCriterion: an accepted manual criterion stops
// counting as open at once, so maul/acceptance (which reads the counts) no
// longer names it, without waiting for another verification.
func TestAcceptCountsTheManualCriterion(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	e.acceptReady(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		countManual(in.Status.Verification, in.Status.Acceptance)
	})
	if v := e.verification(name); v.ManualOpen != 1 || v.ManualAccepted != 0 {
		t.Fatalf("before: manual open %d accepted %d, want 1 and 0", v.ManualOpen, v.ManualAccepted)
	}
	e.gh.comment(approver, "/maul accept "+manualID)
	e.settleActions(name)
	if v := e.verification(name); v.ManualOpen != 0 || v.ManualAccepted != 1 {
		t.Errorf("after: manual open %d accepted %d, want 0 and 1", v.ManualOpen, v.ManualAccepted)
	}
}

// TestVerifyCommandReplyAfterCrash: a request whose status write landed but
// whose reply did not is answered on the next pass, once, and applied once.
func TestVerifyCommandReplyAfterCrash(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
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

// TestVerifyCommandsOnThePullRequest: the acceptance commands on an intent's
// pull request settle exactly as on the issue, authorised against the
// application repository and answered there once; a non-approver's comment
// gets nothing, as on the pull request every non-approver's does; revise
// and retry are left to their own round.
func TestVerifyCommandsOnThePullRequest(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	e.acceptReady(name)
	in := e.get(name)
	pr := in.Status.PullRequests[0]
	stranger := e.prCommand(pr.Number, "mallory", "/maul verify --reset")
	verify := e.prCommand(pr.Number, approver, "/maul verify fix")
	accept := e.prCommand(pr.Number, approver, "/maul accept "+manualID)
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
	if got := in.Status.Acceptance.Accepted; len(got) != 1 || got[0].ID != manualID || got[0].CommentID != accept {
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

// TestVerifyCommandsOnThePullRequestNeedWriteAccess: an approver without
// write access to the application repository is refused there once.
func TestVerifyCommandsOnThePullRequestNeedWriteAccess(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
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

// TestVerifyCommandsNeedTheSwitch: with --intent-verification off the
// acceptance commands on the intent issue write nothing (a reset would
// restart the Preview's every part) and are answered as not available,
// naming only the commands that are.
func TestVerifyCommandsNeedTheSwitch(t *testing.T) {
	e := newAcceptEnv(t)
	e.intent.Settings.Verification = false
	name := e.reviewing()
	reset := e.gh.comment(approver, "/maul verify --reset")
	accept := e.gh.comment(approver, "/maul accept "+manualID)
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

// TestVerifyCommandAppliedOnce: a command whose effect was written but whose
// reply was not is never applied again once the loop consumed its request:
// the retry only answers it.
func TestVerifyCommandAppliedOnce(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	id := e.gh.comment(approver, "/maul verify fix")
	e.gh.failNext("CreateIssueComment", errTransient)
	if err := e.reconcileIntent(name); err == nil {
		t.Fatal("the failed reply returned no error")
	}
	if v := e.verification(name); v.Requested == nil || v.AutoFix.State != v1alpha1.AutoFixArmed {
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

// TestVerifyCommandAtTheCap: an approver's /maul verify once the approved
// plan revision has used the Project's maxVerifications records nothing and
// says so, naming the limit and how to raise it, rather than answering
// "recorded" for a request the loop then drops unrun.
func TestVerifyCommandAtTheCap(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
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

// TestPRVerifyCommandsNeverReplay: deleting maul's replies on the pull
// request never makes an approver's old /maul verify fix re-arm automatic
// fixes a human round halted since, nor a superseded /maul accept replace
// the newer acceptance; each is only answered again.
func TestPRVerifyCommandsNeverReplay(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	e.acceptReady(name)
	n := e.prNumber(name)
	fix := e.prCommand(n, approver, "/maul verify fix")
	first := e.prCommand(n, approver, "/maul accept "+manualID)
	e.clock.Advance(time.Second)
	second := e.prCommand(n, approver, "/maul accept "+manualID)
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

// TestAcceptBindsToWhatTheApproverSaw: an acceptance holds only at commits
// the Preview was live at when it was given: one given before the Preview
// went live at the recorded commits, or while it does not run them, is
// refused and recorded nowhere.
func TestAcceptBindsToWhatTheApproverSaw(t *testing.T) {
	moved := "fedcba9876543210fedcba9876543210fedcba98"
	moveHeads := func(e *env, name string) {
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.PullRequests[0].HeadSHA = moved
		})
		hs, _ := previewHeadSet(e.getProject(), e.get(name))
		e.setStatus(name, func(in *v1alpha1.Intent) {
			in.Status.Verification.HeadSetDigest, in.Status.Verification.Heads = hs.digest, hs.heads
		})
		e.previewOf(name)
	}
	for _, tc := range []struct {
		name string
		run  func(e *env, name string) int64
	}{
		{"before the preview was live", func(e *env, name string) int64 {
			id := e.prCommand(e.prNumber(name), approver, "/maul accept "+manualID)
			e.clock.Advance(time.Minute)
			e.readyPreview(name)
			return id
		}},
		{"the commits moved after it", func(e *env, name string) int64 {
			e.acceptReady(name)
			id := e.prCommand(e.prNumber(name), approver, "/maul accept "+manualID)
			e.clock.Advance(time.Minute)
			moveHeads(e, name)
			e.readyPreview(name)
			return id
		}},
		{"the preview does not run the commits", func(e *env, name string) int64 {
			e.acceptReady(name)
			moveHeads(e, name)
			e.servePreview(name, v1alpha1.PreviewDeploying, "")
			e.clock.Advance(time.Minute)
			return e.prCommand(e.prNumber(name), approver, "/maul accept "+manualID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAcceptEnv(t)
			name := e.reviewingWithCriteria()
			id := tc.run(e, name)
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

// TestPRCommentsListedOncePerPass: one pass lists a pull request's comments
// once, however many of its steps read them (the acceptance commands, the
// command round and the human-pending check).
func TestPRCommentsListedOncePerPass(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
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

// TestAcceptAll: /maul accept all accepts a plan that declares no acceptance
// criteria at the current commits, and nothing else: on a plan with
// criteria it names no criterion and records nothing.
func TestAcceptAll(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	e.acceptReady(name)
	refused := e.gh.comment(approver, "/maul accept all")
	e.settleActions(name)
	if got := e.get(name).Status.Acceptance.Accepted; len(got) != 0 {
		t.Fatalf("accept all on a plan with criteria recorded %+v", got)
	}
	if body := e.onlyReply(refused); !strings.Contains(body, "names no criterion") {
		t.Errorf("reply = %s", body)
	}

	e.setStatus(name, func(in *v1alpha1.Intent) { in.Status.Acceptance.Criteria = nil })
	all := e.gh.comment(approver, "/maul accept all")
	e.settleActions(name)
	got := e.get(name).Status.Acceptance.Accepted
	digest, _ := e.headSet(name)
	if len(got) != 1 || got[0].ID != acceptAll || got[0].HeadSetDigest != digest || got[0].CommentID != all {
		t.Fatalf("accepted = %+v", got)
	}
	if body := e.onlyReply(all); !strings.Contains(body, "declares no acceptance criteria") {
		t.Errorf("reply = %s", body)
	}
}

// TestAcceptCommandCriterion: a command criterion, which maul does not run
// in this slice, is closed by an approver's acceptance like a manual one,
// so a plan carrying one can still reach a green maul/acceptance.
func TestAcceptCommandCriterion(t *testing.T) {
	e := newAcceptEnv(t)
	name := e.reviewingWithCriteria()
	e.acceptReady(name)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance.Criteria = append(in.Status.Acceptance.Criteria, v1alpha1.AcceptanceCriterion{
			ID: "kick-onsets", Kind: v1alpha1.AcceptanceCommand, Statement: "kicks land on the beat", Target: "app"})
	})
	id := e.gh.comment(approver, "/maul accept kick-onsets")
	e.settleActions(name)
	got := e.get(name).Status.Acceptance.Accepted
	if len(got) != 1 || got[0].ID != "kick-onsets" || got[0].CommentID != id {
		t.Fatalf("accepted = %+v", got)
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
			HeadSetDigest: hs.digest, Heads: hs.heads,
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
