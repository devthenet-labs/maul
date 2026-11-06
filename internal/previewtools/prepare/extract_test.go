// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractGitHubArchive(t *testing.T) {
	dst := t.TempDir()
	if err := Extract(bytes.NewReader(tarball(t, githubEntries()...)), dst, MaxTreeBytes); err != nil {
		t.Fatal(err)
	}
	sameListing(t, listing(t, dst), githubListing)
	raw, err := os.ReadFile(filepath.Join(dst, "cmd", "tool", "main.go"))
	if err != nil || string(raw) != "package main\n" {
		t.Errorf("main.go = %q, %v", raw, err)
	}
	if got, err := os.Readlink(filepath.Join(dst, "cmd", "README")); err != nil || got != "../go.mod" {
		t.Errorf("link = %q, %v", got, err)
	}
}

// TestExtractModes: a file's mode is its archive mode's permission bits
// with group and others never writable and never setuid.
func TestExtractModes(t *testing.T) {
	dst := t.TempDir()
	odd := []entry{dir(top),
		{name: top + "wide", body: "x", typ: tar.TypeReg, mode: 0o4777},
		{name: top + "private", body: "x", typ: tar.TypeReg, mode: 0o600},
	}
	if err := Extract(bytes.NewReader(tarball(t, odd...)), dst, MaxTreeBytes); err != nil {
		t.Fatal(err)
	}
	sameListing(t, listing(t, dst), map[string]string{"wide": "-rwxr-xr-x", "private": "-rw-------"})
}

func TestExtractRefusals(t *testing.T) {
	outside := t.TempDir()
	tests := []struct {
		name    string
		entries []entry
		limit   int64
		want    string
	}{
		{"dot-dot entry", []entry{dir(top), file(top+"../../escape", "x")}, 0, "escapes"},
		{"dot-dot inside the top", []entry{dir(top), file(top+"a/../../escape", "x")}, 0, "escapes"},
		{"absolute entry", []entry{file("/etc/passwd", "x")}, 0, "escapes"},
		{"absolute below the top", []entry{dir(top), file(top+"/etc/passwd", "x")}, 0, ""},
		{"two top directories", []entry{dir(top), file(top+"a", "x"), file("other/b", "x")}, 0, "top-level"},
		{"a file at the top", []entry{file("README", "x")}, 0, "top-level"},
		{"absolute link", []entry{dir(top), link(top+"l", "/etc/passwd")}, 0, "outside"},
		{"link above the tree", []entry{dir(top), link(top+"l", "../../x")}, 0, "outside"},
		{"link to the parent", []entry{dir(top), link(top+"l", "..")}, 0, "outside"},
		{"empty link", []entry{dir(top), link(top+"l", "")}, 0, "outside"},
		{"link resolving outside through a link", []entry{dir(top), dir(top + "deep/"),
			link(top+"deep/q", ".."), link(top+"deep/l", "q/../..")}, 0, "outside"},
		{"entry through a link", []entry{dir(top), dir(top + "sub/"), link(top+"l", "sub"),
			file(top+"l/f", "x")}, 0, "through the link"},
		{"entry through a link to outside", []entry{dir(top), link(top+"l", ".."), file(top+"l/f", "x")}, 0,
			"outside"},
		{"hard link", []entry{dir(top), file(top+"a", "x"),
			{name: top + "b", link: top + "a", typ: tar.TypeLink}}, 0, "type"},
		{"device", []entry{dir(top), {name: top + "dev", typ: tar.TypeChar}}, 0, "type"},
		{"fifo", []entry{dir(top), {name: top + "fifo", typ: tar.TypeFifo}}, 0, "type"},
		{"duplicate file", []entry{dir(top), file(top+"a", "x"), file(top+"a", "y")}, 0, "twice"},
		{"file over a directory", []entry{dir(top), dir(top + "a/"), file(top+"a", "y")}, 0, "twice"},
		{"over the size limit", []entry{dir(top), file(top+"a", "12345"), file(top+"b", "678")}, 7, "limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "tree")
			if err := os.Mkdir(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			limit := tc.limit
			if limit == 0 {
				limit = MaxTreeBytes
			}
			err := Extract(bytes.NewReader(tarball(t, tc.entries...)), dst, limit)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Extract = %v, want ErrRefused naming %q", err, tc.want)
			}
			if got := listing(t, outside); len(got) != 0 {
				t.Errorf("wrote outside the tree: %v", got)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape")); !os.IsNotExist(err) {
				t.Errorf("an escape landed beside the tree: %v", err)
			}
		})
	}
}

func TestExtractNotGzip(t *testing.T) {
	err := Extract(strings.NewReader("not a tarball"), t.TempDir(), MaxTreeBytes)
	if err == nil {
		t.Fatal("Extract of garbage succeeded")
	}
}

func TestExtractTruncated(t *testing.T) {
	raw := tarball(t, githubEntries()...)
	err := Extract(bytes.NewReader(raw[:len(raw)/2]), t.TempDir(), MaxTreeBytes)
	if err == nil {
		t.Fatal("Extract of a truncated archive succeeded")
	}
}

// TestExtractEmptyDestinationOnly: a tree is never merged into another.
func TestExtractEmptyDestinationOnly(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "left-over"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Extract(bytes.NewReader(tarball(t, githubEntries()...)), dst, MaxTreeBytes)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Extract = %v, want ErrRefused for a non-empty destination", err)
	}
}

// TestExtractEmptyArchive: an archive with no tree is refused, never an
// empty tree.
func TestExtractEmptyArchive(t *testing.T) {
	err := Extract(bytes.NewReader(tarball(t, globalHeader())), t.TempDir(), MaxTreeBytes)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Extract = %v, want ErrRefused", err)
	}
}
