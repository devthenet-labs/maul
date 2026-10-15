// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"testing"
)

func TestFileContent(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/contents/go.mod", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("ref") {
		case "abc":
			// "module x\n" in base64.
			writeJSON(t, w, `{"type":"file","encoding":"base64","content":"bW9kdWxlIHgK"}`)
		case "dir":
			writeJSON(t, w, `[]`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	got, err := c.FileContent(context.Background(), testRepo, "go.mod", "abc")
	if err != nil || string(got) != "module x\n" {
		t.Errorf("FileContent() = %q, %v; want the decoded file", got, err)
	}
	if _, err := c.FileContent(context.Background(), testRepo, "go.mod", "missing"); !IsNotFound(err) {
		t.Errorf("FileContent() of an absent file error = %v, want a 404", err)
	}
	if _, err := c.FileContent(context.Background(), testRepo, "go.mod", "dir"); err == nil || IsNotFound(err) {
		t.Errorf("FileContent() of a directory error = %v, want a non-404 error", err)
	}
}
