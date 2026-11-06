// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// seedFixture is a pristine tree per side and an empty work volume.
func seedFixture(t *testing.T) (trees, work string) {
	t.Helper()
	dir := t.TempDir()
	trees, work = filepath.Join(dir, "trees"), filepath.Join(dir, "work")
	for _, side := range []string{"head", "base"} {
		if err := os.MkdirAll(filepath.Join(trees, side), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(trees, side, "go.mod"), []byte("module "+side+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return trees, work
}

func TestSeedSide(t *testing.T) {
	for side, uid := range sideUIDs {
		t.Run(side, func(t *testing.T) {
			trees, work := seedFixture(t)
			d := seedDeps{trees: trees, work: work, getuid: func() int { return uid }}
			if code := runSeed(context.Background(), []string{"--side=" + side}, d, io.Discard); code != 0 {
				t.Fatalf("seed = %d", code)
			}
			raw, err := os.ReadFile(filepath.Join(work, "tree", "go.mod"))
			if err != nil || string(raw) != "module "+side+"\n" {
				t.Errorf("tree/go.mod = %q, %v; want %s's", raw, err, side)
			}
			for _, d := range []string{"bin", "scratch"} {
				if info, err := os.Stat(filepath.Join(work, d)); err != nil || !info.IsDir() {
					t.Errorf("%s: %v", d, err)
				}
			}
		})
	}
}

// TestSeedRefusesAnotherUID: a seed runs only as its own side's sandbox,
// so the copy is that sandbox's and never the other's.
func TestSeedRefusesAnotherUID(t *testing.T) {
	tests := []struct {
		side string
		uid  int
	}{
		{"head", sideUIDs["base"]},
		{"base", sideUIDs["head"]},
		{"head", 65532},
		{"base", 0},
	}
	for _, tc := range tests {
		trees, work := seedFixture(t)
		d := seedDeps{trees: trees, work: work, getuid: func() int { return tc.uid }}
		if code := runSeed(context.Background(), []string{"--side=" + tc.side}, d, io.Discard); code != 2 {
			t.Errorf("seed --side=%s as %d = %d, want 2", tc.side, tc.uid, code)
		}
		if left, _ := os.ReadDir(work); len(left) != 0 {
			t.Errorf("seed --side=%s as %d wrote %d entries", tc.side, tc.uid, len(left))
		}
	}
}

func TestSeedFlagRefusals(t *testing.T) {
	for name, args := range map[string][]string{
		"no side":      nil,
		"unknown side": {"--side=main"},
		"two sides":    {"--side=head", "--side=base"},
		"positional":   {"--side=head", "extra"},
		"a path flag":  {"--side=head", "--trees=/"},
	} {
		t.Run(name, func(t *testing.T) {
			trees, work := seedFixture(t)
			d := seedDeps{trees: trees, work: work, getuid: func() int { return sideUIDs["head"] }}
			if code := runSeed(context.Background(), args, d, io.Discard); code != 2 {
				t.Errorf("seed %v = %d, want 2", args, code)
			}
		})
	}
}

// TestSeedHygiene: a pristine tree that escapes is refused, exit 1.
func TestSeedHygiene(t *testing.T) {
	trees, work := seedFixture(t)
	if err := os.Symlink("../../outside", filepath.Join(trees, "head", "l")); err != nil {
		t.Fatal(err)
	}
	d := seedDeps{trees: trees, work: work, getuid: func() int { return sideUIDs["head"] }}
	if code := runSeed(context.Background(), []string{"--side=head"}, d, io.Discard); code != 1 {
		t.Errorf("seed = %d, want 1", code)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	if code := dispatch(context.Background(), []string{"unpack"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("dispatch(unpack) = %d, want 2", code)
	}
}
