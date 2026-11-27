// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// seamsFor writes a round's config against srv, with a one-second wall
// clock, and returns the seams that read it.
func seamsFor(t *testing.T, srv *httptest.Server, mut func(*previewtools.Config)) previewtools.Seams {
	t.Helper()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	cfg := previewtools.Config{
		Components:   []previewtools.Component{{Key: "api", Path: "/", Addr: "127.0.0.1:" + port}},
		Viewer:       "maul-evaluator",
		MaxToolCalls: 50, TimeoutSeconds: 1,
	}
	if mut != nil {
		mut(&cfg)
	}
	dir := t.TempDir()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, previewtools.ConfigFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return previewtools.Seams{ConfigDir: dir, TerminationPath: filepath.Join(t.TempDir(), "termination-log"),
		ListenAddr: "127.0.0.1:0"}
}

// TestRunServesUntilItsWallClock: the binary waits for its component,
// serves until its wall clock, and closes the ledger on stdout, verified
// by the termination message; it runs nothing of its own, so the component
// sees no request.
func TestRunServesUntilItsWallClock(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var in struct{ N string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, _ = fmt.Fprintf(w, "echo %s", in.N)
	}))
	defer srv.Close()
	s := seamsFor(t, srv, nil)
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
	if len(l.Entries) != 2 || l.Entries[0].Verb != toolwire.VerbHTTP || l.Entries[1].Verb != toolwire.VerbFinish {
		t.Errorf("ledger %+v, want readiness and finish", l.Entries)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the component saw %d requests; the relay ran something of its own", n)
	}
	if strings.Contains(stderr.String(), "MAUL-TOOLS") {
		t.Errorf("the ledger leaked onto stderr")
	}
}

func TestRunRefusesABadConfig(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	s := seamsFor(t, srv, func(c *previewtools.Config) { c.MaxToolCalls = 1 })
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), s, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused config wrote a ledger: %q", stdout.String())
	}
}
