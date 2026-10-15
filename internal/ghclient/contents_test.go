// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
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
		case "link":
			writeJSON(t, w, `{"type":"symlink","target":"elsewhere"}`)
		case "big":
			writeJSON(t, w, `{"type":"file","encoding":"none","content":""}`)
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
	for _, ref := range []string{"dir", "link", "big"} {
		if _, err := c.FileContent(context.Background(), testRepo, "go.mod", ref); !errors.Is(err, ErrNotInline) ||
			IsNotFound(err) {
			t.Errorf("FileContent() at %s error = %v, want ErrNotInline", ref, err)
		}
	}
}
