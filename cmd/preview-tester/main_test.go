// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// specFor is a one-criterion spec whose target is the test server, by
// name: localhost resolves without the e2e dial map.
func specFor(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	return fmt.Sprintf(`{"round":1,"attempt":1,"run":"abcdef","budgetSeconds":30,
	  "criteria":[{"id":"health","kind":"http","statement":"healthy","target":"api","request":{"path":"/healthz"}}],
	  "targets":{"api":"127.0.0.1:%s"}}`, port)
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunObserves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	term := filepath.Join(t.TempDir(), "termination-log")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), env(map[string]string{"MAUL_VERIFY_SPEC": specFor(t, srv)}), term, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	obs, sum, err := acceptance.ParseWire(stdout.Bytes())
	if err != nil || len(obs) != 1 || obs[0].ID != "health" {
		t.Fatalf("ParseWire = %+v, %v\n%s", obs, err, stdout.String())
	}
	msg, err := os.ReadFile(term)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := acceptance.ParseSummary(string(msg)); err != nil || !got.Equal(sum) {
		t.Errorf("termination message %q, want %+v", msg, sum)
	}
}

// TestRunFailures: a refused spec observes nothing (exit 2); a summary
// that cannot be repeated as the termination message is a failed run
// (exit 1), never a silent success.
func TestRunFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	for _, tc := range []struct {
		name string
		spec string
		term string
		want int
	}{
		{"no spec", "", "", 2},
		{"refused spec", `{"round":1}`, "", 2},
		{"termination unwritable", specFor(t, srv), filepath.Join(t.TempDir(), "missing", "log"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), env(map[string]string{"MAUL_VERIFY_SPEC": tc.spec}), tc.term, &stdout, &stderr)
			if code != tc.want {
				t.Errorf("exit %d, want %d\n%s", code, tc.want, stderr.String())
			}
			if tc.want == 2 && stdout.Len() != 0 {
				t.Errorf("a refused spec wrote %q", stdout.String())
			}
		})
	}
}

// TestImports: the binary links the standard library, the acceptance core
// and its engine, nothing else.
func TestImports(t *testing.T) {
	allowed := []string{
		"github.com/devthenet-labs/maul/internal/acceptance",
		"github.com/devthenet-labs/maul/internal/previewtester",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && !slices.Contains(allowed, path) {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}
