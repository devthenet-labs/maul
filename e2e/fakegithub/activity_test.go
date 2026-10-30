// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// TestPushedAt: the repository activity records the App's branch creation
// and a human's push, each at the fake's clock, and ghclient.PushedAt
// finds the push that moved the branch to a commit, never the creation,
// and only in its own repository.
func TestPushedAt(t *testing.T) {
	srv, c, clk := newFake(t)
	ctx := context.Background()
	const branch = "maul-intent/target-1"
	build, err := c.CreateCommit(ctx, target, ghclient.CommitRequest{BaseSHA: fakegithub.BaseSHA, Message: "build",
		Files: []ghclient.CommitFile{{Path: "v.go", Mode: "100644", Content: []byte("build")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBranchRef(ctx, target, branch, build); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	human := srv.HumanPush(target.Owner, target.Name, branch, "NOTES.md", "hand\n", "docs: notes")
	if _, ok, err := c.PushedAt(ctx, target, branch, build); err != nil || ok {
		t.Errorf("PushedAt(created) = %v, %v; want not found: a creation is no push", ok, err)
	}
	at, ok, err := c.PushedAt(ctx, target, branch, human)
	if want := clk.now(); err != nil || !ok || !at.Equal(want) {
		t.Errorf("PushedAt(human) = %v, %v, %v; want %v", at, ok, err, want)
	}
	if _, ok, err := c.PushedAt(ctx, intents, branch, human); err != nil || ok {
		t.Errorf("PushedAt in another repository = %v, %v; want not found", ok, err)
	}
}
