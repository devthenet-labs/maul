// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

const wsKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const execCheck = `{"id":"count.prints","outcome":"count","form":"exec","exec":{"argv":["{{bin}}"],
 "expect":{"exit":0,"stdoutContains":"count=3"}}}`

// fakeShim serves the exec shim's endpoints on a unix socket, answering
// every program with output.
type fakeShim struct {
	mu   sync.Mutex
	reqs []toolexec.Request
}

func serveShim(t *testing.T, sock, output string) *fakeShim {
	t.Helper()
	f := &fakeShim{}
	mux := http.NewServeMux()
	mux.HandleFunc(toolexec.ReadyPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc(toolexec.ExecPath, func(w http.ResponseWriter, r *http.Request) {
		var req toolexec.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		res := toolexec.Result{Output: output}
		if req.Kind == toolexec.KindBuild {
			res.Output = "built"
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	ln, err := toolexec.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// workspaceSeams writes a workspace replay round's config and key, two
// pristine trees and two shims, and returns the seams that read them.
func workspaceSeams(t *testing.T, key string) (previewtools.Seams, *fakeShim, *fakeShim) {
	t.Helper()
	c, err := checks.Parse([]byte(execCheck))
	if err != nil {
		t.Fatal(err)
	}
	cfg := previewtools.Config{
		Mode: previewtools.ModeWorkspace,
		Workspace: &previewtools.Workspace{Build: []string{"go", "build", "-o", "/work/bin/bb", "."},
			Binary: "/work/bin/bb", BuildTimeoutSeconds: 60, SearchPath: "/usr/bin"},
		Checks:       []previewtools.StoredCheck{{ID: c.ID, Digest: checks.Digest(c), Body: string(checks.Canonical(c))}},
		MaxToolCalls: 50, TimeoutSeconds: 60, ReplayOnly: true,
	}
	dir := t.TempDir()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, previewtools.ConfigFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		if err := os.WriteFile(filepath.Join(dir, previewtools.KeyFile), []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trees := t.TempDir()
	short, err := os.MkdirTemp("", "ptm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	shims := map[string]*fakeShim{}
	for side, out := range map[string]string{"head": "count=3", "base": "count=2"} {
		if err := os.MkdirAll(filepath.Join(trees, side), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(short, side), 0o755); err != nil {
			t.Fatal(err)
		}
		shims[side] = serveShim(t, filepath.Join(short, side, "sock"), out)
	}
	return previewtools.Seams{ConfigDir: dir, TerminationPath: filepath.Join(t.TempDir(), "termination-log"),
			ListenAddr: "127.0.0.1:0", ExecDir: short, TreesDir: trees, HeadUID: os.Getuid(), BaseUID: os.Getuid()},
		shims["head"], shims["base"]
}

// TestRunWorkspaceRound: a workspace's replay round builds both trees,
// replays the check at each, each over its own socket, and closes the
// ledger. Its side clients read the shims' peer credentials, which only
// linux gives (toolexec.NewSideClient refuses every dial elsewhere).
func TestRunWorkspaceRound(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("side clients read peer credentials on linux only")
	}
	s, head, base := workspaceSeams(t, wsKey+"\n")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), s, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	term, err := os.ReadFile(s.TerminationPath)
	if err != nil {
		t.Fatal(err)
	}
	l, err := toolwire.Verify(stdout.Bytes(), string(term))
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, stdout.String())
	}
	if len(l.Entries) != 5 || l.Entries[0].Verb != toolwire.VerbBuild || l.Entries[1].Side != toolwire.SideBase ||
		l.Entries[2].CheckRun.Result != toolwire.RunPass || l.Entries[3].Side != toolwire.SideBase ||
		l.Entries[3].CheckRun.Result != toolwire.RunFail {
		t.Fatalf("ledger %+v, want two builds, a head and a base replay, and finish", l.Entries)
	}
	if len(head.reqs) != 2 || len(base.reqs) != 3 {
		t.Fatalf("head ran %d, base %d requests", len(head.reqs), len(base.reqs))
	}
}

// TestRunWorkspaceNeedsItsKey: a workspace without its key file, or with
// one that is not a key, does not start.
func TestRunWorkspaceNeedsItsKey(t *testing.T) {
	for _, key := range []string{"", "not-a-key"} {
		s, _, _ := workspaceSeams(t, key)
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), s, &stdout, &stderr); code != 2 {
			t.Fatalf("key %q: exit %d, want 2\n%s", key, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("key %q: wrote a ledger", key)
		}
	}
}

// TestWorkspacePlacesMatchTheToolPod: the relay reads its key, trees and
// sockets where jobs.CreateToolPod mounts them.
func TestWorkspacePlacesMatchTheToolPod(t *testing.T) {
	s, err := previewtools.SeamsFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if previewtools.KeyFile != jobs.ToolsKeyFile || s.TreesDir != "/trees" || s.ExecDir != "/run/maul-exec" ||
		filepath.Base(toolexec.SocketPath) != "sock" || filepath.Dir(toolexec.SocketPath) != s.ExecDir {
		t.Fatalf("relay places %s %s %s, the tool pod's key %s and socket %s", previewtools.KeyFile, s.TreesDir,
			s.ExecDir, jobs.ToolsKeyFile, toolexec.SocketPath)
	}
	if previewtools.WorkTree != "/work/tree" {
		t.Fatalf("work tree %s", previewtools.WorkTree)
	}
}
