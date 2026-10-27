// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"context"
	"testing"

	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// TestCommitStatuses: a status created through the API gets an id above
// every earlier one, its creator is the caller (the App's bot for an
// installation token), and the raw listing is newest first with ids,
// creators and creation times, as GitHub's is; a status a test seeds as
// another account without an id gets the next one, so it is the latest.
func TestCommitStatuses(t *testing.T) {
	srv, _, _ := newFake(t)
	ctx := context.Background()
	app := newApp(t, srv)
	bot := scopedClient(t, srv, app, target, ghclient.TokenPerms{Statuses: ghclient.PermWrite})
	const sha = "0123456789abcdef0123456789abcdef01234567"
	srv.SetCommitStatus(sha, fakegithub.CommitStatus{ID: 40, Context: "lint", State: "success"})

	first, err := bot.CreateCommitStatus(ctx, target, sha, "pending", "maul/acceptance", "verifying",
		"https://example.test/v")
	if err != nil || first <= 40 {
		t.Fatalf("CreateCommitStatus() = %d, %v, want an id above the seeded 40", first, err)
	}
	second, err := bot.CreateCommitStatus(ctx, target, sha, "success", "maul/acceptance", "1/1 automated passed", "")
	if err != nil || second <= first {
		t.Fatalf("second CreateCommitStatus() = %d, %v, want an id above %d", second, err, first)
	}
	srv.SetCommitStatus(sha, fakegithub.CommitStatus{Context: "maul/acceptance", State: "failure",
		Creator: fakegithub.Actor{Login: "mallory", ID: 1001, Type: "User"}})

	checkListedStatuses(t, bot, sha, second)
	all := srv.Statuses(sha)
	if len(all) != 4 || all[1].TargetURL != "" || all[2].TargetURL != "https://example.test/v" ||
		all[2].Creator != fakegithub.Bot {
		t.Errorf("Statuses() = %+v, want the four, newest first, with their target URLs and creators", all)
	}
	if latest, ok := srv.LatestStatus(sha, "maul/acceptance"); !ok || latest.Creator.Login != "mallory" {
		t.Errorf("LatestStatus() = %+v, %v, want mallory's", latest, ok)
	}

	// A token without statuses write may not post one.
	issues := scopedClient(t, srv, app, target, ghclient.TokenPerms{Issues: ghclient.PermWrite})
	if _, err := issues.CreateCommitStatus(ctx, target, sha, "success", "maul/acceptance", "", ""); err == nil {
		t.Error("an issues token posted a commit status")
	}
}

// checkListedStatuses asserts the API listing of TestCommitStatuses'
// statuses on sha: mallory's newest, the bot's second (id second) before
// it with its time, the seeded lint status oldest.
func checkListedStatuses(t *testing.T, c *ghclient.Client, sha string, second int64) {
	t.Helper()
	got, err := c.ListCommitStatuses(context.Background(), target, sha)
	if err != nil || len(got) != 4 {
		t.Fatalf("ListCommitStatuses() = %+v, %v, want 4", got, err)
	}
	if got[0].Creator != "mallory" || got[0].ID <= second || got[0].State != "failure" {
		t.Errorf("newest status = %+v, want mallory's, with an id above %d", got[0], second)
	}
	if got[1].ID != second || got[1].Creator != fakegithub.BotLogin || got[1].Description != "1/1 automated passed" ||
		got[1].CreatedAt.IsZero() {
		t.Errorf("second status = %+v, want the bot's %d with its time", got[1], second)
	}
	if got[3].ID != 40 || got[3].Context != "lint" {
		t.Errorf("oldest status = %+v, want the seeded lint 40", got[3])
	}
}

// TestHumanPush: a human's push moves the branch in that repository only,
// to a new commit on its old head carrying the file, and is no ref write
// of the App's.
func TestHumanPush(t *testing.T) {
	srv, pat, _ := newFake(t)
	ctx := context.Background()
	pr, err := pat.CreatePR(ctx, target, ghclient.PRRequest{Title: "t", Head: "maul-intent/target-1", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := srv.HumanPush(target.Owner, target.Name, "maul-intent/target-1", "NOTES.md", "by hand\n", "docs: notes")
	if sha == "" || sha == fakegithub.BaseSHA {
		t.Fatalf("HumanPush() = %q, want a new commit", sha)
	}
	if got := srv.RepoBranchHead(target.Owner, target.Name, "maul-intent/target-1"); got != sha {
		t.Errorf("branch head = %s, want the pushed %s", got, sha)
	}
	if got := srv.RepoBranchHead(intents.Owner, intents.Name, "maul-intent/target-1"); got != fakegithub.BaseSHA {
		t.Errorf("another repository's branch = %s, want untouched", got)
	}
	if p, ok := srv.Pull(pr.Number); !ok || p.HeadSHA != sha {
		t.Errorf("pull request head = %+v, want %s", p, sha)
	}
	c, ok := srv.CommitOf(sha)
	if !ok || string(c.Files["NOTES.md"]) != "by hand\n" || c.Message != "docs: notes" ||
		len(c.Parents) != 1 || c.Parents[0] != fakegithub.BaseSHA {
		t.Errorf("commit = %+v, want NOTES.md on the base", c)
	}
	if w := srv.RefWrites(); len(w) != 0 {
		t.Errorf("ref writes = %+v, want none: the push is not the App's", w)
	}
}

// TestDefaultComparePatch: an unseeded pair is a 404 until a default patch
// is set; a seeded pair keeps its own.
func TestDefaultComparePatch(t *testing.T) {
	srv, c, _ := newFake(t)
	ctx := context.Background()
	srv.SetComparePatch("main", "abc", "seeded")
	if _, err := c.ComparePatch(ctx, target, "main", "def"); err == nil {
		t.Error("an unseeded compare answered before any default")
	}
	srv.SetDefaultComparePatch("default")
	if got, err := c.ComparePatch(ctx, target, "main", "def"); err != nil || got != "default" {
		t.Errorf("unseeded compare = %q, %v, want the default", got, err)
	}
	if got, err := c.ComparePatch(ctx, target, "main", "abc"); err != nil || got != "seeded" {
		t.Errorf("seeded compare = %q, %v, want its own", got, err)
	}
}

// TestDeniedPermission: an installation that does not hold a permission
// refuses a token asking for it, as GitHub answers 422 for a permission the
// App was not granted, and still mints every other.
func TestDeniedPermission(t *testing.T) {
	srv, _, _ := newFake(t)
	ctx := context.Background()
	app := newApp(t, srv)
	srv.DenyPermission("statuses")
	if _, _, err := app.ScopedToken(ctx, target, ghclient.TokenPerms{Statuses: ghclient.PermWrite}); err == nil {
		t.Error("a token was minted with the denied statuses permission")
	}
	if _, _, err := app.ScopedToken(ctx, target, ghclient.TokenPerms{Issues: ghclient.PermWrite}); err != nil {
		t.Errorf("an issues token = %v, want minted", err)
	}
}
