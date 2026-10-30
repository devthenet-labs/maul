// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"testing"

	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// TestIssueDependencies reads the fake's dependencies through the shipped
// client: the listing with closed blockers and their reasons, its tag moving
// with a blocker's state, the summary counting what a one-repository token
// is not shown, and the unsupported and failing switches.
func TestIssueDependencies(t *testing.T) {
	srv, _, _ := newFake(t)
	ctx := context.Background()
	human := fakegithub.Actor{Login: "octocat", ID: 583231, Type: "User"}
	a := srv.OpenIssue(intents.Owner, intents.Name, "A", "", nil, human)
	b := srv.OpenIssue(intents.Owner, intents.Name, "B", "", nil, human)
	other := srv.OpenIssue(intents.Owner, "lib", "elsewhere", "", nil, human)
	srv.AddBlockedBy(b, a)
	srv.AddBlockedBy(b, other)

	app := newApp(t, srv)
	c := scopedClient(t, srv, app, intents, ghclient.TokenPerms{Issues: ghclient.PermRead})
	first, err := c.ListBlockedBy(ctx, intents, int64(b), "")
	if err != nil {
		t.Fatalf("ListBlockedBy() error = %v", err)
	}
	if len(first.Blockers) != 1 || first.Blockers[0].Number != a || first.Blockers[0].State != "open" ||
		first.Blockers[0].Repo != intents {
		t.Fatalf("blockers = %+v, want #%d alone (the other repository's is not this token's)", first.Blockers, a)
	}
	is, err := c.GetIssue(ctx, intents, b)
	if err != nil {
		t.Fatal(err)
	}
	if is.Dependencies == nil || is.Dependencies.BlockedBy != 2 || is.Dependencies.TotalBlockedBy != 2 {
		t.Fatalf("summary = %+v, want 2 open of 2, the hidden one counted", is.Dependencies)
	}

	again, err := c.ListBlockedBy(ctx, intents, int64(b), first.ETag)
	if err != nil || !again.NotModified {
		t.Fatalf("an unchanged listing = %+v, %v; want NotModified", again, err)
	}
	srv.CloseIssueAs(a, "not_planned", human)
	closed, err := c.ListBlockedBy(ctx, intents, int64(b), first.ETag)
	if err != nil || closed.NotModified || closed.Blockers[0].State != "closed" ||
		closed.Blockers[0].StateReason != "not_planned" {
		t.Fatalf("after the close = %+v, %v; want the blocker closed as not planned under a new tag", closed, err)
	}

	srv.SetCrossRepositoryBlockersVisible(true)
	if all, err := c.ListBlockedBy(ctx, intents, int64(b), ""); err != nil || len(all.Blockers) != 2 {
		t.Fatalf("with cross-repository blockers visible = %+v, %v; want both", all, err)
	}
}

// TestIssueDependenciesUnavailable: the failing switch answers 500, the
// unsupported one 404 with no summary on the issue.
func TestIssueDependenciesUnavailable(t *testing.T) {
	srv, _, _ := newFake(t)
	ctx := context.Background()
	human := fakegithub.Actor{Login: "octocat", ID: 583231, Type: "User"}
	b := srv.OpenIssue(intents.Owner, intents.Name, "B", "", nil, human)
	c := scopedClient(t, srv, newApp(t, srv), intents, ghclient.TokenPerms{Issues: ghclient.PermRead})
	srv.SetDependenciesFailing(true)
	if _, err := c.ListBlockedBy(ctx, intents, int64(b), ""); err == nil || ghclient.IsNotFound(err) ||
		ghclient.IsForbidden(err) {
		t.Fatalf("failing listing error = %v, want a server error", err)
	}
	srv.SetDependenciesFailing(false)
	srv.SetDependenciesUnsupported(true)
	if _, err := c.ListBlockedBy(ctx, intents, int64(b), ""); !ghclient.IsNotFound(err) {
		t.Fatalf("unsupported listing error = %v, want 404", err)
	}
	if is, err := c.GetIssue(ctx, intents, b); err != nil || is.Dependencies != nil {
		t.Fatalf("unsupported summary = %+v, %v; want none", is, err)
	}
}
