// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// profilesJSON is the chart's rendering of one profile (§3.4 of the spec),
// admitting uuid, golang.org/x and BurntSushi.
const profilesJSON = `[{"name":"go","route":"go","port":8090,"host":"github.com",
  "repositories":["devthenet-labs/patchy-preview-demo"],
  "modules":["github.com/google/uuid","golang.org/x","github.com/BurntSushi"],
  "bakedModCache":"/opt/go/pkg/mod","allowPseudoVersions":false},
 {"name":"pseudo","route":"go","port":8091,
  "repositories":["devthenet-labs/patchy-target"],
  "modules":["github.com/google/uuid"],"allowPseudoVersions":true}]`

func testProfiles(t *testing.T) netprofile.Set {
	t.Helper()
	set, err := netprofile.Parse([]byte(profilesJSON))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines decodes the JSON log lines.
func (b *lockedBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// upstreamHit is what an upstream saw of one request.
type upstreamHit struct {
	method     string
	requestURI string
	header     http.Header
}

// recordingUpstream serves h and records every request it sees.
type recordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	hits []upstreamHit
}

func newUpstream(t *testing.T, h http.HandlerFunc) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits = append(u.hits, upstreamHit{method: r.Method, requestURI: r.RequestURI, header: r.Header.Clone()})
		u.mu.Unlock()
		if h != nil {
			h(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) Hits() []upstreamHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamHit(nil), u.hits...)
}

// failingUpstream fails the test on any request: the refusal paths must
// never contact the upstream.
func failingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream contacted: %s %s", r.Method, r.RequestURI)
		w.WriteHeader(http.StatusTeapot)
	})
}

// harness is an engine on loopback, its profile listeners and its log.
type harness struct {
	srv    *Server
	urls   map[string]string // profile name -> listener base URL
	log    *lockedBuffer
	goUp   *recordingUpstream
	sumUp  *recordingUpstream
	client *http.Client
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// newHarness starts the engine with goUp and sumUp as its upstreams (the
// sumdb one at a base path, to pin the join) and the test dial policy.
func newHarness(t *testing.T, cfg Config, goUp, sumUp *recordingUpstream) *harness {
	t.Helper()
	if cfg.Profiles.Len() == 0 {
		cfg.Profiles = testProfiles(t)
	}
	cfg.GoProxy = mustURL(t, goUp.URL)
	cfg.SumDB = mustURL(t, sumUp.URL+"/sumdb-base")
	cfg.Policy.testLoopback = true
	log := &lockedBuffer{}
	srv, err := New(cfg, slog.New(slog.NewJSONHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{srv: srv, urls: map[string]string{}, log: log, goUp: goUp, sumUp: sumUp,
		client: &http.Client{Transport: &http.Transport{DisableCompression: true}}}
	for _, p := range cfg.Profiles.Profiles() {
		handler, err := srv.Handler(p.Name)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(handler)
		t.Cleanup(ts.Close)
		h.urls[p.Name] = ts.URL
	}
	return h
}

// result is a response with its body read.
type result struct {
	status int
	header http.Header
	body   string
	err    error
}

func (h *harness) do(t *testing.T, profile, method, path string, header http.Header) result {
	t.Helper()
	req, err := http.NewRequest(method, h.urls[profile]+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return result{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, body: string(body), err: err}
}

func (h *harness) get(t *testing.T, path string) result {
	t.Helper()
	return h.do(t, "go", http.MethodGet, path, nil)
}

// raw sends one raw HTTP/1.1 request line and returns the response status.
func raw(t *testing.T, base, requestLine string, extra ...string) int {
	t.Helper()
	u := mustURL(t, base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	msg := requestLine + "\r\nHost: " + u.Host + "\r\n" + strings.Join(extra, "") + "Connection: close\r\n\r\n"
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func okBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
}

// TestAllowedModuleForwarded: an allowed module's requests reach the module
// proxy at exactly the path the go command sent, escaping included, and the
// answer comes back.
func TestAllowedModuleForwarded(t *testing.T) {
	goUp := newUpstream(t, okBody("v1.5.0\nv1.6.0\n"))
	h := newHarness(t, Config{}, goUp, failingUpstream(t))
	paths := []string{
		"/go/github.com/google/uuid/@v/list",
		"/go/github.com/google/uuid/@v/v1.6.0.info",
		"/go/github.com/google/uuid/@v/v1.6.0.mod",
		"/go/github.com/google/uuid/@v/v1.6.0.zip",
		"/go/github.com/google/uuid/@latest",
		"/go/golang.org/x/mod/@v/v0.20.0.mod",
		"/go/github.com/%21burnt%21sushi/toml/@v/v1.3.2.mod",
	}
	for _, p := range paths {
		r := h.get(t, p)
		if r.status != http.StatusOK || r.body != "v1.5.0\nv1.6.0\n" {
			t.Errorf("GET %s = %d %q, want 200 and the upstream body", p, r.status, r.body)
		}
	}
	hits := goUp.Hits()
	if len(hits) != len(paths) {
		t.Fatalf("upstream saw %d requests, want %d", len(hits), len(paths))
	}
	for i, p := range paths {
		if want := strings.TrimPrefix(p, "/go"); hits[i].requestURI != want {
			t.Errorf("upstream request URI = %q, want %q", hits[i].requestURI, want)
		}
	}
}

// TestHeadForwardedAsHead: a HEAD stays a HEAD upstream.
func TestHeadForwardedAsHead(t *testing.T) {
	goUp := newUpstream(t, okBody("x"))
	h := newHarness(t, Config{}, goUp, failingUpstream(t))
	r := h.do(t, "go", http.MethodHead, "/go/github.com/google/uuid/@v/list", nil)
	if r.status != http.StatusOK || r.body != "" {
		t.Errorf("HEAD = %d %q, want 200 and no body", r.status, r.body)
	}
	if hits := goUp.Hits(); len(hits) != 1 || hits[0].method != http.MethodHead {
		t.Errorf("upstream hits = %+v, want one HEAD", hits)
	}
}

// TestDeniedModuleNeverContactsUpstream: a module outside the allowlist,
// and the go command's prefix probes above an allowed one, are 410 with the
// reason, and the upstream never sees them.
func TestDeniedModuleNeverContactsUpstream(t *testing.T) {
	h := newHarness(t, Config{}, failingUpstream(t), failingUpstream(t))
	tests := []struct{ path, module string }{
		{"/go/rsc.io/quote/@v/v1.5.2.info", "rsc.io/quote"},
		{"/go/rsc.io/quote/@v/list", "rsc.io/quote"},
		{"/go/rsc.io/quote/@latest", "rsc.io/quote"},
		{"/go/github.com/@v/v1.6.0.info", "github.com"},
		{"/go/github.com/google/@v/v1.6.0.info", "github.com/google"},
		{"/go/github.com/google/uuidx/@v/list", "github.com/google/uuidx"},
		{"/go/golang.org/xyz/@v/list", "golang.org/xyz"},
		{"/go/github.com/burntsushi/toml/@v/list", "github.com/burntsushi/toml"},
		{"/go/sumdb/sum.golang.org/lookup/rsc.io/quote@v1.5.2", "rsc.io/quote"},
	}
	for _, tt := range tests {
		r := h.get(t, tt.path)
		want := fmt.Sprintf("egress-proxy: module path %s is not allowed by profile go; allowed prefixes: "+
			"github.com/google/uuid,golang.org/x,github.com/BurntSushi\n", tt.module)
		if r.status != http.StatusGone || r.body != want {
			t.Errorf("GET %s = %d %q, want 410 %q", tt.path, r.status, r.body, want)
		}
	}
}

// TestSumDBRoutes: supported is answered locally, latest and tiles are
// forwarded below the sumdb base, and an allowed lookup is forwarded.
func TestSumDBRoutes(t *testing.T) {
	sumUp := newUpstream(t, okBody("tree"))
	h := newHarness(t, Config{}, failingUpstream(t), sumUp)

	if r := h.get(t, "/go/sumdb/sum.golang.org/supported"); r.status != http.StatusOK || r.body != "" {
		t.Errorf("supported = %d %q, want 200 and no body", r.status, r.body)
	}
	if len(sumUp.Hits()) != 0 {
		t.Fatalf("supported reached the upstream")
	}
	forwarded := []string{
		"latest",
		"tile/8/0/x001/234.p/5",
		"tile/8/data/000",
		"lookup/github.com/google/uuid@v1.6.0",
		"lookup/github.com/%21burnt%21sushi/toml@v1.3.2",
	}
	for _, p := range forwarded {
		before := len(sumUp.Hits())
		r := h.get(t, "/go/sumdb/sum.golang.org/"+p)
		hits := sumUp.Hits()
		if want := "/sumdb-base/" + p; r.status != http.StatusOK || len(hits) != before+1 ||
			hits[before].requestURI != want {
			t.Errorf("GET %s = %d, upstream %+v; want 200 at %s", p, r.status, hits[before:], want)
		}
	}
	if r := h.get(t, "/go/sumdb/sum.example.org/supported"); r.status != http.StatusNotFound {
		t.Errorf("another database's supported = %d, want 404", r.status)
	}
}

// TestVersionGate: .info, .mod, .zip and lookup admit only canonical
// tagged versions (unless the profile admits pseudo-versions) and refuse
// the rest with 410 before any upstream contact; list and @latest are never
// gated.
func TestVersionGate(t *testing.T) {
	goUp := newUpstream(t, okBody("ok"))
	sumUp := newUpstream(t, okBody("ok"))
	h := newHarness(t, Config{}, goUp, sumUp)
	const pseudo = "v0.0.0-20240101000000-abcdefabcdef"
	refused := []string{pseudo, "main", "abcdef1", "v1.6"}
	for _, v := range refused {
		for _, p := range []string{
			"/go/github.com/google/uuid/@v/" + v + ".info",
			"/go/github.com/google/uuid/@v/" + v + ".mod",
			"/go/github.com/google/uuid/@v/" + v + ".zip",
			"/go/sumdb/sum.golang.org/lookup/github.com/google/uuid@" + v,
		} {
			r := h.get(t, p)
			want := "egress-proxy: version " + v + " of github.com/google/uuid is not a tagged release; " +
				"profile go admits tagged releases only\n"
			if r.status != http.StatusGone || r.body != want {
				t.Errorf("GET %s = %d %q, want 410 %q", p, r.status, r.body, want)
			}
		}
	}
	if n := len(goUp.Hits()) + len(sumUp.Hits()); n != 0 {
		t.Fatalf("refused versions reached the upstream %d times", n)
	}
	for _, v := range []string{"v1.6.0", "v2.0.0+incompatible", "v1.6.0-rc.1"} {
		if r := h.get(t, "/go/github.com/google/uuid/@v/"+v+".info"); r.status != http.StatusOK {
			t.Errorf("version %s = %d, want 200", v, r.status)
		}
	}
	for _, p := range []string{"/go/github.com/google/uuid/@v/list", "/go/github.com/google/uuid/@latest"} {
		if r := h.get(t, p); r.status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (never version-gated)", p, r.status)
		}
	}
	// The pseudo profile forwards the pseudo-version, but still refuses a
	// branch name.
	if r := h.do(t, "pseudo", http.MethodGet, "/go/github.com/google/uuid/@v/"+pseudo+".info", nil); r.status != 200 {
		t.Errorf("pseudo profile, pseudo-version = %d, want 200", r.status)
	}
	if r := h.do(t, "pseudo", http.MethodGet, "/go/sumdb/sum.golang.org/lookup/github.com/google/uuid@"+pseudo,
		nil); r.status != 200 {
		t.Errorf("pseudo profile, pseudo-version lookup = %d, want 200", r.status)
	}
	if r := h.do(t, "pseudo", http.MethodGet, "/go/github.com/google/uuid/@v/main.info", nil); r.status != 410 ||
		!strings.Contains(r.body, "not a canonical version") {
		t.Errorf("pseudo profile, branch = %d %q, want 410 not a canonical version", r.status, r.body)
	}
	for _, line := range h.log.lines(t) {
		if line["outcome"] == OutcomeNotAllowed && line["reason"] != ReasonVersion {
			t.Errorf("version refusal audited with reason %v", line["reason"])
		}
	}
}

// TestSuspended: with Suspended, every request, supported included, is 410
// suspended and audited, and the upstream is never contacted.
func TestSuspended(t *testing.T) {
	h := newHarness(t, Config{Suspended: true}, failingUpstream(t), failingUpstream(t))
	for _, p := range []string{
		"/go/github.com/google/uuid/@v/list",
		"/go/sumdb/sum.golang.org/supported",
		"/go/sumdb/sum.golang.org/latest",
		"/go/rsc.io/quote/@v/list",
		"/elsewhere",
	} {
		r := h.get(t, p)
		if r.status != http.StatusGone || r.body != "egress-proxy: profile routes are suspended by the operator\n" {
			t.Errorf("GET %s = %d %q, want 410 suspended", p, r.status, r.body)
		}
	}
	lines := h.log.lines(t)
	if len(lines) != 5 {
		t.Fatalf("%d audit lines, want 5", len(lines))
	}
	for _, l := range lines {
		if l["outcome"] != OutcomeSuspended || l["msg"] != "refused" {
			t.Errorf("audit line %v, want a refused suspended line", l)
		}
	}
}

// TestRequestEnvelope: only GET and HEAD, no body, no query, no
// absolute-form URI; shape refusals of well-formed requests are 410, never
// 400; a path outside the protocol is 404.
func TestRequestEnvelope(t *testing.T) {
	h := newHarness(t, Config{}, failingUpstream(t), failingUpstream(t))
	base := h.urls["go"]
	const ok = "/go/github.com/google/uuid/@v/list"
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if r := h.do(t, "go", m, ok, nil); r.status != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, r.status)
		}
	}
	tests := []struct {
		name string
		line string
		hdr  []string
		want int
	}{
		{"connect", "CONNECT proxy.golang.org:443 HTTP/1.1", nil, http.StatusMethodNotAllowed},
		{"absolute-form", "GET http://proxy.golang.org" + ok + " HTTP/1.1", nil, http.StatusBadRequest},
		{"body", "GET " + ok + " HTTP/1.1", []string{"Content-Length: 3\r\n"}, http.StatusBadRequest},
		{"chunked body", "GET " + ok + " HTTP/1.1", []string{"Transfer-Encoding: chunked\r\n"}, http.StatusBadRequest},
		{"query", "GET " + ok + "?go-get=1 HTTP/1.1", nil, http.StatusBadRequest},
		{"empty query", "GET " + ok + "? HTTP/1.1", nil, http.StatusBadRequest},
		{"non-canonical escape", "GET /go/github.com/google/uui%64/@v/list HTTP/1.1", nil, http.StatusGone},
		{"raw bang", "GET /go/github.com/!burnt!sushi/toml/@v/list HTTP/1.1", nil, http.StatusGone},
		{"escaped slash", "GET /go/github.com/google%2Fuuid/@v/list HTTP/1.1", nil, http.StatusGone},
		{"dot dot", "GET /go/github.com/google/../uuid/@v/list HTTP/1.1", nil, http.StatusGone},
		{"double slash", "GET /go/github.com//google/uuid/@v/list HTTP/1.1", nil, http.StatusGone},
		{"space escape", "GET /go/github.com/google/u%20uid/@v/list HTTP/1.1", nil, http.StatusGone},
		{"too long", "GET /go/" + strings.Repeat("a", MaxPathLen) + ".com/@v/list HTTP/1.1", nil, http.StatusGone},
		{"not a proxy path", "GET /go/github.com/google/uuid HTTP/1.1", nil, http.StatusGone},
		{"outside /go", "GET /github.com/google/uuid/@v/list HTTP/1.1", nil, http.StatusNotFound},
		{"bare /go", "GET /go HTTP/1.1", nil, http.StatusNotFound},
	}
	for _, tt := range tests {
		if got := raw(t, base, tt.line, tt.hdr...); got != tt.want {
			t.Errorf("%s: %q = %d, want %d", tt.name, tt.line, got, tt.want)
		}
	}
}

// TestStatusMapping: 200, 404 and 410 pass through with their bodies;
// every other status, a redirect included, becomes 502 and no Location
// leaves the proxy.
func TestStatusMapping(t *testing.T) {
	for _, tt := range []struct {
		upstream, want int
	}{
		{200, 200}, {404, 404}, {410, 410},
		{301, 502}, {302, 502}, {304, 502}, {403, 502}, {429, 502}, {500, 502}, {503, 502},
	} {
		goUp := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.example/")
			w.WriteHeader(tt.upstream)
			_, _ = io.WriteString(w, "not found: upstream says")
		})
		h := newHarness(t, Config{}, goUp, failingUpstream(t))
		r := h.get(t, "/go/github.com/google/uuid/@v/list")
		if r.status != tt.want {
			t.Errorf("upstream %d answered %d, want %d", tt.upstream, r.status, tt.want)
		}
		if r.header.Get("Location") != "" {
			t.Errorf("upstream %d: Location %q left the proxy", tt.upstream, r.header.Get("Location"))
		}
		if tt.want == 502 && r.body != fmt.Sprintf("egress-proxy: upstream answered %d\n", tt.upstream) {
			t.Errorf("upstream %d: body %q", tt.upstream, r.body)
		}
		if (tt.want == 404 || tt.want == 410) && r.body != "not found: upstream says" {
			t.Errorf("upstream %d: body %q, want it passed through", tt.upstream, r.body)
		}
	}
}

// TestHeaderAllowlist: no inbound header reaches the upstream (the
// outbound headers are the fixed User-Agent and Accept-Encoding), and only
// Content-Type, Content-Length and Last-Modified come back.
func TestHeaderAllowlist(t *testing.T) {
	goUp := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		h.Set("Set-Cookie", "session=1")
		h.Set("X-Upstream-Secret", "leak")
		h.Set("Cache-Control", "public")
		_, _ = io.WriteString(w, "body")
	})
	h := newHarness(t, Config{}, goUp, failingUpstream(t))
	r := h.do(t, "go", http.MethodGet, "/go/github.com/google/uuid/@v/list", http.Header{
		"Authorization":     {"Bearer secret-token"},
		"Cookie":            {"a=b"},
		"X-Forwarded-For":   {"1.2.3.4"},
		"X-Forwarded-Host":  {"evil"},
		"X-Forwarded-Proto": {"https"},
		"User-Agent":        {"Go-http-client/1.1 custom"},
		"Accept-Encoding":   {"gzip"},
	})
	if r.status != 200 || r.body != "body" {
		t.Fatalf("GET = %d %q", r.status, r.body)
	}
	hit := goUp.Hits()[0]
	for k := range hit.header {
		switch k {
		case "User-Agent", "Accept-Encoding":
		default:
			t.Errorf("upstream saw header %s: %v", k, hit.header[k])
		}
	}
	if got := hit.header.Get("User-Agent"); !strings.HasPrefix(got, "maul-egress-proxy/") {
		t.Errorf("outbound User-Agent = %q", got)
	}
	if got := hit.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("outbound Accept-Encoding = %q, want identity", got)
	}
	for k := range r.header {
		switch k {
		case "Content-Type", "Content-Length", "Last-Modified", "Date":
			// Date is the proxy's own server header.
		default:
			t.Errorf("response carried header %s: %v", k, r.header[k])
		}
	}
	if r.header.Get("Last-Modified") == "" || r.header.Get("Content-Length") != "4" {
		t.Errorf("allowlisted headers not copied: %v", r.header)
	}
}

// TestSizeCaps: a declared Content-Length over the kind's cap is a 502
// before any byte; a stream past the cap is cut and the connection aborted.
func TestSizeCaps(t *testing.T) {
	big := strings.Repeat("x", maxInfoBytes+1)
	t.Run("declared", func(t *testing.T) {
		goUp := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(big)))
			_, _ = io.WriteString(w, big)
		})
		h := newHarness(t, Config{}, goUp, failingUpstream(t))
		r := h.get(t, "/go/github.com/google/uuid/@v/v1.6.0.info")
		if r.status != http.StatusBadGateway || !strings.Contains(r.body, "exceeds") {
			t.Errorf(".info over 64 KiB = %d (%d bytes), want 502", r.status, len(r.body))
		}
		// The same size is within the list cap.
		if r := h.get(t, "/go/github.com/google/uuid/@v/list"); r.status != 200 || len(r.body) != len(big) {
			t.Errorf("list of %d bytes = %d, %d bytes", len(big), r.status, len(r.body))
		}
	})
	t.Run("streamed", func(t *testing.T) {
		goUp := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			// No Content-Length: flush in pieces so the response is chunked.
			for range 3 {
				_, _ = io.WriteString(w, strings.Repeat("y", maxInfoBytes/2))
				w.(http.Flusher).Flush()
			}
		})
		h := newHarness(t, Config{}, goUp, failingUpstream(t))
		r := h.get(t, "/go/github.com/google/uuid/@v/v1.6.0.info")
		if r.err == nil {
			t.Errorf("streamed .info over the cap read cleanly (%d, %d bytes), want a cut transfer",
				r.status, len(r.body))
		}
		waitForLine(t, h.log, func(l map[string]any) bool { return l["outcome"] == OutcomeTooLarge })
	})
	t.Run("zip cap", func(t *testing.T) {
		goUp := newUpstream(t, okBody(strings.Repeat("z", 2048)))
		h := newHarness(t, Config{Limits: Limits{MaxZipBytes: 1024}}, goUp, failingUpstream(t))
		if r := h.get(t, "/go/github.com/google/uuid/@v/v1.6.0.zip"); r.status != http.StatusBadGateway {
			t.Errorf("zip over --max-zip-bytes = %d, want 502", r.status)
		}
	})
}

// waitForLine waits for an audit line (the abort path audits as the
// handler unwinds, after the client has seen the cut).
func waitForLine(t *testing.T, log *lockedBuffer, match func(map[string]any) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range log.lines(t) {
			if match(l) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no matching audit line in:\n%s", log.String())
}

// TestAudit: exactly one line per request with the fields of the spec,
// attribution filled from the PodLookup, no header value and clipped
// strings.
func TestAudit(t *testing.T) {
	goUp := newUpstream(t, okBody("v1.6.0\n"))
	pods := fakePods{"127.0.0.1": {Name: "maul-x-abc", Finding: "f-1", Owner: "devthenet-labs",
		RunKind: "remediation", Attempt: "2", Repo: "devthenet-labs/patchy-target"}}
	h := newHarness(t, Config{Pods: pods}, goUp, failingUpstream(t))
	secret := http.Header{"Authorization": {"Bearer very-secret-value"}, "Cookie": {"c=cookie-secret"}}
	h.do(t, "go", http.MethodGet, "/go/github.com/google/uuid/@v/list", secret)
	long := "example.com/" + strings.Repeat("a", 600)
	h.do(t, "go", http.MethodGet, "/go/"+long+"/@v/v1.0.0.info", secret)

	if s := h.log.String(); strings.Contains(s, "very-secret-value") || strings.Contains(s, "cookie-secret") {
		t.Fatalf("a header value reached the audit log:\n%s", s)
	}
	lines := h.log.lines(t)
	if len(lines) != 2 {
		t.Fatalf("%d audit lines, want 2:\n%s", len(lines), h.log.String())
	}
	fields := []string{"profile", "port", "source_ip", "pod", "finding", "owner", "run_kind", "attempt", "repo",
		"route", "kind", "module", "version", "method", "status", "upstream_status", "bytes", "duration",
		"outcome", "reason"}
	for _, l := range lines {
		for _, f := range fields {
			if _, ok := l[f]; !ok {
				t.Errorf("audit line lacks %s: %v", f, l)
			}
		}
	}
	ok := lines[0]
	want := map[string]any{"msg": "proxied", "profile": "go", "port": float64(8090), "source_ip": "127.0.0.1",
		"pod": "maul-x-abc", "finding": "f-1", "owner": "devthenet-labs", "run_kind": "remediation",
		"attempt": "2", "repo": "devthenet-labs/patchy-target", "route": "go", "kind": "list",
		"module": "github.com/google/uuid", "method": "GET", "status": float64(200),
		"upstream_status": float64(200), "bytes": float64(7), "outcome": OutcomeProxied}
	for k, v := range want {
		if ok[k] != v {
			t.Errorf("proxied line %s = %v, want %v", k, ok[k], v)
		}
	}
	denied := lines[1]
	if denied["msg"] != "refused" || denied["outcome"] != OutcomeNotAllowed || denied["reason"] != ReasonPrefix ||
		denied["status"] != float64(410) || denied["version"] != "v1.0.0" {
		t.Errorf("refused line = %v", denied)
	}
	if m, _ := denied["module"].(string); m != long[:maxEcho]+"...(truncated)" {
		t.Errorf("module not clipped: %d bytes", len(m))
	}
}

type fakePods map[string]PodInfo

func (f fakePods) Lookup(ip string) (PodInfo, bool) {
	p, ok := f[ip]
	return p, ok
}

// TestHealth: healthz is always 200; readyz is 503 until SetReady.
func TestHealth(t *testing.T) {
	h := newHarness(t, Config{}, failingUpstream(t), failingUpstream(t))
	hh := h.srv.HealthHandler()
	probe := func(path string) int {
		rec := httptest.NewRecorder()
		hh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if probe("/healthz") != 200 || probe("/readyz") != 503 {
		t.Errorf("before SetReady: healthz %d, readyz %d; want 200, 503", probe("/healthz"), probe("/readyz"))
	}
	h.srv.SetReady()
	if probe("/readyz") != 200 {
		t.Errorf("after SetReady: readyz %d, want 200", probe("/readyz"))
	}
}

// TestNewRefuses: no profiles, a bad upstream, and an unknown profile's
// handler are refused.
func TestNewRefuses(t *testing.T) {
	if _, err := New(Config{}, nil); err == nil {
		t.Error("New without profiles succeeded")
	}
	set := testProfiles(t)
	if _, err := New(Config{Profiles: set, GoProxy: &url.URL{Scheme: "ftp", Host: "x"}}, nil); err == nil {
		t.Error("New with an ftp upstream succeeded")
	}
	if _, err := New(Config{Profiles: set, SumDB: &url.URL{Scheme: "https", Host: "x", RawQuery: "a=b"}},
		nil); err == nil {
		t.Error("New with a query on the upstream succeeded")
	}
	srv, err := New(Config{Profiles: set}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Handler("npm"); err == nil {
		t.Error("Handler for an unknown profile succeeded")
	}
	if got := srv.cfg.GoProxy.String() + " " + srv.cfg.SumDB.String(); got != DefaultGoProxy+" "+DefaultSumDB {
		t.Errorf("default upstreams = %s", got)
	}
}

func TestParseUpstream(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://proxy.golang.org":      true,
		"https://proxy.example/base":    true,
		"ftp://proxy.golang.org":        false,
		"https://":                      false,
		"https://u:p@proxy.golang.org":  false,
		"https://proxy.golang.org/?x=1": false,
		"https://proxy.golang.org/#f":   false,
		"::":                            false,
	} {
		if _, err := ParseUpstream(raw); (err == nil) != ok {
			t.Errorf("ParseUpstream(%q) error = %v, want ok=%v", raw, err, ok)
		}
	}
}
