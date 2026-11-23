// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestBuildKeepsNothing: a build that built is answered as it ran, with
// nothing kept beside the tree (no copy, no "could not be kept" note), even
// for a tree no copy could make (a fifo); the next exec runs on the tree,
// build output and HOME as the build and anything since left them, with
// the tmp root as its TMPDIR. maul runs no check of its own, so no program
// needs the build's state restored (evaluator observed, W9).
func TestBuildKeepsNothing(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("no mkfifo on PATH")
	}
	tests := []struct{ name, build string }{
		{"a plain build", `echo gen > gen.txt && printf '#!/bin/sh\necho built\n' > ../bin/x && chmod 755 ../bin/x`},
		{"a tree holding a fifo", `mkfifo pipe && printf '#!/bin/sh\necho built\n' > ../bin/x && chmod 755 ../bin/x`},
		{"a build leaving HOME", `echo home > "$HOME/.rc" && printf '#!/bin/sh\necho built\n' > ../bin/x && ` +
			`chmod 755 ../bin/x`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ws := newWorkspace(t)
			env := pathEnv()
			got := s.Do(context.Background(), &Request{Kind: KindBuild,
				Argv: []string{"sh", "-c", tc.build + " && echo done"}, Env: env})
			if got.Exit != 0 || got.Refused != "" || got.Unclean || got.Output != "done\n" {
				t.Fatalf("build: %+v, want exit 0 and only its own output", got)
			}
			if _, err := os.Lstat(filepath.Join(ws.work, "kept")); !os.IsNotExist(err) {
				t.Errorf("the build left a copy beside the tree: %v", err)
			}
			if got := s.Do(context.Background(), &Request{Argv: []string{"sh", "-c",
				`echo changed > ` + filepath.Join(ws.work, "bin", "x")}, Run: "a", Env: env}); got.Exit != 0 {
				t.Fatalf("change: %+v", got)
			}
			got = s.Do(context.Background(), &Request{Argv: []string{"sh", "-c",
				`cat ` + filepath.Join(ws.work, "bin", "x") + ` && printf %s "$TMPDIR"`}, Run: "b", Env: env})
			if got.Exit != 0 || got.Output != "changed\n"+ws.tmp {
				t.Errorf("the next exec: %+v, want the build output as the last program left it and the tmp root",
					got)
			}
		})
	}
}

// TestFreshRetired: a request still asking for a fresh tree is refused at
// the wire, by a workspace shim and a slot shim alike, and runs nothing:
// the shim restores nothing any more, so a caller that wants a restore is
// never served one it did not get.
func TestFreshRetired(t *testing.T) {
	slot := newServer(t)
	ws, dirs := newWorkspace(t)
	if got := ws.Do(context.Background(), &Request{Kind: KindBuild, Argv: []string{"true"},
		Env: pathEnv()}); got.Exit != 0 {
		t.Fatalf("build: %+v", got)
	}
	for _, tc := range []struct {
		name string
		s    *Server
		run  string
	}{{"workspace", ws, `,"run":"r"`}, {"slot", slot, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(dirs.work, "ran-"+tc.name)
			_, sock := serve(t, tc.s)
			body := `{"argv":["touch","` + marker + `"]` + tc.run + `,"fresh":true}`
			req, err := http.NewRequest(http.MethodPost, "http://sandbox"+ExecPath, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := unixHTTP(sock, nil).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var res Result
			if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest || res.Refused != toolwire.RefusedInvalid {
				t.Errorf("status %d, %+v; want 400 refused invalid", resp.StatusCode, res)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the program ran")
			}
		})
	}
}
