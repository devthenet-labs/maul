// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// serve starts s on a socket under a short temporary directory and returns
// a client of it.
func serve(t *testing.T, s *Server) (*Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "toolexec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(sock), sock
}

// TestSocket: the socket is reachable by another UID of the pod (mode
// 0666), and a stale one left by a previous shim is replaced.
func TestSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "toolexec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over a stale file: %v", err)
	}
	defer func() { _ = ln.Close() }()
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o666 {
		t.Errorf("socket mode %v", fi.Mode())
	}
}

// TestClientExec: the client and the handler round-trip an exec, and the
// handler answers a refused request with its refusal, never a run.
func TestClientExec(t *testing.T) {
	s := newServer(t)
	c, _ := serve(t, s)
	ctx := context.Background()
	if err := c.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	got, err := c.Exec(ctx, &toolwire.ExecRequest{
		Argv:  []string{"cat", "in.json"},
		Files: []toolwire.File{{Name: "in.json", Content: []byte(`{"a":1}`)}},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got.Exit != 0 || got.Output != `{"a":1}` || got.Refused != "" {
		t.Errorf("%+v", got)
	}
	got, err = c.Exec(ctx, &toolwire.ExecRequest{Argv: []string{"true"}, Files: []toolwire.File{{Name: "../x"}}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got.Refused != toolwire.RefusedInvalid {
		t.Errorf("refused %q, want %q", got.Refused, toolwire.RefusedInvalid)
	}
}

// TestHandlerRefuses: anything but POST /v1/exec with a request of the
// wire is refused by status.
func TestHandlerRefuses(t *testing.T) {
	s := newServer(t)
	_, sock := serve(t, s)
	hc := unixHTTP(sock)
	tests := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, ExecPath, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/other", "{}", http.StatusNotFound},
		{http.MethodPost, ExecPath, `{"argv":["true"],"shell":true}`, http.StatusBadRequest},
		{http.MethodPost, ExecPath, "not json", http.StatusBadRequest},
		{http.MethodPost, ExecPath, `{"argv":["` + strings.Repeat("a", toolwire.MaxRequestBytes) + `"]}`,
			http.StatusBadRequest},
		{http.MethodPost, ReadyPath, "", http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		req, err := http.NewRequest(tc.method, "http://sandbox"+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

// TestRunSerialised: one program runs at a time, and a caller that gives
// up waiting is told so without running.
func TestRunSerialised(t *testing.T) {
	s := newServer(t)
	s.timeout = 2 * time.Second
	done := make(chan Result)
	go func() { done <- s.Run(context.Background(), &toolwire.ExecRequest{Argv: []string{"sleep", "1"}}) }()
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := s.Run(ctx, &toolwire.ExecRequest{Argv: []string{"true"}})
	if got.Refused != toolwire.RefusedUnavailable {
		t.Errorf("refused %q, want %q", got.Refused, toolwire.RefusedUnavailable)
	}
	if first := <-done; first.Exit != 0 {
		t.Errorf("first %+v", first)
	}
}

// TestClientNoServer: a client with nothing listening reports an error,
// never a result.
func TestClientNoServer(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "sock"))
	if _, err := c.Exec(context.Background(), &toolwire.ExecRequest{Argv: []string{"true"}}); err == nil {
		t.Error("Exec without a server succeeded")
	}
	if err := c.Ready(context.Background()); err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("Ready: %v", err)
	}
}
