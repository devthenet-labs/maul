// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
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
