// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

func resolvedAgo(d time.Duration) func(*v1alpha1.Project) {
	return func(p *v1alpha1.Project) {
		at := metav1.NewTime(testNow.Add(-d))
		p.Status.Approvers.ResolvedAt = &at
	}
}

func identities(ids ...v1alpha1.ProjectApproverIdentity) func(*v1alpha1.Project) {
	return func(p *v1alpha1.Project) { p.Status.Approvers.Identities = ids }
}

func logins(l ...string) func(*v1alpha1.Project) {
	return func(p *v1alpha1.Project) { p.Spec.Approvers.Logins = l }
}

func TestGate(t *testing.T) {
	who := func(login string, id int64, write bool) v1alpha1.ProjectApproverIdentity {
		return v1alpha1.ProjectApproverIdentity{Login: login, ID: id, Write: write}
	}
	for _, tc := range []struct {
		name   string
		mut    []func(*v1alpha1.Project)
		id     int64
		reason string
		login  string
	}{
		{name: "an approver with write access", id: 1, login: "alice"},
		{name: "the other approver", id: 2, login: "bob"},
		{name: "no GitHub identity on the session", id: 0, reason: ReasonNoGitHubIdentity},
		{name: "a negative id is no identity", id: -1, reason: ReasonNoGitHubIdentity},
		{name: "nothing resolved yet", id: 1, reason: ReasonApproversUnresolved,
			mut: []func(*v1alpha1.Project){func(p *v1alpha1.Project) { p.Status.Approvers = nil }}},
		{name: "identities without a resolution time", id: 1, reason: ReasonApproversUnresolved,
			mut: []func(*v1alpha1.Project){func(p *v1alpha1.Project) { p.Status.Approvers.ResolvedAt = nil }}},
		{name: "an account that is no approver", id: 99, reason: ReasonNotApprover},
		{name: "removed from the list though status still lists it", id: 2, reason: ReasonNotApprover,
			mut: []func(*v1alpha1.Project){logins("alice")}},
		{name: "on the list without write access", id: 2, reason: ReasonNoWriteAccess,
			mut: []func(*v1alpha1.Project){identities(who("alice", 1, true), who("bob", 2, false))}},
		{name: "resolved too long ago", id: 1, reason: ReasonApproversStale,
			mut: []func(*v1alpha1.Project){resolvedAgo(ApproversMaxAge + time.Second)}},
		{name: "resolved exactly at the limit", id: 1, login: "alice",
			mut: []func(*v1alpha1.Project){resolvedAgo(ApproversMaxAge)}},
		{name: "stale and without write access says stale: the write fact is as old", id: 2,
			reason: ReasonApproversStale,
			mut: []func(*v1alpha1.Project){identities(who("bob", 2, false)),
				resolvedAgo(time.Hour)}},
		{name: "a stranger is not an approver however old the resolution", id: 99, reason: ReasonNotApprover,
			mut: []func(*v1alpha1.Project){resolvedAgo(time.Hour)}},
		{name: "a login added since the last resolution is not resolved yet", id: 3,
			reason: ReasonApproversUnresolved, mut: []func(*v1alpha1.Project){logins("alice", "bob", "carol")}},
		{name: "a resolved approver is not held up by an unresolved newcomer", id: 1, login: "alice",
			mut: []func(*v1alpha1.Project){logins("alice", "bob", "carol")}},
		{name: "the list spells the login in another case", id: 1, login: "alice",
			mut: []func(*v1alpha1.Project){logins("Alice", "bob")}},
		{name: "an account GitHub does not know matches nobody", id: 0, reason: ReasonNoGitHubIdentity,
			mut: []func(*v1alpha1.Project){identities(who("alice", 0, true))}},
		{name: "two logins of one account: the one still listed with write access counts", id: 1,
			login: "alice-new", mut: []func(*v1alpha1.Project){logins("alice-new", "bob"),
				identities(who("alice", 1, true), who("alice-new", 1, true), who("bob", 2, true))}},
		{name: "two logins of one account, only the unlisted one has write access", id: 1,
			reason: ReasonNoWriteAccess, mut: []func(*v1alpha1.Project){logins("alice-new", "bob"),
				identities(who("alice", 1, true), who("alice-new", 1, false), who("bob", 2, true))}},
		{name: "a resolution dated in the future is not stale", id: 1, login: "alice",
			mut: []func(*v1alpha1.Project){resolvedAgo(-time.Minute)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Gate(baseProject(tc.mut...), tc.id, testNow)
			want := GateResult{OK: tc.reason == "", Reason: tc.reason, Login: tc.login}
			if got != want {
				t.Errorf("Gate = %+v, want %+v", got, want)
			}
		})
	}
}

// The refusal reasons are keys the page and the logs carry: fixed.
func TestGateReasonsAreFixed(t *testing.T) {
	got := []string{ReasonNoGitHubIdentity, ReasonNotApprover, ReasonNoWriteAccess, ReasonApproversUnresolved,
		ReasonApproversStale}
	want := []string{"no-github-identity", "not-approver", "no-write-access", "approvers-unresolved",
		"approvers-stale"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reasons %v, want %v", got, want)
	}
	if ApproversMaxAge != 30*time.Minute {
		t.Errorf("ApproversMaxAge = %s, want 30m", ApproversMaxAge)
	}
}

// Whatever the Project and whoever asks, the gate opens only when all of
// its facts hold at once: a session id, an identity of that id with write
// access, that identity's login on the live list, and a recent resolution.
// And it always gives exactly one of: OK with a login, or a reason.
func TestGateOpensOnlyOnEveryFact(t *testing.T) {
	prop := func(p *v1alpha1.Project, id int64) bool {
		res := Gate(p, id, testNow)
		if res.OK != (res.Reason == "") || res.OK != (res.Login != "") {
			t.Logf("Gate = %+v: OK, reason and login disagree", res)
			return false
		}
		if !res.OK {
			return true
		}
		st := p.Status.Approvers
		if id <= 0 || st == nil || st.ResolvedAt == nil || testNow.Sub(st.ResolvedAt.Time) > ApproversMaxAge {
			t.Logf("opened for id %d on %+v", id, st)
			return false
		}
		if !p.Spec.Approvers.Has(res.Login) {
			t.Logf("opened as %q, not on the list %v", res.Login, p.Spec.Approvers.Logins)
			return false
		}
		for _, who := range st.Identities {
			if who.ID == id && who.Write && who.Login == res.Login {
				return true
			}
		}
		t.Logf("opened as %q for id %d with no such identity in %+v", res.Login, id, st.Identities)
		return false
	}
	cfg := quickConfig(3000, func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(genProject(r))
		args[1] = reflect.ValueOf(int64(r.Intn(10) - 1))
	})
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// The gate reads; it never changes the Project it judges.
func TestGateLeavesTheProjectAlone(t *testing.T) {
	p := baseProject()
	before := p.DeepCopy()
	Gate(p, 1, testNow)
	if !reflect.DeepEqual(p, before) {
		t.Error("Gate changed the Project")
	}
}
