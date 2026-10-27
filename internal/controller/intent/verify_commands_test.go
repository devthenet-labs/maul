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
	testHeadSet  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testHeadSHA  = "0123abc456def7890123abc456def7890123abcd"
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

// reviewingWithCriteria drives a new intent to InReview and records an
// approved plan's criteria (one manual, two automatic, one of them
// quarantined) and a verification of the current commits.
func (e *env) reviewingWithCriteria() string {
	e.t.Helper()
	name := e.awaiting()
	e.gh.label(1, "maul:approved", approver)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Acceptance = &v1alpha1.IntentAcceptance{PlanRevision: in.Status.Approval.PlanRevision,
			Digest: testHeadSet, Criteria: []v1alpha1.AcceptanceCriterion{
				{ID: manualID, Kind: v1alpha1.AcceptanceManual, Statement: "the beat sounds good"},
				{ID: automaticID, Kind: v1alpha1.AcceptanceHTTP, Statement: "a render returns audio", Target: "app"},
				{ID: quarantineID, Kind: v1alpha1.AcceptanceHTTP, Statement: "the leaderboard lists votes",
					Target: "app"},
			}}
		in.Status.Verification = &v1alpha1.IntentVerification{State: v1alpha1.VerificationPassed,
			HeadSetDigest: testHeadSet, QuarantinedIDs: []string{quarantineID},
			Heads:   []v1alpha1.VerificationHead{{Component: "app", Repository: appRepoURL, Revision: testHeadSHA}},
			AutoFix: v1alpha1.AutoFix{State: v1alpha1.AutoFixHalted, Reason: v1alpha1.AutoFixHumanReview}}
	})
	e.clock.Advance(time.Minute)
	return name
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
	e := newEnv(t, testProject())
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
	e := newEnv(t, testProject())
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
	e := newEnv(t, testProject())
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
		{name: "reset as a verb", actor: approver, body: "/maul reset", want: "/maul verify [fix|--reset]"},
		{name: "two arguments", actor: approver, body: "/maul verify fix --reset", want: "not understood"},
		{name: "accept without an id", actor: approver, body: "/maul accept", want: "not understood"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProject()
			p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, "renovate[bot]")
			e := newEnv(t, p)
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
	e := newEnv(t, testProject())
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
	e := newEnv(t, testProject())
	name := e.reviewingWithCriteria()
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
			r.HeadSetDigest != testHeadSet || !slices.Equal(r.Heads, []string{testHeadSHA[:7]}) || r.At.IsZero() {
			t.Errorf("accepted[%d] = %+v", i, r)
		}
	}
	if body := e.onlyReply(manual); !strings.Contains(body, "accepted at `"+testHeadSHA[:7]+"`") {
		t.Errorf("reply = %s", body)
	}
	if body := e.onlyReply(automatic); !strings.Contains(body, "checked automatically") ||
		!strings.Contains(body, "`"+manualID+"`") || strings.Contains(body, "`"+automaticID+"`,") {
		t.Errorf("reply = %s", body)
	}
	if body := e.onlyReply(unknown); !strings.Contains(body, "names no criterion") {
		t.Errorf("reply = %s", body)
	}

	// Accepting again at other commits replaces the record: one per
	// criterion, the latest.
	e.setStatus(name, func(in *v1alpha1.Intent) {
		in.Status.Verification.HeadSetDigest = "sha256:" + strings.Repeat("2", 64)
	})
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

// TestAcceptCountsTheManualCriterion: an accepted manual criterion stops
// counting as open at once, so maul/acceptance (which reads the counts) no
// longer names it, without waiting for another verification.
func TestAcceptCountsTheManualCriterion(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.reviewingWithCriteria()
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
	e := newEnv(t, testProject())
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
	p := &pass{r: e.intent, set: testSettings(), in: e.get(name), proj: testProject(), now: e.clock.Now()}
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
	e := newEnv(t, testProject())
	name := e.reviewingWithCriteria()
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
	e := newEnv(t, testProject())
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
