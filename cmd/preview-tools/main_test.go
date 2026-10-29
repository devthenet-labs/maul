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
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

const check = `{"id":"health.body","outcome":"health","form":"http","http":{"target":"api","steps":[
 {"request":{"method":"POST","path":"/api/echo","json":{"n":"{{run}}"}},"expect":{"bodyContains":"{{run}}"}}]}}`

// seamsFor writes a replay round's config against srv and returns the
// seams that read it.
func seamsFor(t *testing.T, srv *httptest.Server, mut func(*previewtools.Config)) previewtools.Seams {
	t.Helper()
	c, err := checks.Parse([]byte(check))
	if err != nil {
		t.Fatal(err)
	}
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	cfg := previewtools.Config{
		Components:   []previewtools.Component{{Key: "api", Path: "/", Addr: "127.0.0.1:" + port}},
		Viewer:       "maul-evaluator",
		Checks:       []previewtools.StoredCheck{{ID: c.ID, Digest: checks.Digest(c), Body: string(checks.Canonical(c))}},
		MaxToolCalls: 50, TimeoutSeconds: 60, ReplayOnly: true,
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

func TestRunReplayRound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if len(l.Entries) != 3 || l.Entries[1].CheckRun == nil || l.Entries[1].CheckRun.Result != toolwire.RunPass {
		t.Errorf("ledger %+v, want readiness, a passing replay and finish", l.Entries)
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
