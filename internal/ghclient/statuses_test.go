// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCreateCommitStatus: the status is posted to the commit as the fields
// maul sets, and the id GitHub gives it comes back, which is how a later
// pass tells maul's own status from an impostor's.
func TestCreateCommitStatus(t *testing.T) {
	mux, c := newFakeClient(t)
	type body struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	var got body
	mux.HandleFunc("POST /repos/o/r/statuses/abc", func(w http.ResponseWriter, r *http.Request) {
		got = decodeBody[body](t, r)
		writeJSON(t, w, `{"id":4242,"state":"pending","context":"maul/acceptance"}`)
	})
	id, err := c.CreateCommitStatus(context.Background(), testRepo, "abc", "pending", "maul/acceptance",
		"verifying: round 1 of 2", "https://maul.example/intents/x")
	if err != nil || id != 4242 {
		t.Fatalf("CreateCommitStatus() = %d, %v; want 4242", id, err)
	}
	want := body{"pending", "maul/acceptance", "verifying: round 1 of 2", "https://maul.example/intents/x"}
	if got != want {
		t.Errorf("posted %+v, want %+v", got, want)
	}
}

// TestCreateCommitStatusRefuses: a state GitHub does not know, an empty
// context or sha, or a description over GitHub's 140-character limit never
// reaches the API.
func TestCreateCommitStatusRefuses(t *testing.T) {
	_, c := newFakeClient(t) // no handler: a request would 404 with a different error
	ctx := context.Background()
	for _, tt := range []struct {
		name, sha, state, context, desc, want string
	}{
		{"bad state", "abc", "done", "maul/acceptance", "", "state"},
		{"no context", "abc", "success", "", "", "context"},
		{"no sha", "", "success", "maul/acceptance", "", "sha"},
		{"long description", "abc", "success", "maul/acceptance", strings.Repeat("x", 141), "description"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.CreateCommitStatus(ctx, testRepo, tt.sha, tt.state, tt.context, tt.desc, "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}

// TestCreateCommitStatusFailure wraps the API's refusal with the commit.
func TestCreateCommitStatusFailure(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("POST /repos/o/r/statuses/abc", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})
	_, err := c.CreateCommitStatus(context.Background(), testRepo, "abc", "success", "maul/acceptance", "", "")
	if err == nil || !strings.Contains(err.Error(), "ghclient: create status on o/r@abc") {
		t.Errorf("error = %v", err)
	}
}

// TestListCommitStatusesCreator: a listed status carries its id, the login
// that created it and when, which is what the impostor check reads.
func TestListCommitStatusesCreator(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/commits/abc/statuses", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `[{"id":9,"context":"maul/acceptance","state":"success","description":"d",`+
			`"created_at":"2026-05-01T10:00:00Z","creator":{"login":"mallory"}},{"id":8,"context":"x","state":"error"}]`)
	})
	got, err := c.ListCommitStatuses(context.Background(), testRepo, "abc")
	if err != nil || len(got) != 2 {
		t.Fatalf("statuses = %+v, %v", got, err)
	}
	want := CommitStatus{ID: 9, Context: "maul/acceptance", State: "success", Description: "d",
		Creator: "mallory", CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	if got[0] != want {
		t.Errorf("status = %+v, want %+v", got[0], want)
	}
	if got[1].Creator != "" || !got[1].CreatedAt.IsZero() {
		t.Errorf("status without a creator = %+v", got[1])
	}
}
