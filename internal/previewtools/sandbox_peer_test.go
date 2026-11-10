// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
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
