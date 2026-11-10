// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build linux

package previewtools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// serveReady serves toolexec.ReadyPath on a unix socket at path.
func serveReady(t *testing.T, path string) {
	t.Helper()
	ln, err := toolexec.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(toolexec.ReadyPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc(toolexec.ExecPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(toolexec.Result{Output: "shim\n"})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// TestSandboxSocketCrossingRefused (review SEC-1): head code can replace
// its own exec socket (a 0777 emptyDir it writes) with a symlink to the
// base's, which the relay's mount namespace resolves. A head client must
// never reach the base's shim: neither through a symlink, nor through a
// socket served by any UID but its side's.
func TestSandboxSocketCrossingRefused(t *testing.T) {
	dir, err := os.MkdirTemp("", "pt-peer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, side := range []toolwire.Side{toolwire.SideHead, toolwire.SideBase} {
		if err := os.Mkdir(filepath.Join(dir, string(side)), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	baseSock := filepath.Join(dir, string(toolwire.SideBase), "sock")
	serveReady(t, baseSock)
	if err := os.Symlink(baseSock, filepath.Join(dir, string(toolwire.SideHead), "sock")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid := os.Getuid()

	head, base := NewSandboxes(dir, uid, uid)
	if err := base.Ready(ctx); err != nil {
		t.Fatalf("the base's own shim, served by its UID, refused: %v", err)
	}
	if err := head.Ready(ctx); err == nil {
		t.Fatal("the head's client reached the base's shim through a symlinked socket")
	}

	// A socket served by another UID than the side's: the test serves as
	// uid, so a client expecting uid+1 sees a foreign peer.
	_, foreign := NewSandboxes(dir, uid, uid+1)
	if err := foreign.Ready(ctx); err == nil {
		t.Fatal("a shim served by a foreign UID was accepted")
	}
}

// TestSlotSandboxCrossingRefused (SEC-1 follow-up): a slot tool pod's exec
// socket is a volume the sandbox writes, so the relay's client refuses a
// socket that is a link or is served by another UID than the sandbox's.
// Either is the relay's "unavailable" answer, never the shim's result.
func TestSlotSandboxCrossingRefused(t *testing.T) {
	dir, err := os.MkdirTemp("", "pt-slot")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	serveReady(t, sock)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	for _, tc := range []struct {
		name   string
		socket string
		uid    int
		ok     bool
	}{
		{"its own socket, the sandbox's UID", sock, uid, true},
		{"a symlink to a socket", link, uid, false},
		{"a socket of another UID", sock, uid + 1, false},
	} {
		ex := NewSlotSandbox(tc.socket, tc.uid)
		h := newHarness(t, baseConfig(), routes{}, func(o *Options) { o.Exec = ex })
		_, resp := h.call(toolwire.VerbExec, `{"argv":["ls"]}`)
		if tc.ok {
			if resp.Refused != "" || resp.Observed == nil || resp.Observed.Exit == nil || *resp.Observed.Exit != 0 {
				t.Errorf("%s: %+v, want the shim's exit 0", tc.name, resp)
			}
			continue
		}
		if resp.Refused != toolwire.RefusedUnavailable || resp.Observed != nil {
			t.Errorf("%s: %+v, want refused %q and nothing observed", tc.name, resp, toolwire.RefusedUnavailable)
		}
	}
}
