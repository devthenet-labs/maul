// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// freshBuild is a build that leaves state in both kept places: a generated
// file and a read-only directory in the tree, an executable and a symlink
// in the build output.
const freshBuild = `echo gen > gen.txt && mkdir ro && echo kept > ro/f && chmod 555 ro &&
printf '#!/bin/sh\necho built\n' > ../bin/x && chmod 755 ../bin/x && ln -s x ../bin/link`

// freshLook prints what a check run would find of the build's state.
const freshLook = `cd "$W" && cat tree/gen.txt tree/ro/f && ./bin/link && ls -A tree bin && ` +
	`stat -c '%a' bin/x tree/ro`

// freshWant is freshLook's output on the build's state.
const freshWant = "gen\nkept\nbuilt\nbin:\nlink\nx\n\ntree:\ngen.txt\nro\n755\n555\n"

// writable makes everything under dir writable again at the test's end, so
// the read-only directories a build leaves are removed with it.
func writable(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+rwx", dir).Run() })
}

// TestWorkspaceFresh (review VC-2): a fresh request runs on the tree and
// build output exactly as the last build left them, whatever an earlier
// program changed there; a plain exec sees the change.
func TestWorkspaceFresh(t *testing.T) {
	tests := []struct{ name, mutate string }{
		{"a file rewritten", `echo changed > tree/gen.txt`},
		{"a file added", `echo new > tree/added.go`},
		{"the binary replaced", `printf '#!/bin/sh\necho patched\n' > bin/x`},
		{"the binary removed", `rm bin/x`},
		{"a mode changed", `chmod 700 bin/x`},
		{"a link repointed", `ln -sfn /bin/true bin/link`},
		{"a read-only directory emptied", `chmod 755 tree/ro && rm tree/ro/f`},
		{"the tree made unreadable", `chmod 000 tree`},
		{"the tree replaced by a link", `mv tree tree.old && ln -s tree.old tree && echo x > tree.old/gen.txt`},
		{"the build output removed", `rm -rf bin`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ws := newWorkspace(t)
			writable(t, ws.work)
			env := append(pathEnv(), EnvVar{Name: "LANG", Value: "C"})
			look := func(fresh bool) Result {
				t.Helper()
				return s.Do(context.Background(), &Request{Argv: []string{"sh", "-c",
					"W=" + ws.work + "; " + freshLook}, Run: "look", Env: env, Fresh: fresh})
			}
			if got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", freshBuild},
				Env: env}); got.Exit != 0 {
				t.Fatalf("build: %+v", got)
			}
			if got := s.Do(context.Background(), &Request{Argv: []string{"sh", "-c", "cd " + ws.work + " && " +
				tc.mutate}, Run: "mutate", Env: env}); got.Exit != 0 {
				t.Fatalf("mutate: %+v", got)
			}
			if got := look(false); got.Output == freshWant {
				t.Fatalf("a plain exec did not see the change: %q", got.Output)
			}
			if got := look(true); got.Refused != "" || got.Exit != 0 || got.Output != freshWant {
				t.Fatalf("a fresh run: %+v", got)
			}
			// A second fresh run finds the build's state as it is.
			if got := look(true); got.Output != freshWant {
				t.Fatalf("a second fresh run: %+v", got)
			}
		})
	}
}

// TestWorkspaceFreshRefused: with no build's state to start from, or none
// it can restore exactly, a fresh request runs nothing.
func TestWorkspaceFreshRefused(t *testing.T) {
	tests := []struct {
		name   string
		build  string
		mutate string
	}{
		{"no build ran", "", ""},
		{"the build failed", "echo gen > gen.txt; exit 1", ""},
		// A program that rewrites every copy it can reach of a file.
		{"every copy changed", freshBuild, `find . -name gen.txt -exec sh -c 'echo forged > "$1"' _ {} \;`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ws := newWorkspace(t)
			writable(t, ws.work)
			env := pathEnv()
			if tc.build != "" {
				s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", tc.build}, Env: env})
			}
			if tc.mutate != "" {
				if got := s.Do(context.Background(), &Request{Argv: []string{"sh", "-c", "cd " + ws.work + " && " +
					tc.mutate}, Run: "mutate", Env: env}); got.Exit != 0 {
					t.Fatalf("mutate: %+v", got)
				}
			}
			marker := filepath.Join(ws.work, "ran")
			got := s.Do(context.Background(), &Request{Argv: []string{"touch", marker}, Run: "r", Env: env,
				Fresh: true})
			if got.Refused != toolwire.RefusedUnavailable {
				t.Fatalf("not refused: %+v", got)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the program ran")
			}
		})
	}
}

// TestWorkspaceKeepFailed (VC-2 follow-up): a build that built but whose
// tree and output could not be kept says so in its Result, beside its
// output's note, so the relay can tell maul's trouble from the build's;
// a build whose copy was kept does not.
func TestWorkspaceKeepFailed(t *testing.T) {
	tests := []struct {
		name, build string
		keepFailed  bool
	}{
		{"kept", "echo gen > gen.txt", false},
		// The copy refuses a fifo (errSpecial), as it fails on ENOSPC.
		{"a fifo in the tree", "mkfifo pipe", true},
		{"the build failed", "mkfifo pipe; exit 1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ws := newWorkspace(t)
			writable(t, ws.work)
			got := s.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"sh", "-c", tc.build},
				Env: pathEnv()})
			if got.KeepFailed != tc.keepFailed {
				t.Fatalf("keep failed %v, want %v: %+v", got.KeepFailed, tc.keepFailed, got)
			}
		})
	}
}
