// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// cannedAPI answers each path with a fixed status, body and (optional)
// declared Content-Length.
type cannedAPI map[string]struct {
	status int
	body   string
	length int64
}

func (c cannedAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, ok := c[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.length > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(a.length))
	}
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, a.body)
}

func cannedGitHub(t *testing.T, api cannedAPI) *HTTPGitHub {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &HTTPGitHub{APIURL: srv.URL}
}

func TestHTTPGitHubHeadFailures(t *testing.T) {
	const repo = "/repos/acme/web"
	cases := []struct {
		name string
		api  cannedAPI
		want string
	}{
		{
			name: "repository answer without a default branch",
			api:  cannedAPI{repo: {body: `{"name":"web"}`}},
			want: "without a default branch",
		},
		{
			name: "repository answer that is not JSON",
			api:  cannedAPI{repo: {body: `<html>`}},
			want: "without a default branch",
		},
		{
			name: "repository read fails",
			api:  cannedAPI{repo: {status: http.StatusInternalServerError}},
			want: "500 Internal Server Error",
		},
		{
			name: "head commit read fails",
			api: cannedAPI{
				repo:                   {body: `{"default_branch":"main"}`},
				repo + "/commits/main": {status: http.StatusBadGateway},
			},
			want: "502 Bad Gateway",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := cannedGitHub(t, c.api)
			head, err := g.Head(context.Background(), "https://github.com/acme/web")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Head = %+v, %v; want error containing %q", head, err, c.want)
			}
			if head != (Head{}) {
				t.Errorf("head = %+v alongside an error", head)
			}
		})
	}
}

func TestHTTPGitHubFileEdges(t *testing.T) {
	const trees = "/repos/acme/web/git/trees/"
	const blobs = "/repos/acme/web/git/blobs/"
	ref := sha("c")
	listing := func(size int) string {
		return fmt.Sprintf(`{"truncated":false,"tree":[{"path":"agent.yaml","mode":"100644","type":"blob",`+
			`"sha":"b1","size":%d}]}`, size)
	}
	cases := []struct {
		name      string
		api       cannedAPI
		path      string
		limit     int64
		wantData  string
		wantSize  int64
		wantFound bool
		wantErr   string
	}{
		{
			name: "root-level file", path: "agent.yaml", limit: 100,
			api:      cannedAPI{trees + ref: {body: listing(5)}, blobs + "b1": {body: "hello"}},
			wantData: "hello", wantSize: 5, wantFound: true,
		},
		{
			name: "listing says too large", path: ".patchy/agent.yaml", limit: 4,
			api:      cannedAPI{trees + ref + ":.patchy": {body: listing(5000)}},
			wantSize: 5000, wantFound: true,
		},
		{
			name: "blob turns out larger than the listing said", path: ".patchy/agent.yaml", limit: 5,
			api: cannedAPI{
				trees + ref + ":.patchy": {body: listing(3)},
				blobs + "b1":             {body: "0123456789", length: 10},
			},
			wantSize: 10, wantFound: true,
		},
		{
			name: "tree read fails", path: ".patchy/agent.yaml", limit: 100,
			api:     cannedAPI{trees + ref + ":.patchy": {status: http.StatusForbidden}},
			wantErr: "403 Forbidden",
		},
		{
			name: "tree answer is not JSON", path: ".patchy/agent.yaml", limit: 100,
			api:     cannedAPI{trees + ref + ":.patchy": {body: "nope"}},
			wantErr: "without a tree",
		},
		{
			name: "blob read fails", path: ".patchy/agent.yaml", limit: 100,
			api: cannedAPI{
				trees + ref + ":.patchy": {body: listing(5)},
				blobs + "b1":             {status: http.StatusInternalServerError},
			},
			wantErr: "500 Internal Server Error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := cannedGitHub(t, c.api)
			data, size, found, err := g.File(context.Background(), "https://github.com/acme/web", ref, c.path, c.limit)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("File err = %v, want %q", err, c.wantErr)
				}
				if found || data != nil {
					t.Errorf("File = %q, found %v alongside an error", data, found)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != c.wantData || size != c.wantSize || found != c.wantFound {
				t.Errorf("File = %q, %d, %v; want %q, %d, %v", data, size, found, c.wantData, c.wantSize, c.wantFound)
			}
			if c.wantData == "" && data != nil {
				t.Errorf("over-limit file returned data %q", data)
			}
		})
	}
}

// TestHTTPGitHubTokenWithoutSource: a token whose source is unnamed is still
// said to have been sent, and a refusal carries the dotcom hint.
func TestHTTPGitHubTokenWithoutSource(t *testing.T) {
	g := cannedGitHub(t, cannedAPI{"/repos/acme/web": {status: http.StatusUnauthorized}})
	g.Token = "gho_x"
	_, err := g.Head(context.Background(), "https://github.com/acme/web")
	if err == nil || !strings.Contains(err.Error(), "(with your token)") ||
		!strings.Contains(err.Error(), "GH_TOKEN or GITHUB_TOKEN") {
		t.Fatalf("Head = %v", err)
	}
}

func TestHTTPGitHubTransportFailures(t *testing.T) {
	failing := &HTTPGitHub{APIURL: "https://api.example.test", Client: &http.Client{
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}),
	}}
	if _, err := failing.Head(context.Background(), "https://github.com/acme/web"); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Head over a failing transport = %v", err)
	}

	broken := &HTTPGitHub{APIURL: "https://api.example.test", Client: &http.Client{
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(failingReader{}), Header: http.Header{}}, nil
		}),
	}}
	if _, err := broken.Head(context.Background(), "https://github.com/acme/web"); err == nil ||
		!strings.Contains(err.Error(), "body went away") {
		t.Errorf("Head with an unreadable body = %v", err)
	}

	badRoot := &HTTPGitHub{APIURL: "https://api.example.test/\x7f"}
	if _, err := badRoot.Head(context.Background(), "https://github.com/acme/web"); err == nil {
		t.Error("Head with an unparseable API root succeeded")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("body went away") }

func TestHostName(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"  ":                              "",
		"ghe.acme.test":                   "ghe.acme.test",
		"GHE.Acme.Test:8443":              "ghe.acme.test",
		"https://GHE.acme.test/some/path": "ghe.acme.test",
		"http://[::1":                     "",
	}
	for in, want := range cases {
		if got := hostName(in); got != want {
			t.Errorf("hostName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	if shortSHA("abc") != "abc" || shortSHA(sha("d")) != strings.Repeat("d", 12) {
		t.Error("shortSHA")
	}
	if plural(1) != "" || plural(0) != "s" || plural(2) != "s" {
		t.Error("plural")
	}
	if got := orNone("", "patchy"); !strings.Contains(got, "namespace patchy") {
		t.Errorf("orNone(empty) = %q", got)
	}
	if got := orNone("patchy/intent-controller", "patchy"); got != "patchy/intent-controller" {
		t.Errorf("orNone(named) = %q", got)
	}
	cases := []struct {
		err  error
		want transport.ErrorCode
	}{
		{errors.New("dial tcp: refused"), ""},
		{&transport.Error{StatusCode: http.StatusNotFound}, "404"},
		{&transport.Error{StatusCode: http.StatusUnauthorized}, ""},
		{fmt.Errorf("wrapped: %w", &transport.Error{
			Errors: []transport.Diagnostic{{Code: transport.ManifestUnknownErrorCode}},
		}), transport.ManifestUnknownErrorCode},
	}
	for _, c := range cases {
		if got := registryCode(c.err); got != c.want {
			t.Errorf("registryCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestKeychainAndFetch(t *testing.T) {
	r := &run{}
	if r.keychain() == nil {
		t.Fatal("no keychain without configuration")
	}
	kc := authn.NewMultiKeychain(authn.DefaultKeychain)
	r.cfg.Keychain = kc
	if r.keychain() != kc {
		t.Error("configured keychain not used")
	}
	if _, err := r.fetch(context.Background(), "UPPER/Case:bad tag"); err == nil {
		t.Error("fetch of an unparseable reference succeeded")
	}
}
