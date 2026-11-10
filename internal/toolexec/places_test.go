// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newMountedWorkspace is newWorkspace with HOME, the caches and the tmp
// root in a directory the shim cannot write, as each is a volume's mount
// point in the pod: none of them can be removed or replaced, only what is
// in it.
func newMountedWorkspace(t *testing.T) (*Server, workspace) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	root := t.TempDir()
	writable(t, root)
	mnt := filepath.Join(root, "mnt")
	ws := workspace{
		work:  filepath.Join(root, "work"),
		home:  filepath.Join(mnt, "home"),
		cache: filepath.Join(mnt, "cache"),
		tmp:   filepath.Join(mnt, "tmp"),
		proc:  filepath.Join(root, "proc"),
	}
	for _, d := range []string{filepath.Join(ws.work, "tree"), filepath.Join(ws.work, "bin"), ws.proc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewServer(Options{Work: ws.work, Home: ws.home, Cache: ws.cache, Tmp: ws.tmp, Procfs: ws.proc})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := os.Chmod(mnt, 0o555); err != nil {
		t.Fatal(err)
	}
	return s, ws
}

// placesBuild is a build that leaves state in HOME and both caches: a
// file in HOME, two Go cache entries in one directory and one in another,
// and an npm cache file.
const placesBuild = `echo home > "$HOME/.rc" && mkdir -p "$GOCACHE/ab" "$GOCACHE/cd" "$NPM_CONFIG_CACHE" &&
printf good > "$GOCACHE/ab/entry-d" && printf othr > "$GOCACHE/ab/other-d" && printf kept > "$GOCACHE/cd/keep-d" &&
echo npm > "$NPM_CONFIG_CACHE/index"`

// goodOrAbsent holds the build's cache entry ab/entry-d: gone (a cache
// miss, which the go command builds again) or exactly the build's.
const goodOrAbsent = `{ test -r "$GOCACHE/ab" -o ! -e "$GOCACHE/ab"; } && ` +
	`{ ! test -e "$GOCACHE/ab/entry-d" || test "$(cat "$GOCACHE/ab/entry-d")" = good; }`

// TestCheckRunsShareNoState: a check run (a fresh request) starts with
// HOME as the build left it, the caches holding only what the build left
// there, unchanged, and a TMPDIR of its own, empty, whatever an earlier
// check run wrote in any of them; so no check run's result depends on
// which ran before it. OUT is a directory outside every place.
func TestCheckRunsShareNoState(t *testing.T) {
	tests := []struct{ name, poison, look string }{
		{"a file added to HOME", `echo p > "$HOME/poison"`,
			`test ! -e "$HOME/poison" && test "$(cat "$HOME/.rc")" = home`},
		{"the build's HOME file rewritten", `echo evil > "$HOME/.rc"`, `test "$(cat "$HOME/.rc")" = home`},
		{"HOME made unreadable", `chmod 000 "$HOME"`, `test "$(ls -A "$HOME")" = .rc`},
		{"HOME's file replaced by a link out",
			`rm "$HOME/.rc" && echo evil > "$OUT/rc" && ln -s "$OUT/rc" "$HOME/.rc"`,
			`test ! -L "$HOME/.rc" && test "$(cat "$HOME/.rc")" = home && test "$(cat "$OUT/rc")" = evil`},
		{"a file left in TMPDIR", `echo p > "$TMPDIR/poison"`, `test -d "$TMPDIR" && test -z "$(ls -A "$TMPDIR")"`},
		{"a read-only directory left in TMPDIR",
			`mkdir "$TMPDIR/ro" && touch "$TMPDIR/ro/f" && chmod 555 "$TMPDIR/ro"`,
			`test -d "$TMPDIR" && test -z "$(ls -A "$TMPDIR")"`},
		{"a Go cache entry added", `echo p > "$GOCACHE/ab/new-d"`, `test ! -e "$GOCACHE/ab/new-d"`},
		{"a Go cache directory added", `mkdir "$GOCACHE/ef" && echo p > "$GOCACHE/ef/x-d"`,
			`test ! -e "$GOCACHE/ef"`},
		{"a Go cache entry overwritten in place, its size and times put back",
			`cp -p "$GOCACHE/ab/entry-d" ref && printf evil > "$GOCACHE/ab/entry-d" && ` +
				`touch -r ref "$GOCACHE/ab/entry-d"`,
			goodOrAbsent},
		{"a Go cache entry renamed over another", `mv "$GOCACHE/ab/other-d" "$GOCACHE/ab/entry-d"`, goodOrAbsent},
		{"a Go cache entry hard-linked over another", `ln -f "$GOCACHE/ab/other-d" "$GOCACHE/ab/entry-d"`,
			goodOrAbsent},
		{"a Go cache entry's mode changed", `chmod 777 "$GOCACHE/ab/entry-d"`,
			`! test -e "$GOCACHE/ab/entry-d" || test "$(stat -c %a "$GOCACHE/ab/entry-d")" != 777`},
		{"a Go cache directory made unreadable", `chmod 000 "$GOCACHE/ab"`, goodOrAbsent},
		{"a Go cache directory replaced by a link out",
			`rm -rf "$GOCACHE/ab" && printf evil > "$OUT/entry-d" && ln -s "$OUT" "$GOCACHE/ab"`,
			`test ! -L "$GOCACHE/ab" && ` + goodOrAbsent + ` && test "$(cat "$OUT/entry-d")" = evil`},
		{"a Go cache entry deleted", `rm "$GOCACHE/ab/entry-d"`, `test ! -e "$GOCACHE/ab/entry-d"`},
		{"the Go cache replaced by a link out", `rm -rf "$GOCACHE" && ln -s "$OUT" "$GOCACHE"`,
			`test ! -L "$GOCACHE"`},
		{"a file added to the npm cache", `echo p > "$NPM_CONFIG_CACHE/poison"`,
			`test ! -e "$NPM_CONFIG_CACHE/poison" && test "$(cat "$NPM_CONFIG_CACHE/index")" = npm`},
	}
	// lost are the rows whose poisoning run removed the whole Go cache.
	lost := map[string]bool{"the Go cache replaced by a link out": true}
	for _, mounted := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("%s mounted=%v", tc.name, mounted), func(t *testing.T) {
				s, ws := newWorkspace(t)
				if mounted {
					s, ws = newMountedWorkspace(t)
				}
				for _, d := range []string{ws.work, ws.home, ws.cache, ws.tmp} {
					writable(t, d)
				}
				out := t.TempDir()
				writable(t, out)
				env := append(pathEnv(), EnvVar{Name: "LANG", Value: "C"})
				sh := func(run, script string) Result {
					t.Helper()
					return s.Do(context.Background(), &Request{Argv: []string{"sh", "-c",
						"OUT=" + out + "; " + script}, Run: run, Env: env, Fresh: true})
				}
				if got := s.Do(context.Background(), &Request{Kind: KindBuild,
					Argv: []string{"sh", "-c", placesBuild}, Env: env}); got.Exit != 0 || got.KeepFailed {
					t.Fatalf("build: %+v", got)
				}
				keep := filepath.Join(ws.cache, "go-build", "cd", "keep-d")
				before, err := os.Stat(keep)
				if err != nil {
					t.Fatal(err)
				}
				if got := sh("a", tc.poison); got.Refused != "" || got.Exit != 0 {
					t.Fatalf("the poisoning check run: %+v", got)
				}
				if got := sh("b", tc.look); got.Refused != "" || got.Exit != 0 {
					t.Fatalf("the next check run saw the earlier one's state: %+v", got)
				}
				// An entry no check run touched is the build's own file: the
				// cache is pruned, never emptied.
				if after, err := os.Stat(keep); !lost[tc.name] && (err != nil || !os.SameFile(before, after)) {
					t.Errorf("an untouched cache entry was not kept as the build left it: %v", err)
				}
			})
		}
	}
}

// TestCheckRunTmpDir: a check run's TMPDIR is its own, shared by its later
// requests (a run check's compile, then its program); an exploratory
// request keeps the tmp root, as before.
func TestCheckRunTmpDir(t *testing.T) {
	s, ws := newWorkspace(t)
	writable(t, ws.tmp)
	env := pathEnv()
	do := func(run, script string, fresh bool) string {
		t.Helper()
		got := s.Do(context.Background(), &Request{Argv: []string{"sh", "-c", script}, Run: run, Env: env,
			Fresh: fresh})
		if got.Refused != "" || got.Exit != 0 {
			t.Fatalf("run %s: %+v", run, got)
		}
		return got.Output
	}
	if got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"true"},
		Env: env}); got.Exit != 0 {
		t.Fatalf("build: %+v", got)
	}
	first := do("c1", `echo x > "$TMPDIR/x" && printf %s "$TMPDIR"`, true)
	if first == ws.tmp || !within(first, ws.tmp) {
		t.Fatalf("a check run's TMPDIR %q is not its own under %s", first, ws.tmp)
	}
	if got := do("c1", `test -e "$TMPDIR/x" && printf %s "$TMPDIR"`, false); got != first {
		t.Errorf("the check run's later request has TMPDIR %q, not its own %q", got, first)
	}
	if got := do("e1", `printf %s "$TMPDIR"`, false); got != ws.tmp {
		t.Errorf("an exploratory request has TMPDIR %q, not the tmp root", got)
	}
	if got := do("c2", `test ! -e "$TMPDIR/x" && printf %s "$TMPDIR"`, true); got == ws.tmp {
		t.Error("the next check run has the tmp root as TMPDIR")
	}
}

// TestKeptCopyBounded: a build whose tree, output and HOME together are
// more than the kept copy may hold is not kept (KeepFailed), and no copy
// is left; one within the bound is.
func TestKeptCopyBounded(t *testing.T) {
	tests := []struct {
		name       string
		bound      int64
		keepFailed bool
	}{
		{"within the bound", 1 << 20, false},
		{"past the bound", 16 << 10, true},
	}
	const build = `head -c 8192 /dev/zero > big && head -c 8192 /dev/zero > ../bin/big && ` +
		`head -c 8192 /dev/zero > "$HOME/big"`
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ws := newWorkspace(t)
			writable(t, ws.work)
			s.keptBound = tc.bound
			got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", build},
				Env: pathEnv()})
			if got.Exit != 0 || got.KeepFailed != tc.keepFailed {
				t.Fatalf("keep failed %v, want %v: %+v", got.KeepFailed, tc.keepFailed, got)
			}
			if _, err := os.Stat(filepath.Join(ws.work, keptDir)); tc.keepFailed && !os.IsNotExist(err) {
				t.Errorf("a copy past the bound was left: %v", err)
			}
		})
	}
}
