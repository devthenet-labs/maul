// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// seen is one request a fake component received.
type seen struct {
	Method, RequestURI, Host string
	Header                   http.Header
	Body                     string
}

// component is a fake previewed component: it records every request and
// answers with handler.
type component struct {
	mu      sync.Mutex
	reqs    []seen
	handler func(w http.ResponseWriter, r *http.Request, n int)
	srv     *httptest.Server
}

func newComponent(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *component {
	t.Helper()
	c := &component{handler: h}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		n := len(c.reqs)
		c.reqs = append(c.reqs, seen{r.Method, r.RequestURI, r.Host, r.Header.Clone(), string(body)})
		c.mu.Unlock()
		c.handler(w, r, n)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *component) requests() []seen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]seen(nil), c.reqs...)
}

// dialer maps Service addresses to listeners, as cluster DNS would, and
// records every address the client asked for.
type dialer struct {
	mu     sync.Mutex
	routes map[string]string
	asked  []string
}

func (d *dialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, addr)
	to, ok := d.routes[addr]
	d.mu.Unlock()
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, to)
}

func (d *dialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

func listenerAddr(c *component) string { return strings.TrimPrefix(c.srv.URL, "http://") }

// criterion decodes one criterion from JSON, failing the test on error.
func criterion(t *testing.T, raw string) acceptance.Criterion {
	t.Helper()
	var c acceptance.Criterion
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("criterion %s: %v", raw, err)
	}
	if err := acceptance.Validate([]acceptance.Criterion{c}); err != nil {
		t.Fatalf("criterion %s: %v", raw, err)
	}
	return c
}

var tokens = acceptance.RunTokens{Round: 1, Attempt: 1, Run: "abcdef"}

// apiTarget is the Service address every client test's criteria go to.
const apiTarget = "preview-bb-api:80"

// observe runs c against apiTarget, routed to comp.
func observe(t *testing.T, c acceptance.Criterion, comp *component) (acceptance.CriterionObservation, *dialer) {
	t.Helper()
	d := &dialer{routes: map[string]string{apiTarget: listenerAddr(comp)}}
	client := NewClient(Options{Dial: d.dial})
	return acceptance.Observe(c, tokens, client.Doer(context.Background(), apiTarget)), d
}

func lastStep(o acceptance.CriterionObservation) acceptance.StepObservation {
	t := o.Tries[len(o.Tries)-1]
	return t.Steps[len(t.Steps)-1]
}

// TestRequestShape: the request leaves exactly as built, to the Service
// the spec names and nowhere else: method, target, Host, headers and the
// substituted JSON body.
func TestRequestShape(t *testing.T) {
	comp := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"b1"}`)
	})
	c := criterion(t, `{"id":"create","kind":"http","statement":"s","target":"api",
	  "request":{"method":"POST","path":"/api/battles?mode=fast&tag=x","headers":{"X-Run":"r-{{run}}"},
	    "json":{"name":"maul-{{run}}"}},
	  "expect":{"status":201}}`)
	obs, d := observe(t, c, comp)

	reqs := comp.requests()
	if len(reqs) != 1 {
		t.Fatalf("component saw %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.RequestURI != "/api/battles?mode=fast&tag=x" || r.Host != apiTarget {
		t.Errorf("request = %s %s Host %s", r.Method, r.RequestURI, r.Host)
	}
	if got := r.Header.Get("X-Run"); got != "r-v1a1t1-abcdef" {
		t.Errorf("X-Run = %q", got)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if r.Body != `{"name":"maul-v1a1t1-abcdef"}` {
		t.Errorf("body = %s", r.Body)
	}
	if got := r.Header.Get("Accept-Encoding"); got != "" {
		t.Errorf("Accept-Encoding = %q: the tester must not ask for a compressed body", got)
	}
	if got := d.addresses(); len(got) != 1 || got[0] != apiTarget {
		t.Errorf("dialled %v, want only the target", got)
	}
	st := lastStep(obs)
	if st.TransportError != acceptance.TransportNone || st.Status != 201 || st.Path != "/api/battles?mode=fast&tag=x" {
		t.Errorf("observation = %+v", st)
	}
	if v := acceptance.Judge(c, obs, tokens); v.Outcome != acceptance.OutcomePass {
		t.Errorf("Judge = %+v, want pass", v)
	}
}

// TestPathBuilding: a captured value is escaped into its path segment and
// query value; one that would leave the path grammar is never sent.
func TestPathBuilding(t *testing.T) {
	for _, tc := range []struct {
		name, id, wantURI string
		sent              bool
	}{
		{"space escaped", "x y", "/items/x%20y?q=x+y", true},
		{"query delimiters escaped", "a&b=c", "/items/a&b=c?q=a%26b%3Dc", true},
		{"slash never sent", "a/b", "", false},
		{"dot segment never sent", "..", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				enc, _ := json.Marshal(map[string]string{"id": tc.id})
				_, _ = w.Write(enc)
			})
			c := criterion(t, `{"id":"items","kind":"http","statement":"s","target":"api","steps":[
			  {"request":{"path":"/new"},"capture":{"id":"/id"}},
			  {"request":{"path":"/items/{{capture.id}}?q={{capture.id}}"}}]}`)
			obs, _ := observe(t, c, comp)
			reqs := comp.requests()
			if !tc.sent {
				for _, r := range reqs {
					if strings.HasPrefix(r.RequestURI, "/items") {
						t.Fatalf("sent %s", r.RequestURI)
					}
				}
				if got := lastStep(obs).TransportError; got != acceptance.TransportCaptureMissing {
					t.Errorf("transport = %q, want capture-missing", got)
				}
				return
			}
			if len(reqs) != 2 || reqs[1].RequestURI != tc.wantURI {
				t.Fatalf("requests = %+v, want second %s", reqs, tc.wantURI)
			}
			if got := obs.Tries[0].Steps[1].Path; got != tc.wantURI {
				t.Errorf("observed path %q, want %q", got, tc.wantURI)
			}
		})
	}
}

// TestRedirectsNotFollowed: a redirect is the response, never a second
// request, wherever it points.
func TestRedirectsNotFollowed(t *testing.T) {
	for _, loc := range []string{"/elsewhere", "http://169.254.169.254/latest/meta-data/", "//other:80/x"} {
		t.Run(loc, func(t *testing.T) {
			comp := newComponent(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				http.Redirect(w, r, loc, http.StatusFound)
			})
			c := criterion(t, `{"id":"r","kind":"http","statement":"s","target":"api",
			  "request":{"path":"/start"},"expect":{"status":302}}`)
			obs, d := observe(t, c, comp)
			if n := len(comp.requests()); n != 1 {
				t.Errorf("component saw %d requests, want 1", n)
			}
			for _, a := range d.addresses() {
				if a != apiTarget {
					t.Errorf("dialled %s", a)
				}
			}
			if st := lastStep(obs); st.Status != http.StatusFound || st.TransportError != acceptance.TransportNone {
				t.Errorf("observation = %+v", st)
			}
		})
	}
}

// TestBodyCap: the tester reads at most MaxResponseBytes of a body; past
// it the step is too-large, however big the body.
func TestBodyCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		want acceptance.Transport
	}{
		{"at the cap", acceptance.MaxResponseBytes, acceptance.TransportNone},
		{"one past", acceptance.MaxResponseBytes + 1, acceptance.TransportTooLarge},
		{"huge", 64 << 20, acceptance.TransportTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				chunk := []byte(strings.Repeat("a", 32<<10))
				for left := tc.size; left > 0; left -= len(chunk) {
					if _, err := w.Write(chunk[:min(left, len(chunk))]); err != nil {
						return
					}
				}
			})
			c := criterion(t, `{"id":"big","kind":"http","statement":"s","target":"api","request":{"path":"/"}}`)
			start := time.Now()
			obs, _ := observe(t, c, comp)
			if time.Since(start) > 5*time.Second {
				t.Errorf("reading took %v: the cap must stop the read", time.Since(start))
			}
			if got := obs.Tries[0].Steps[0].TransportError; got != tc.want {
				t.Errorf("transport = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNoCookieJar: a cookie a component sets is never sent back.
func TestNoCookieJar(t *testing.T) {
	comp := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/"})
		_, _ = io.WriteString(w, `{}`)
	})
	c := criterion(t, `{"id":"two","kind":"http","statement":"s","target":"api","steps":[
	  {"request":{"path":"/a"}},{"request":{"path":"/b"}}]}`)
	observe(t, c, comp)
	for _, r := range comp.requests() {
		if r.Header.Get("Cookie") != "" {
			t.Errorf("%s carried Cookie %q", r.RequestURI, r.Header.Get("Cookie"))
		}
	}
}

// TestProxyEnvironmentIgnored: an HTTP_PROXY in the pod's environment
// cannot route the tester's requests anywhere else. net/http reads the
// proxy environment once per process, so the transport's configuration is
// pinned as well as its behaviour.
func TestProxyEnvironmentIgnored(t *testing.T) {
	if tr, ok := NewClient(Options{}).http.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the tester's transport consults a proxy")
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	comp := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "ok") })
	c := criterion(t, `{"id":"p","kind":"http","statement":"s","target":"api","request":{"path":"/"}}`)
	obs, d := observe(t, c, comp)
	if st := lastStep(obs); st.TransportError != acceptance.TransportNone || st.Status != 200 {
		t.Errorf("observation = %+v", st)
	}
	if got := d.addresses(); len(got) != 1 || got[0] != apiTarget {
		t.Errorf("dialled %v", got)
	}
}

// TestTransportClasses: a request that gets no response is classified,
// never an error: the controller judges it a fail.
func TestTransportClasses(t *testing.T) {
	closed := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}
	c := criterion(t, `{"id":"t","kind":"http","statement":"s","target":"api","request":{"path":"/"}}`)
	for _, tc := range []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request, n int)
		route   func(comp *component) string
		want    acceptance.Transport
	}{
		{"refused", nil, func(*component) string { return closed() }, acceptance.TransportRefused},
		{"dns", nil, func(*component) string { return "" }, acceptance.TransportDNS},
		{"timeout", func(w http.ResponseWriter, r *http.Request, _ int) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, listenerAddr, acceptance.TransportTimeout},
		{"reset", func(w http.ResponseWriter, _ *http.Request, _ int) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, listenerAddr, acceptance.TransportReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.handler
			if h == nil {
				h = func(http.ResponseWriter, *http.Request, int) {}
			}
			comp := newComponent(t, h)
			d := &dialer{routes: map[string]string{}}
			if to := tc.route(comp); to != "" {
				d.routes[apiTarget] = to
			}
			client := NewClient(Options{Dial: d.dial, Timeout: 200 * time.Millisecond})
			obs := acceptance.Observe(c, tokens, client.Doer(context.Background(), apiTarget))
			if len(obs.Tries) != 2 {
				t.Errorf("tries = %d, want the retry too", len(obs.Tries))
			}
			for _, try := range obs.Tries {
				if got := try.Steps[0].TransportError; got != tc.want {
					t.Errorf("transport = %q, want %q", got, tc.want)
				}
			}
			if v := acceptance.Judge(c, obs, tokens); v.Outcome != acceptance.OutcomeFail {
				t.Errorf("Judge = %s, want fail", v.Outcome)
			}
		})
	}
}

// TestRetry: a criterion that fails its first try re-runs once, whole,
// under a fresh {{run}}; the controller then judges it quarantined.
func TestRetry(t *testing.T) {
	comp := newComponent(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"b1"}`)
	})
	c := criterion(t, `{"id":"create","kind":"http","statement":"s","target":"api",
	  "request":{"method":"POST","path":"/battles","json":{"name":"maul-{{run}}"}},"expect":{"status":201}}`)
	obs, _ := observe(t, c, comp)
	reqs := comp.requests()
	if len(reqs) != 2 {
		t.Fatalf("component saw %d requests, want 2", len(reqs))
	}
	if reqs[0].Body != `{"name":"maul-v1a1t1-abcdef"}` || reqs[1].Body != `{"name":"maul-v1a1t2-abcdef"}` {
		t.Errorf("bodies = %s, %s: each try has its own run token", reqs[0].Body, reqs[1].Body)
	}
	if len(obs.Tries) != 2 || obs.Tries[0].Run != "v1a1t1-abcdef" || obs.Tries[1].Run != "v1a1t2-abcdef" {
		t.Errorf("tries = %+v", obs.Tries)
	}
	if v := acceptance.Judge(c, obs, tokens); v.Outcome != acceptance.OutcomeQuarantined {
		t.Errorf("Judge = %s, want quarantined", v.Outcome)
	}
}

// TestSubstitutionAcrossSteps: a captured value and the run token reach
// the next step's path, header and body.
func TestSubstitutionAcrossSteps(t *testing.T) {
	comp := newComponent(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch n {
		case 0:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"b-42"}`)
		default:
			_, _ = io.WriteString(w, `{"battle":"b-42","name":"maul-v1a1t1-abcdef"}`)
		}
	})
	c := criterion(t, `{"id":"flow","kind":"http","statement":"s","target":"api","steps":[
	  {"request":{"method":"POST","path":"/battles","json":{"name":"maul-{{run}}"}},
	   "expect":{"status":201},"capture":{"battle":"/id"}},
	  {"request":{"method":"PUT","path":"/battles/{{capture.battle}}","headers":{"X-Battle":"{{capture.battle}}"},
	    "body":"vote {{capture.battle}} {{run}}"},
	   "expect":{"json":{"/battle":"{{capture.battle}}","/name":"maul-{{run}}"}}}]}`)
	obs, _ := observe(t, c, comp)
	reqs := comp.requests()
	if len(reqs) != 2 {
		t.Fatalf("component saw %d requests", len(reqs))
	}
	r := reqs[1]
	if r.RequestURI != "/battles/b-42" || r.Header.Get("X-Battle") != "b-42" || r.Body != "vote b-42 v1a1t1-abcdef" {
		t.Errorf("second request = %+v", r)
	}
	if v := acceptance.Judge(c, obs, tokens); v.Outcome != acceptance.OutcomePass {
		t.Errorf("Judge = %+v, want pass", v)
	}
}
