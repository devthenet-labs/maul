// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package prepare

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadable: every entry below the directory becomes readable by all
// (and searchable, or executable where anyone could execute it); the
// directory itself, a volume's root the init does not own, is untouched,
// and no link is followed.
func TestReadable(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, filepath.Join(root, "sub")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]os.FileMode{
		filepath.Join(root, "sub", "private"): 0o600,
		filepath.Join(root, "tool"):           0o700,
		outside:                               0o600,
	}
	for p, m := range files {
		if err := os.WriteFile(p, []byte("x"), m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../outside", filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := Readable(root); err != nil {
		t.Fatal(err)
	}
	sameListing(t, listing(t, root), map[string]string{
		"sub":         "drwxr-xr-x",
		"sub/private": "-rw-r--r--",
		"tool":        "-rwxr-xr-x",
		"out":         "Lrwxrwxrwx",
	})
	for p, want := range map[string]os.FileMode{root: 0o700, outside: 0o600} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v, want %v untouched", p, got, want)
		}
	}
}

func TestInstall(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tool-exec")
	if err := os.WriteFile(src, []byte("\x7fELF static"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Install(src, dst); err != nil {
		t.Fatal(err)
	}
	sameListing(t, listing(t, dst), map[string]string{"tool-exec": "-r-xr-xr-x"})
	got, err := os.ReadFile(filepath.Join(dst, "tool-exec"))
	if err != nil || !bytes.Equal(got, []byte("\x7fELF static")) {
		t.Errorf("installed %q, %v", got, err)
	}
	// Never over an existing file: the volume is fresh, or something else
	// wrote it.
	if err := Install(src, dst); err == nil {
		t.Error("a second Install succeeded")
	}
}

func TestInstallRefusesNonRegular(t *testing.T) {
	if err := Install(t.TempDir(), t.TempDir()); err == nil {
		t.Error("Install of a directory succeeded")
	}
}

// pristine extracts githubEntries into a fresh directory, as prepare does.
func pristine(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "head")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Extract(bytes.NewReader(tarball(t, githubEntries()...)), src, MaxTreeBytes); err != nil {
		t.Fatal(err)
	}
	if err := Readable(src); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestSeed(t *testing.T) {
	src := pristine(t)
	work := t.TempDir()
	if err := Seed(src, work); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"tree": "drwxr-xr-x", "bin": "drwxr-xr-x", "scratch": "drwxr-xr-x"}
	for p, m := range githubListing {
		// The copy is the sandbox's own: writable by its owner.
		want["tree/"+p] = m
	}
	sameListing(t, listing(t, work), want)
	raw, err := os.ReadFile(filepath.Join(work, "tree", "cmd", "tool", "main.go"))
	if err != nil || string(raw) != "package main\n" {
		t.Errorf("main.go = %q, %v", raw, err)
	}
	if got, err := os.Readlink(filepath.Join(work, "tree", "cmd", "README")); err != nil || got != "../go.mod" {
		t.Errorf("link = %q, %v", got, err)
	}
}

// TestSeedOwnerWritable: a pristine file nobody may write is the owner's
// to write in the copy.
func TestSeedOwnerWritable(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "ro"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := Seed(src, work); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(work, "tree", "ro"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("ro copied as %v, %v; want 0644", info.Mode(), err)
	}
}

// TestSeedRefusals: path hygiene as for prepare, and never a write outside
// the work volume.
func TestSeedRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, src, work string)
		want  string
	}{
		{"link above the tree", func(t *testing.T, src, _ string) {
			symlink(t, "../../outside", filepath.Join(src, "l"))
		}, "outside"},
		{"absolute link", func(t *testing.T, src, _ string) {
			symlink(t, "/etc", filepath.Join(src, "l"))
		}, "outside"},
		{"link to the parent", func(t *testing.T, src, _ string) {
			symlink(t, "..", filepath.Join(src, "l"))
		}, "outside"},
		{"link resolving outside through a link", func(t *testing.T, src, _ string) {
			mkdir(t, filepath.Join(src, "deep"))
			symlink(t, "..", filepath.Join(src, "deep", "q"))
			symlink(t, "q/../..", filepath.Join(src, "deep", "l"))
		}, "outside"},
		{"fifo", func(t *testing.T, src, _ string) {
			mkfifo(t, filepath.Join(src, "fifo"))
		}, "type"},
		{"a tree already there", func(t *testing.T, _, work string) {
			mkdir(t, filepath.Join(work, "tree"))
		}, "exists"},
		{"tree is a link out of the work volume", func(t *testing.T, _, work string) {
			symlink(t, "..", filepath.Join(work, "tree"))
		}, "exists"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			src, work := filepath.Join(base, "src"), filepath.Join(base, "work")
			mkdir(t, src)
			mkdir(t, work)
			if err := os.WriteFile(filepath.Join(src, "a"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, src, work)
			err := Seed(src, work)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Seed = %v, want ErrRefused naming %q", err, tc.want)
			}
			for _, name := range []string{"a", "l", "tree", "bin", "scratch"} {
				if _, err := os.Lstat(filepath.Join(base, name)); err == nil {
					t.Errorf("%s was written outside the work volume", name)
				}
			}
		})
	}
}

// TestSeedBinAndScratchMayExist: mkdir -p for bin and scratch.
func TestSeedBinAndScratchMayExist(t *testing.T) {
	work := t.TempDir()
	mkdir(t, filepath.Join(work, "bin"))
	if err := Seed(pristine(t), work); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, to, at string) {
	t.Helper()
	if err := os.Symlink(to, at); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
