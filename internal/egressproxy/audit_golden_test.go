// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goAuditScript is a fixed run of step 1 requests: forwarded, refused by
// each rule, answered locally and failed upstream.
var goAuditScript = []struct{ profile, method, path string }{
	{"go", http.MethodGet, "/go/github.com/google/uuid/@v/list"},
	{"go", http.MethodHead, "/go/github.com/google/uuid/@v/v1.6.0.info"},
	{"go", http.MethodGet, "/go/github.com/google/uuid/@v/v1.6.0.zip"},
	{"go", http.MethodGet, "/go/rsc.io/quote/@v/v1.5.2.info"},
	{"go", http.MethodGet, "/go/github.com/google/uuid/@v/v0.0.0-20230101000000-abcdefabcdef.info"},
	{"pseudo", http.MethodGet, "/go/github.com/google/uuid/@v/v0.0.0-20230101000000-abcdefabcdef.info"},
	{"go", http.MethodGet, "/go/sumdb/sum.golang.org/supported"},
	{"go", http.MethodGet, "/go/sumdb/sum.golang.org/lookup/github.com/google/uuid@v1.6.0"},
	{"go", http.MethodGet, "/go/sumdb/sum.golang.org/latest"},
	{"go", http.MethodGet, "/go/sumdb/other.example/latest"},
	{"go", http.MethodGet, "/go/github.com/google/uuid/@v/bad"},
	{"go", http.MethodGet, "/elsewhere"},
	{"go", http.MethodGet, "/r/anything/x"},
	{"go", http.MethodPost, "/go/github.com/google/uuid/@v/list"},
	{"go", http.MethodGet, "/go/github.com/google/uuid/@v/v1.5.0.info"},
	{"go", http.MethodGet, "/go/github.com/google/uuid/@v/v1.4.0.info"},
}

// TestGoAuditGolden pins step 1's audit lines for a profile with no
// registry grant byte for byte (time and duration aside): step 2 appends
// its attributes on /r/ routes only. The golden was written by the step-1
// code.
func TestGoAuditGolden(t *testing.T) {
	goUp := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/github.com/google/uuid/@v/v1.5.0.info":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found\n"))
		case "/github.com/google/uuid/@v/v1.4.0.info":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte("v1.6.0\n"))
		}
	})
	sumUp := newUpstream(t, okBody("go.sum lines\n"))
	h := newHarness(t, Config{}, goUp, sumUp)
	var buf lockedBuffer
	drop := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == "duration" {
			return slog.Attr{}
		}
		return a
	}
	srv, err := New(Config{Profiles: h.srv.cfg.Profiles, GoProxy: h.srv.cfg.GoProxy, SumDB: h.srv.cfg.SumDB,
		Policy: h.srv.cfg.Policy}, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: drop})))
	if err != nil {
		t.Fatal(err)
	}
	urls := map[string]string{}
	for _, p := range srv.Profiles().Profiles() {
		handler, _ := srv.Handler(p.Name)
		ts := httptest.NewServer(handler)
		t.Cleanup(ts.Close)
		urls[p.Name] = ts.URL
	}
	h.urls = urls
	for _, step := range goAuditScript {
		if res := h.do(t, step.profile, step.method, step.path, nil); res.err != nil {
			t.Fatal(res.err)
		}
	}
	got := []byte(buf.String())
	path := filepath.Join("testdata", "audit_go.golden.jsonl")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("step 1 audit lines changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}
