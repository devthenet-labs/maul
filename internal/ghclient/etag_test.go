// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// taggedServer answers every request the way GitHub does: the body set for
// its path (bodies), a weak ETag over it, and a bodiless 304 when
// If-None-Match names that tag. It records each request's If-None-Match and
// counts what GitHub would bill: every answer but a 304.
type taggedServer struct {
	mu        sync.Mutex
	bodies    map[string]string
	status    map[string]int
	link      map[string]string
	remaining int
	billed    int
	asked     []string // If-None-Match of each request, "" when none
}

func newTaggedServer(t *testing.T) (*taggedServer, *httptest.Server) {
	t.Helper()
	s := &taggedServer{bodies: map[string]string{}, status: map[string]int{}, link: map[string]string{},
		remaining: 5000}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *taggedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	s.asked = append(s.asked, r.Header.Get("If-None-Match"))
	s.remaining--
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(s.remaining))
	if code := s.status[path]; code != 0 {
		s.billed++
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"no"}`))
		return
	}
	body := s.bodies[path]
	sum := sha256.Sum256([]byte(body))
	tag := `W/"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		s.remaining++ // a 304 costs nothing
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(s.remaining))
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.billed++
	if l := s.link[path]; l != "" {
		w.Header().Set("Link", l)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (s *taggedServer) set(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[path] = body
}

// lastAsked is the If-None-Match of the latest request.
func (s *taggedServer) lastAsked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[len(s.asked)-1]
}

func (s *taggedServer) billedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.billed
}

// etagGet GETs url through a client of token on cache, returning the
// status, body and remaining-budget header the caller sees.
func etagGet(t *testing.T, cache *ETagCache, token, rawURL string, header ...string) (int, string, string) {
	t.Helper()
	c := &http.Client{Transport: &etagTransport{next: http.DefaultTransport, cache: cache}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b), resp.Header.Get("X-RateLimit-Remaining")
}

// TestETagUnchangedAnswerIsServedFrom304: a second GET of an unchanged
// resource is asked conditionally, GitHub's 304 costs nothing, and the
// caller sees the kept body as a 200 with the 304's fresh rate figures. A
// changed resource is read anew at once.
func TestETagUnchangedAnswerIsServedFrom304(t *testing.T) {
	s, srv := newTaggedServer(t)
	s.set("/repos/o/r/issues/1", `{"number":1,"state":"open"}`)
	cache := NewETagCache(1 << 20)
	u := srv.URL + "/repos/o/r/issues/1"

	code, body, _ := etagGet(t, cache, "tok", u)
	if code != http.StatusOK || body != `{"number":1,"state":"open"}` || s.lastAsked() != "" {
		t.Fatalf("first GET: %d %q, asked %q", code, body, s.lastAsked())
	}
	for range 5 {
		code, body, remaining := etagGet(t, cache, "tok", u)
		if code != http.StatusOK || body != `{"number":1,"state":"open"}` {
			t.Fatalf("unchanged GET: %d %q", code, body)
		}
		if s.lastAsked() == "" {
			t.Fatal("an unchanged GET was not asked conditionally")
		}
		if remaining != "4999" {
			t.Fatalf("remaining = %q, want the 304's fresh 4999", remaining)
		}
	}
	if n := s.billedCount(); n != 1 {
		t.Fatalf("billed %d, want 1 over six unchanged GETs", n)
	}
	s.set("/repos/o/r/issues/1", `{"number":1,"state":"closed"}`)
	if _, body, _ := etagGet(t, cache, "tok", u); body != `{"number":1,"state":"closed"}` {
		t.Fatalf("after a change: %q, want the new body", body)
	}
}

// TestETagPassesThrough: what is not a plain GET is never made conditional
// nor served from the cache: a caller's own If-None-Match (it handles the
// 304 itself), a write, an error answer (which also forgets what was kept),
// and another credential's request for the same URL.
func TestETagPassesThrough(t *testing.T) {
	s, srv := newTaggedServer(t)
	s.set("/repos/o/r/pulls/2", `{"number":2}`)
	u := srv.URL + "/repos/o/r/pulls/2"
	cache := NewETagCache(1 << 20)
	etagGet(t, cache, "tok", u)

	if code, body, _ := etagGet(t, cache, "tok", u, "If-None-Match", `W/"other"`); code != http.StatusOK ||
		s.lastAsked() != `W/"other"` || body != `{"number":2}` {
		t.Fatalf("caller's If-None-Match: %d %q, asked %q", code, body, s.lastAsked())
	}
	if code, _, _ := etagGet(t, cache, "other-token", u); code != http.StatusOK || s.lastAsked() != "" {
		t.Fatalf("another credential was asked conditionally on this one's answer: asked %q", s.lastAsked())
	}

	c := &http.Client{Transport: &etagTransport{next: http.DefaultTransport, cache: cache}}
	resp, err := c.Post(u, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if s.lastAsked() != "" {
		t.Fatalf("a POST was made conditional: %q", s.lastAsked())
	}

	s.mu.Lock()
	s.status["/repos/o/r/pulls/2"] = http.StatusNotFound
	s.mu.Unlock()
	if code, _, _ := etagGet(t, cache, "tok", u); code != http.StatusNotFound {
		t.Fatalf("a 404 became %d", code)
	}
	s.mu.Lock()
	delete(s.status, "/repos/o/r/pulls/2")
	s.mu.Unlock()
	if etagGet(t, cache, "tok", u); s.lastAsked() != "" {
		t.Fatalf("the answer kept before a 404 was asked for again: %q", s.lastAsked())
	}
}

// TestETagStandsAlone: a 304 stands for the whole answer only when no
// change could hide behind an unchanged body: an object; a listing page
// that is not full, or that names its next page. A full page with no next
// page (an entry added would start page two and leave this one's tag as
// it was) and a page of unknown size never do, nor anything not JSON.
func TestETagStandsAlone(t *testing.T) {
	next := `<https://api.github.com/repos/o/r/issues?page=2>; rel="next", <https://x>; rel="last"`
	for _, tc := range []struct {
		name, query, body, link string
		want                    bool
	}{
		{"object", "", `{"number":1}`, "", true},
		{"wrapped listing", "per_page=100", `{"total_count":3,"check_runs":[]}`, "", true},
		{"short page", "per_page=3", `[1,2]`, "", true},
		{"empty page", "per_page=100", ` [] `, "", true},
		{"full page with a next page", "per_page=2", `[1,2]`, next, true},
		{"full last page", "per_page=2", `[1,2]`, "", false},
		{"full last page, other links", "per_page=2", `[1,2]`, `<https://x>; rel="prev"`, false},
		{"unknown page size", "", `[1]`, "", false},
		{"bad page size", "per_page=x", `[1]`, "", false},
		{"not json", "per_page=100", `[1,`, "", false},
		{"empty", "", ``, "", false},
		{"text", "", `hello`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse("https://api.github.com/repos/o/r/issues?" + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			if tc.link != "" {
				h.Set("Link", tc.link)
			}
			if got := standsAlone(u, h, []byte(tc.body)); got != tc.want {
				t.Fatalf("standsAlone = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestETagFullLastPageIsReadAgain: end to end, a full last page is never
// asked conditionally, so an entry that starts a next page is seen.
func TestETagFullLastPageIsReadAgain(t *testing.T) {
	s, srv := newTaggedServer(t)
	s.set("/repos/o/r/issues/1/comments", `[{"id":1},{"id":2}]`)
	u := srv.URL + "/repos/o/r/issues/1/comments?per_page=2&page=1"
	cache := NewETagCache(1 << 20)
	etagGet(t, cache, "tok", u)
	if etagGet(t, cache, "tok", u); s.lastAsked() != "" {
		t.Fatalf("a full last page was asked conditionally: %q", s.lastAsked())
	}
}

// TestETagCacheIsBounded: the cache never holds more than its bytes, the
// least recently used answer going first, and an answer too big for its
// share is passed through whole and not kept.
func TestETagCacheIsBounded(t *testing.T) {
	s, srv := newTaggedServer(t)
	const maxBytes = 16 << 10
	cache := NewETagCache(maxBytes)
	for i := range 50 {
		s.set(fmt.Sprintf("/repos/o/r/issues/%d", i), `{"pad":"`+strings.Repeat("x", 400)+`"}`)
		etagGet(t, cache, "tok", fmt.Sprintf("%s/repos/o/r/issues/%d", srv.URL, i))
		if cache.bytes > maxBytes {
			t.Fatalf("cache holds %d bytes, bound %d", cache.bytes, maxBytes)
		}
	}
	if etagGet(t, cache, "tok", srv.URL+"/repos/o/r/issues/0"); s.lastAsked() != "" {
		t.Fatal("the least recently used answer was not evicted")
	}
	if etagGet(t, cache, "tok", srv.URL+"/repos/o/r/issues/49"); s.lastAsked() == "" {
		t.Fatal("the most recently used answer was evicted")
	}
	big := `{"pad":"` + strings.Repeat("y", maxBytes) + `"}`
	s.set("/repos/o/r/issues/big", big)
	if _, body, _ := etagGet(t, cache, "tok", srv.URL+"/repos/o/r/issues/big"); body != big {
		t.Fatalf("an answer too big to keep was cut: %d bytes", len(body))
	}
	if etagGet(t, cache, "tok", srv.URL+"/repos/o/r/issues/big"); s.lastAsked() != "" {
		t.Fatal("an answer too big for its share was kept")
	}
}

// TestETagThroughTheClient: a token client built WithETagCache bills an
// unchanged issue once, and a listing it walks with its own entity tag
// (ListIssues) keeps reporting NotModified itself.
func TestETagThroughTheClient(t *testing.T) {
	s, srv := newTaggedServer(t)
	s.set("/repos/o/r/issues/1", `{"number":1,"state":"open","title":"t"}`)
	s.set("/repos/o/r/issues", `[{"number":1,"state":"open"}]`)
	c, err := NewToken("tok", srv.URL, WithETagCache(NewETagCache(1<<20)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo := Repo{Owner: "o", Name: "r"}
	for range 3 {
		is, err := c.GetIssue(ctx, repo, 1)
		if err != nil || is.Title != "t" || is.State != "open" {
			t.Fatalf("GetIssue = %+v, %v", is, err)
		}
	}
	if n := s.billedCount(); n != 1 {
		t.Fatalf("billed %d for three unchanged reads, want 1", n)
	}
	first, err := c.ListIssues(ctx, repo, nil, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.ListIssues(ctx, repo, nil, "open", first.ETag)
	if err != nil || !again.NotModified {
		t.Fatalf("ListIssues with its own tag = %+v, %v; want NotModified", again, err)
	}
}
