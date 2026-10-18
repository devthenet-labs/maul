// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/credgate"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

const accountURL = "https://status.example.com/#/account"

// fakeCredentials is the credential lookup: each login's record health,
// absent when the login has none.
type fakeCredentials struct {
	mu     sync.Mutex
	health map[string]v1alpha1.ModelCredentialHealth
}

func (f *fakeCredentials) set(login string, h v1alpha1.ModelCredentialHealth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health[login] = h
}

func (f *fakeCredentials) ModelCredential(_ context.Context, owner string, provider v1alpha1.ModelCredentialProvider) (
	*v1alpha1.ModelCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for login, h := range f.health {
		if v1alpha1.ModelCredentialOwner(actorOf(login).ID) != owner {
			continue
		}
		c := &v1alpha1.ModelCredential{Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: provider,
			Kind: v1alpha1.ModelCredentialAPIKey}, Status: v1alpha1.ModelCredentialStatus{Health: h}}
		c.Name = v1alpha1.ModelCredentialName(owner, provider)
		return c, nil
	}
	return nil, nil
}

// enforcing turns attribution and --user-credentials enforce on, with the
// given logins' credentials Valid.
func (e *env) enforcing(valid ...string) *fakeCredentials {
	e.attributing()
	creds := &fakeCredentials{health: map[string]v1alpha1.ModelCredentialHealth{}}
	for _, login := range valid {
		creds.set(login, v1alpha1.ModelCredentialValid)
	}
	e.intent.Settings.UserCredentials = credgate.ModeEnforce
	e.intent.Settings.AccountURL = accountURL
	e.intent.Credentials = creds
	return creds
}

func credentialBlock(in *v1alpha1.Intent) *metav1.Condition {
	c := meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionCredentialRequired)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	return c
}

// TestCredentialBlockHoldsBuildUntilValid: under enforce, a build approved
// by someone with no Valid credential is never created; the Intent is
// Blocked naming them and the Account page, and once their credential is
// Valid it resumes with nothing replanned, the build theirs.
func TestCredentialBlockHoldsBuildUntilValid(t *testing.T) {
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
	e := newEnv(t, p)
	e.gh.perms[reviewer] = ghclient.PermissionWrite
	creds := e.enforcing(approver)
	name := e.discovered()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	plans := len(e.runsOf(name, v1alpha1.IntentStagePlan))

	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", reviewer)
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	c := credentialBlock(in)
	if c == nil || c.Reason != credgate.ReasonMissing || !strings.Contains(c.Message, "@"+reviewer) ||
		!strings.Contains(c.Message, accountURL) {
		t.Fatalf("CredentialRequired = %+v", c)
	}
	if h := in.Status.CredentialHold; h == nil || h.ID != actorOf(reviewer).ID {
		t.Fatalf("credentialHold = %+v, want the approver", h)
	}
	if v1alpha1.IntentBlockedFrom(in) != v1alpha1.IntentBuilding {
		t.Fatalf("blocked from %s, want Building", v1alpha1.IntentBlockedFrom(in))
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageBuild)); n != 0 {
		t.Fatalf("%d build runs were created for an approver with no credential", n)
	}
	e.mustIntent(name)
	if body := e.statusBody(); !strings.Contains(body, reviewer) || !strings.Contains(body, accountURL) {
		t.Errorf("the status comment does not name the approver and the Account page:\n%s", body)
	}

	// Stored, not yet probed: still blocked, now saying so.
	creds.set(reviewer, v1alpha1.ModelCredentialUnknown)
	e.clock.Advance(time.Minute)
	e.mustIntent(name)
	if c := credentialBlock(e.get(name)); c == nil || c.Reason != credgate.ReasonUnverified {
		t.Fatalf("after storing an unprobed credential: %+v", c)
	}

	creds.set(reviewer, v1alpha1.ModelCredentialValid)
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if credentialBlock(in) != nil || in.Status.CredentialHold != nil {
		t.Errorf("the block did not lift cleanly: %+v, hold %+v", in.Status.Conditions, in.Status.CredentialHold)
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStagePlan)); n != plans {
		t.Errorf("plan runs %d, want %d: nothing is replanned", n, plans)
	}
	build := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(build) != 1 || build[0].Spec.AttributedTo == nil || build[0].Spec.AttributedTo.User == nil ||
		build[0].Spec.AttributedTo.User.Login != reviewer {
		t.Fatalf("build runs = %+v, want one attributed to the approver", build)
	}
}

// TestCredentialBlockReapproval: while a build is held on its approver's
// credential, an approval by an approver with no valid credential is told
// so and changes nothing, and one by an approver holding a Valid credential
// replaces the approval, so the build runs as theirs.
func TestCredentialBlockReapproval(t *testing.T) {
	const third = "dana"
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer, third)
	e := newEnv(t, p)
	e.gh.perms[reviewer] = ghclient.PermissionWrite
	e.gh.perms[third] = ghclient.PermissionWrite
	e.enforcing(approver)
	name := e.discovered()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", reviewer)
	e.drive(name, v1alpha1.IntentBlocked, repoImage)

	e.clock.Advance(time.Minute)
	e.gh.comment(third, "/patchy approve")
	e.clock.Advance(time.Minute)
	e.mustIntent(name)
	in := e.get(name)
	if in.Status.Phase != v1alpha1.IntentBlocked || in.Status.Approval.By != reviewer {
		t.Fatalf("a keyless approver's approval was taken: phase %s, approval %+v", in.Status.Phase,
			in.Status.Approval)
	}
	found := false
	for _, c := range e.gh.ownComments() {
		if strings.Contains(c.Body, "@"+third+" has none stored") {
			found = true
		}
	}
	if !found {
		t.Error("the keyless approver was not told why their approval was not taken")
	}

	e.clock.Advance(time.Minute)
	e.gh.comment(approver, "/patchy approve")
	in = e.drive(name, v1alpha1.IntentInReview, repoImage)
	if in.Status.Approval.By != approver || in.Status.Approval.ByID != actorOf(approver).ID {
		t.Fatalf("approval = %+v, want the key holder's", in.Status.Approval)
	}
	build := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(build) != 1 || build[0].Spec.AttributedTo.User == nil ||
		build[0].Spec.AttributedTo.User.Login != approver {
		t.Fatalf("build attributed to %+v, want the re-approver", build[0].Spec.AttributedTo)
	}
}

// TestCredentialBlockHoldsPlan: a plan triggered by an approver with no
// valid credential is held before it runs, and runs once it is.
func TestCredentialBlockHoldsPlan(t *testing.T) {
	e := newEnv(t, testProject())
	creds := e.enforcing()
	name := e.discovered()
	in := e.drive(name, v1alpha1.IntentBlocked, repoImage)
	if c := credentialBlock(in); c == nil || !strings.Contains(c.Message, "@"+approver) ||
		strings.Contains(c.Message, "approves it") {
		t.Fatalf("CredentialRequired = %+v", c)
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStagePlan)); n != 0 {
		t.Fatalf("%d plan runs were created", n)
	}
	creds.set(approver, v1alpha1.ModelCredentialValid)
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	if n := len(e.runsOf(name, v1alpha1.IntentStagePlan)); n != 1 {
		t.Errorf("%d plan runs, want 1", n)
	}
}

// TestCredentialBlockNeverUnderAttribute: under attribute nothing blocks,
// whoever lacks a credential.
func TestCredentialBlockNeverUnderAttribute(t *testing.T) {
	e := newEnv(t, testProject())
	e.enforcing()
	e.intent.Settings.UserCredentials = credgate.ModeAttribute
	name := e.discovered()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	if meta.FindStatusCondition(in.Status.Conditions, v1alpha1.ConditionCredentialRequired) != nil {
		t.Error("a CredentialRequired condition was written under attribute")
	}
}

// heldFromReview drives an Intent built by the approver (whose credential
// is Valid) to InReview under enforce, with reviewer an approver too.
func heldFromReview(t *testing.T, checks bool) (*env, *fakeCredentials, string, *v1alpha1.Intent) {
	t.Helper()
	p := testProject()
	p.Spec.Approvers.Logins = append(p.Spec.Approvers.Logins, reviewer)
	if checks {
		p.Spec.Checks.Fix = []string{"test"}
	}
	e := newEnv(t, p)
	e.gh.perms[reviewer] = ghclient.PermissionWrite
	creds := e.enforcing(approver)
	name := e.discovered()
	e.drive(name, v1alpha1.IntentAwaitingApproval, repoImage)
	e.clock.Advance(time.Minute)
	e.gh.label(1, "patchy:approved", approver)
	return e, creds, name, e.drive(name, v1alpha1.IntentInReview, repoImage)
}

// assertHeldFromReview: the Intent is Blocked from InReview on who's
// credential, and no revise run was created.
func assertHeldFromReview(t *testing.T, e *env, name, who string) {
	t.Helper()
	in := e.get(name)
	c := credentialBlock(in)
	if in.Status.Phase != v1alpha1.IntentBlocked || c == nil || !strings.Contains(c.Message, "@"+who) {
		t.Fatalf("phase %s, CredentialRequired %+v; want Blocked naming %s", in.Status.Phase, c, who)
	}
	if from := v1alpha1.IntentBlockedFrom(in); from != v1alpha1.IntentInReview {
		t.Fatalf("blocked from %s, want InReview", from)
	}
	if h := in.Status.CredentialHold; h == nil || h.ID != actorOf(who).ID {
		t.Fatalf("credentialHold = %+v, want %s", h, who)
	}
	if n := len(e.runsOf(name, v1alpha1.IntentStageRevise)); n != 0 {
		t.Fatalf("%d revise runs were created while held", n)
	}
}

// TestCredentialBlockHoldsCommandRevise: a /patchy revise by an approver
// with no valid credential creates no revise run; the Intent is Blocked
// from InReview naming the commenter, and once their credential is Valid
// the same command is read again and starts its round, theirs.
func TestCredentialBlockHoldsCommandRevise(t *testing.T) {
	e, creds, name, in := heldFromReview(t, false)
	pr := in.Status.PullRequests[0]
	e.gh.prComments[pr.Number] = []*ghclient.Comment{{ID: 940, NodeID: "comment-940",
		UserLogin: reviewer, UserID: actorOf(reviewer).ID, UserType: "User",
		Body:      "/patchy revise Please check the response header.",
		CreatedAt: e.clock.Now(), UpdatedAt: e.clock.Now()}}
	e.clock.Advance(time.Second)
	e.drive(name, v1alpha1.IntentBlocked, repoImage)
	assertHeldFromReview(t, e, name, reviewer)

	creds.set(reviewer, v1alpha1.ModelCredentialValid)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		return len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 1
	})
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerCommand || run.Spec.Inputs.CommandID != 940 ||
		!reflect.DeepEqual(run.Spec.AttributedTo, person(reviewer, v1alpha1.AttributionCommenter)) {
		t.Fatalf("revise run = %+v, want command 940's round, the commenter's", run.Spec)
	}
	if in := e.get(name); credentialBlock(in) != nil || in.Status.CredentialHold != nil {
		t.Errorf("the block did not lift cleanly: %+v, hold %+v", in.Status.Conditions, in.Status.CredentialHold)
	}
}

// TestCredentialBlockHoldsReviewRound: a changes-requested review by an
// approver with no valid credential creates no revise run; the Intent is
// Blocked naming the newest reviewer, and the round starts, theirs, once
// their credential is Valid.
func TestCredentialBlockHoldsReviewRound(t *testing.T) {
	e, creds, name, in := heldFromReview(t, false)
	e.gh.reviews[in.Status.PullRequests[0].Number] = []ghclient.Review{{ID: 501, NodeID: "review-501",
		Author: actorOf(reviewer), State: "CHANGES_REQUESTED", Body: "Add a test.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentBlocked, repoImage)
	assertHeldFromReview(t, e, name, reviewer)

	creds.set(reviewer, v1alpha1.ModelCredentialValid)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		return len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 1
	})
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerReview ||
		!reflect.DeepEqual(run.Spec.AttributedTo, person(reviewer, v1alpha1.AttributionReviewer)) {
		t.Fatalf("revise run = %+v, want the review round, the reviewer's", run.Spec)
	}
}

// TestCredentialBlockHoldsCheckFix: a check-fix round is the build's
// approver's, so once that approver's credential is rejected a failing
// check creates no run; the Intent is Blocked naming the approver, and the
// fix round starts once the credential is Valid again.
func TestCredentialBlockHoldsCheckFix(t *testing.T) {
	e, creds, name, in := heldFromReview(t, true)
	creds.set(approver, v1alpha1.ModelCredentialRejected)
	head := in.Status.PullRequests[0].HeadSHA
	e.gh.checks[head] = []ghclient.CheckRun{{ID: 71, Name: "test", HeadSHA: head,
		Status: "completed", Conclusion: "failure", AppSlug: "github-actions",
		Output: ghclient.CheckOutput{Title: "Go failed", Summary: "failure"}}}
	e.drive(name, v1alpha1.IntentBlocked, repoImage)
	assertHeldFromReview(t, e, name, approver)
	if c := credentialBlock(e.get(name)); c.Reason != credgate.ReasonRejected {
		t.Errorf("reason %s, want %s", c.Reason, credgate.ReasonRejected)
	}

	creds.set(approver, v1alpha1.ModelCredentialValid)
	e.driveUntil(name, func(*v1alpha1.Intent) bool {
		return len(e.runsOf(name, v1alpha1.IntentStageRevise)) == 1
	})
	run := e.runsOf(name, v1alpha1.IntentStageRevise)[0]
	if run.Spec.Trigger != v1alpha1.IntentRunTriggerChecks ||
		!reflect.DeepEqual(run.Spec.AttributedTo, person(approver, v1alpha1.AttributionApprover)) {
		t.Fatalf("check-fix run = %+v, want the build's approver's", run.Spec)
	}
}
