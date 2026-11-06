// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/e2e/fakegithub"
	"github.com/devthenet-labs/maul/internal/ghclient"
)

// tarballFiles reads repo's tarball at ref: each file by its path under the
// archive's top-level directory, and that directory.
func tarballFiles(t *testing.T, c *ghclient.Client, repo ghclient.Repo, ref string) (map[string]string, string) {
	t.Helper()
	rc, err := c.Tarball(context.Background(), repo, ref)
	if err != nil {
		t.Fatalf("Tarball(%s, %s) error = %v", repo, ref, err)
	}
	defer func() { _ = rc.Close() }()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out, top := map[string]string{}, ""
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, top
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		dir, path, _ := strings.Cut(hdr.Name, "/")
		top = dir + "/"
		out[path] = string(body)
	}
}

// TestCommitTreesInTarball: with commit trees served, a pushed commit's
// tarball is the repository's tree with every commit on its way from the
// base applied, under a top-level directory naming that commit; the base's
// tarball is the tree as it was. Off (the default), every ref serves the
// base's tree.
func TestCommitTreesInTarball(t *testing.T) {
	srv, c, _ := newFake(t)
	srv.SetRepoFile(target.Owner, target.Name, "main.go", "package main // base\n")
	first := srv.HumanPush(target.Owner, target.Name, "feature", "main.go", "package main // first\n", "first")
	second := srv.HumanPush(target.Owner, target.Name, "feature", "extra.go", "package main // extra\n", "second")

	if got, _ := tarballFiles(t, c, target, second); got["main.go"] != "package main // base\n" ||
		got["extra.go"] != "" {
		t.Errorf("off: tarball at %s = %v, want the base's tree", second, got)
	}

	srv.ServeCommitTrees(true)
	got, top := tarballFiles(t, c, target, second)
	if got["main.go"] != "package main // first\n" || got["extra.go"] != "package main // extra\n" ||
		got["README.md"] == "" {
		t.Errorf("tarball at %s = %v, want the base's tree with both commits applied", second, got)
	}
	if want := "devthenet-labs-maul-target-" + second[:7] + "/"; top != want {
		t.Errorf("top-level directory %q, want %q", top, want)
	}
	if got, _ := tarballFiles(t, c, target, first); got["main.go"] != "package main // first\n" ||
		got["extra.go"] != "" {
		t.Errorf("tarball at %s = %v, want the first commit's tree", first, got)
	}
	if got, _ := tarballFiles(t, c, target, fakegithub.BaseSHA); got["main.go"] != "package main // base\n" {
		t.Errorf("tarball at the base = %v, want the base's tree", got)
	}
}

// TestCompareMergeBase: compare names the merge base, as GitHub's does: a
// branch never pushed is at the base, so a commit pushed on another branch
// has the base as its merge base with it; a commit with itself is its own.
func TestCompareMergeBase(t *testing.T) {
	srv, c, _ := newFake(t)
	head := srv.HumanPush(target.Owner, target.Name, "feature", "main.go", "package main\n", "feature")
	next := srv.HumanPush(target.Owner, target.Name, "feature", "main.go", "package main // 2\n", "again")
	ctx := context.Background()
	for _, tc := range []struct{ base, head, want string }{
		{"main", head, fakegithub.BaseSHA},
		{"main", next, fakegithub.BaseSHA},
		{head, next, head},
		{next, next, next},
	} {
		got, err := c.MergeBase(ctx, target, tc.base, tc.head)
		if err != nil || got != tc.want {
			t.Errorf("MergeBase(%s, %s) = %q, %v; want %q", tc.base, tc.head, got, err, tc.want)
		}
	}
}
