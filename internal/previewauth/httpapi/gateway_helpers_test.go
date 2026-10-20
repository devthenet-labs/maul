// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/previewauth"
)

const (
	previewHost   = label + "." + suffix
	previewOrigin = "https://" + previewHost
	serviceAddr   = "preview-demo-7.maul-preview-1.svc:80"
)

// app is the preview's own code: an httptest server that records what it
// was sent and answers by path.
type app struct {
	*httptest.Server
	mu   sync.Mutex
	seen []*http.Request
}

func (a *app) requests() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*http.Request(nil), a.seen...)
}

func (a *app) last(t *testing.T) *http.Request {
	t.Helper()
	all := a.requests()
	if len(all) == 0 {
		t.Fatal("the preview was never called")
	}
	return all[len(all)-1]
}

func newApp(t *testing.T) *app {
	t.Helper()
	a := &app{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Clone(context.Background()))
		a.mu.Unlock()
		switch {
		case r.Header.Get("Upgrade") != "":
			echoUpgrade(w, r)
		case r.URL.Path == "/stream":
			stream(w, r)
		case r.URL.Path == "/hostile":
			h := w.Header()
			h.Add("Set-Cookie", previewauth.HostSessionCookie+"=forged; Path=/; Secure; HttpOnly")
			h.Add("Set-Cookie", "__HOST-maul-pv=forged; Path=/; Secure")
			h.Add("Set-Cookie", "app=1; Path=/")
			h.Set("Cache-Control", "public, s-maxage=600, max-age=60")
			h.Set("Clear-Site-Data", `"cookies", "storage"`)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Service-Worker-Allowed", "/")
			_, _ = io.WriteString(w, "hostile")
		case r.URL.Path == "/missing":
			w.Header().Set("Cache-Control", "public, max-age=600")
			http.NotFound(w, r)
		default:
			_, _ = fmt.Fprintf(w, "app %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	t.Cleanup(a.Close)
	return a
}

// stream is a server-sent event stream that writes until the client goes.
func stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	for i := 0; ; i++ {
		if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// echoUpgrade switches protocols and echoes every byte back.
func echoUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + r.Header.Get("Upgrade") +
		"\r\nConnection: Upgrade\r\nSet-Cookie: __Host-maul-pv=forged; Path=/; Secure\r\n\r\n")
	_ = brw.Flush()
	_, _ = io.Copy(conn, brw)
}

// gatewayEnv is a relay with the gateway on, served over plain HTTP on a
// real listener (streams and upgrades need one), and the preview's app
// behind it.
type gatewayEnv struct {
	*relay
	gw  *httptest.Server
	app *app
}

func newGatewayEnv(t *testing.T, editCfg func(*Config), edit func(*GatewayConfig)) *gatewayEnv {
	t.Helper()
	a := newApp(t)
	hosts, err := previewauth.NewHosts(suffix, relayHost)
	if err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != serviceAddr {
			return nil, fmt.Errorf("no such service %s", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, a.Listener.Addr().String())
	}
	r := newRelay(t, func(c *Config) {
		gc := &GatewayConfig{Hosts: hosts, Lifetimes: previewauth.DefaultGatewayLifetimes(),
			Transport: NewUpstreamTransport(DefaultUpstreamTimeouts(), dial)}
		if edit != nil {
			edit(gc)
		}
		c.Gateway = gc
		if editCfg != nil {
			editCfg(c)
		}
	})
	r.previews.set(servingView(r.previews.views[label]))
	srv := httptest.NewServer(r.handler)
	t.Cleanup(srv.Close)
	return &gatewayEnv{relay: r, gw: srv, app: a}
}

// servingView is v serving one component at "/".
func servingView(v previewauth.View) previewauth.View {
	v.Routes = []previewauth.Route{{Path: "/", Service: "preview-demo-7", Port: 80}}
	return v
}

// gwTransport sends every request for the relay host or a preview host to
// the gateway's listener with its Host kept, and anything else to the
// network. The cookie jar still sees the https URL, so Secure and __Host-
// cookies behave as in a browser.
type gwTransport struct {
	addr string
	ip   string
}

func (tr gwTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := req.URL.Hostname()
	if h != relayHost && h != "signin.example.com" && !strings.HasSuffix(h, "."+suffix) {
		return http.DefaultTransport.RoundTrip(req)
	}
	out := req.Clone(req.Context())
	out.Host = req.URL.Host
	out.URL.Scheme, out.URL.Host = "http", tr.addr
	if tr.ip != "" {
		out.Header.Set("X-Forwarded-For", tr.ip)
	}
	return http.DefaultTransport.RoundTrip(out)
}

// browser is a client with a jar that does not follow redirects.
func (e *gatewayEnv) browser() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return &http.Client{Jar: jar, Transport: gwTransport{addr: e.gw.Listener.Addr().String()},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// do sends one request as a browser would: a navigation unless hdr says
// otherwise.
func do(t *testing.T, b *http.Client, method, u string, hdr http.Header, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	for k, v := range hdr {
		if len(v) == 0 || v[0] == "" {
			req.Header.Del(k)
			continue
		}
		req.Header[k] = v
	}
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func nav(t *testing.T, b *http.Client, u string) *http.Response {
	t.Helper()
	return do(t, b, http.MethodGet, u, nil, nil)
}

// redirect checks a 302 (or Dex's 303) and returns its Location.
func redirect(t *testing.T, resp *http.Response, prefix string) string {
	t.Helper()
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(loc, prefix) {
		t.Fatalf("status %d to %q, want a 302 to %s...", resp.StatusCode, loc, prefix)
	}
	return loc
}

// toCallback drives a browser from a preview host to the handoff callback
// URL (not followed): through /start and, without a relay session, the stub
// Dex.
func (e *gatewayEnv) toCallback(t *testing.T, b *http.Client, path string) string {
	t.Helper()
	return e.toCallbackAt(t, b, previewOrigin, path)
}

// toCallbackAt is toCallback for any preview host's origin.
func (e *gatewayEnv) toCallbackAt(t *testing.T, b *http.Client, origin, path string) string {
	t.Helper()
	start := redirect(t, nav(t, b, origin+path), "https://"+e.cfgSignIn()+"/start?")
	next := redirect(t, nav(t, b, start), "https://")
	if strings.HasPrefix(next, origin+previewauth.HandoffPath+"?") {
		return next
	}
	dexURL, err := url.Parse(next)
	if err != nil || dexURL.Host != "dex.example.com" {
		t.Fatalf("/start sent the browser to %q", next)
	}
	cb := "https://" + e.cfgSignIn() + "/dex/callback?" +
		url.Values{"state": {dexURL.Query().Get("state")}, "code": {"ok"}}.Encode()
	return redirect(t, nav(t, b, cb), origin+previewauth.HandoffPath+"?")
}

func (e *gatewayEnv) cfgSignIn() string { return e.srv.cfg.Gateway.Hosts.SignIn() }

// signInPreview signs a browser in to the preview host and returns where
// the handoff sent it.
func (e *gatewayEnv) signInPreview(t *testing.T, b *http.Client, path string) string {
	t.Helper()
	return redirect(t, nav(t, b, e.toCallback(t, b, path)), previewOrigin)
}

// rawUpgrade sends an Upgrade request over its own connection to the
// gateway and returns the connection and the response head.
func (e *gatewayEnv) rawUpgrade(t *testing.T, b *http.Client,
	hdr http.Header) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", e.gw.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequest(http.MethodGet, previewOrigin+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	for k, v := range hdr {
		req.Header[k] = v
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	return conn, br, resp
}

// closedWithin reports whether reading r ends (EOF or an error) within d.
func closedWithin(r io.Reader, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

var errNoCookie = errors.New("no such cookie")

// hostCookie returns the browser's host session cookie for the preview
// host.
func hostCookie(b *http.Client) (string, error) {
	u, _ := url.Parse(previewOrigin + "/")
	for _, c := range b.Jar.Cookies(u) {
		if c.Name == previewauth.HostSessionCookie {
			return c.Value, nil
		}
	}
	return "", errNoCookie
}
