// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestPushedAt: the newest push or force push that moved the branch to the
// commit gives its time; a branch creation, another commit or none is not
// found; the branch, the order and the page size are asked of GitHub.
func TestPushedAt(t *testing.T) {
	ref := `"ref":"refs/heads/` + intentBranch + `"`
	page := `[
	  {"id":4,"after":"bbb",` + ref + `,"timestamp":"2026-10-29T21:35:00Z","activity_type":"push"},
	  {"id":3,"after":"aaa",` + ref + `,"timestamp":"2026-10-29T21:30:50Z","activity_type":"force_push"},
	  {"id":2,"after":"aaa",` + ref + `,"timestamp":"2026-10-29T21:00:00Z","activity_type":"push"},
	  {"id":1,"after":"ccc",` + ref + `,"timestamp":"2026-10-29T20:00:00Z","activity_type":"branch_creation"}
	]`
	tests := []struct {
		sha    string
		want   time.Time
		wantOK bool
	}{
		{sha: "aaa", want: time.Date(2026, 10, 29, 21, 30, 50, 0, time.UTC), wantOK: true},
		{sha: "bbb", want: time.Date(2026, 10, 29, 21, 35, 0, 0, time.UTC), wantOK: true},
		{sha: "ccc"},
		{sha: "ddd"},
	}
	for _, tt := range tests {
		t.Run(tt.sha, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/acme/app/activity", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("ref") != "refs/heads/"+intentBranch || q.Get("direction") != "desc" ||
					q.Get("per_page") != "100" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				writeJSON(t, w, page)
			})
			at, ok, err := c.PushedAt(context.Background(), Repo{Owner: "acme", Name: "app"}, intentBranch, tt.sha)
			if err != nil || ok != tt.wantOK || !at.Equal(tt.want) {
				t.Fatalf("PushedAt(%s) = %v, %v, %v; want %v, %v", tt.sha, at, ok, err, tt.want, tt.wantOK)
			}
		})
	}
}

// TestPushedAtError: GitHub's refusal is an error, never "not found".
func TestPushedAtError(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/acme/app/activity", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})
	_, ok, err := c.PushedAt(context.Background(), Repo{Owner: "acme", Name: "app"}, intentBranch, "aaa")
	if err == nil || ok || !IsForbidden(err) {
		t.Fatalf("PushedAt = %v, %v; want a forbidden error", ok, err)
	}
}
