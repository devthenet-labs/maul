// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

// TestListBlockedByConditional walks the blocked-by listing: pages followed,
// repository_url parsed into the blocker's repository, state_reason kept as
// GitHub sent it (an unknown value included), a repeat with the tag a 304.
func TestListBlockedByConditional(t *testing.T) {
	mux, c := newFakeClient(t)
	current := `W/"d1"`
	var sent []string
	mux.HandleFunc("GET /repos/o/r/issues/7/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		inm := r.Header.Get("If-None-Match")
		page := r.URL.Query().Get("page")
		if page == "1" {
			sent = append(sent, inm)
		}
		w.Header().Set("ETag", current)
		if page == "1" && inm == current {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		switch page {
		case "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s?page=2>; rel="next"`, r.URL.Path))
			writeJSON(t, w, `[
			{"number":3,"state":"open","html_url":"https://github.com/o/r/issues/3",
			 "repository_url":"https://api.github.com/repos/o/r"},
			{"number":4,"state":"closed","state_reason":"not_planned","html_url":"https://github.com/o/r/issues/4",
			 "repository_url":"https://api.github.com/repos/o/r"}]`)
		default:
			writeJSON(t, w, `[
			{"number":9,"state":"closed","state_reason":"something_new",
			 "html_url":"https://github.com/other/lib/issues/9",
			 "repository_url":"https://api.github.com/repos/other/lib"},
			{"number":10,"state":"closed","html_url":"https://github.com/o/r/issues/10",
			 "repository_url":"https://api.github.com/repos/o/r"}]`)
		}
	})

	ctx := context.Background()
	first, err := c.ListBlockedBy(ctx, testRepo, 7, "")
	if err != nil {
		t.Fatalf("ListBlockedBy() error = %v", err)
	}
	want := []Blocker{
		{Repo: Repo{Owner: "o", Name: "r"}, Number: 3, State: "open", HTMLURL: "https://github.com/o/r/issues/3"},
		{Repo: Repo{Owner: "o", Name: "r"}, Number: 4, State: "closed", StateReason: "not_planned",
			HTMLURL: "https://github.com/o/r/issues/4"},
		{Repo: Repo{Owner: "other", Name: "lib"}, Number: 9, State: "closed", StateReason: "something_new",
			HTMLURL: "https://github.com/other/lib/issues/9"},
		{Repo: Repo{Owner: "o", Name: "r"}, Number: 10, State: "closed", HTMLURL: "https://github.com/o/r/issues/10"},
	}
	if first.NotModified || first.ETag != `W/"d1"` || !reflect.DeepEqual(first.Blockers, want) {
		t.Errorf("ListBlockedBy() = %+v, want %+v tagged W/\"d1\"", first, want)
	}
	again, err := c.ListBlockedBy(ctx, testRepo, 7, first.ETag)
	if err != nil {
		t.Fatalf("conditional ListBlockedBy() error = %v", err)
	}
	if !again.NotModified || again.Blockers != nil || again.ETag != `W/"d1"` {
		t.Errorf("conditional ListBlockedBy() = %+v, want NotModified with the same tag", again)
	}
	if wantSent := []string{"", `W/"d1"`}; !reflect.DeepEqual(sent, wantSent) {
		t.Errorf("If-None-Match sent = %q, want %q", sent, wantSent)
	}
}

// TestListBlockedByErrors keeps GitHub's answer classifiable by the error
// predicates the intent controller decides on.
func TestListBlockedByErrors(t *testing.T) {
	unclassified := func(err error) bool {
		return err != nil && !IsNotFound(err) && !IsGone(err) && !IsForbidden(err)
	}
	for _, tc := range []struct {
		label   string
		status  int
		headers map[string]string
		body    string
		check   func(error) bool
		name    string
	}{
		{"404", http.StatusNotFound, nil, "", IsNotFound, "IsNotFound"},
		{"410", http.StatusGone, nil, "", IsGone, "IsGone"},
		{"403", http.StatusForbidden, nil, "", IsForbidden, "IsForbidden"},
		{"500", http.StatusInternalServerError, nil, "", unclassified, "unclassified"},
		// A rate limit is no refusal: a waiting intent must keep waiting
		// (transient), never start as DependenciesForbidden. The waits are
		// zero so the client's own retries do not slow the test.
		{"403 primary rate limit", http.StatusForbidden, map[string]string{
			"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1",
		}, `{"message":"API rate limit exceeded for installation."}`, unclassified, "unclassified"},
		{"403 secondary rate limit", http.StatusForbidden, map[string]string{"Retry-After": "0"},
			`{"message":"You have exceeded a secondary rate limit.",` +
				`"documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api` +
				`#about-secondary-rate-limits"}`,
			unclassified, "unclassified"},
		// The same throttle without go-github's documentation_url anchor
		// arrives as a plain 403.
		{"403 throttle without anchor", http.StatusForbidden, map[string]string{"Retry-After": "0"},
			`{"message":"Forbidden"}`, unclassified, "unclassified"},
		{"429", http.StatusTooManyRequests, map[string]string{"Retry-After": "0"},
			`{"message":"You have exceeded a secondary rate limit."}`, unclassified, "unclassified"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			mux, c := newFakeClient(t)
			mux.HandleFunc("GET /repos/o/r/issues/7/dependencies/blocked_by", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				body := tc.body
				if body == "" {
					body = `{"message":"no"}`
				}
				_, _ = w.Write([]byte(body))
			})
			_, err := c.ListBlockedBy(context.Background(), testRepo, 7, "")
			if !tc.check(err) {
				t.Errorf("ListBlockedBy() error = %v, want %s", err, tc.name)
			}
		})
	}
}

// TestGetIssueDependencySummary reads issue_dependencies_summary when GitHub
// sends it, and leaves it nil when it does not.
func TestGetIssueDependencySummary(t *testing.T) {
	mux, c := newFakeClient(t)
	mux.HandleFunc("GET /repos/o/r/issues/1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"number":1,"state":"open","issue_dependencies_summary":
			{"blocked_by":2,"blocking":0,"total_blocked_by":3,"total_blocking":1}}`)
	})
	mux.HandleFunc("GET /repos/o/r/issues/2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"number":2,"state":"open"}`)
	})
	ctx := context.Background()
	is, err := c.GetIssue(ctx, testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&DependencySummary{BlockedBy: 2, TotalBlockedBy: 3}); !reflect.DeepEqual(is.Dependencies, want) {
		t.Errorf("Dependencies = %+v, want %+v", is.Dependencies, want)
	}
	is, err = c.GetIssue(ctx, testRepo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if is.Dependencies != nil {
		t.Errorf("Dependencies = %+v, want nil without a summary", is.Dependencies)
	}
}
