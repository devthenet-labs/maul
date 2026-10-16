// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// TestGatewayHandoffFlow drives a viewer from a preview host through
// /start and Dex and back, and checks what the preview sees and what the
// viewer gets back from it.
func TestGatewayHandoffFlow(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	b := e.browser()

	// An unauthenticated navigation: a state cookie and a 302 to /start.
	resp := nav(t, b, previewOrigin+"/dash?tab=2")
	start := redirect(t, resp, "https://"+relayHost+"/start?h="+label+"&s=pa1.t.")
	if len(resp.Cookies()) != 1 || !strings.HasPrefix(resp.Cookies()[0].Name, "__Host-patchy-pv-state-") ||
		!resp.Cookies()[0].HttpOnly || !resp.Cookies()[0].Secure || resp.Cookies()[0].MaxAge != 600 {
		t.Fatalf("state cookie = %+v", resp.Cookies())
	}
	if len(e.app.requests()) != 0 {
		t.Fatal("an unauthenticated request reached the preview")
	}
	_ = start

	back := e.signInPreview(t, b, "/dash?tab=2")
	if back != previewOrigin+"/dash?tab=2" {
		t.Fatalf("returned to %q", back)
	}
	if _, err := hostCookie(b); err != nil {
		t.Fatal("no host session after the handoff")
	}

	// The proxied request: the viewer's forged identity headers and the
	// gateway's cookies never reach the preview; the pairwise subject does.
	resp = do(t, b, http.MethodGet, back, http.Header{
		"X-Patchy-Viewer": {"forged"}, "X-Forwarded-User": {"admin"}, "X-Amzn-Oidc-Data": {"forged"},
		"X-Forwarded-For": {"10.0.0.1"}, "Authorization": {"Bearer app-token"}, "Cookie": {"app=1"},
	}, nil)
	if got := body(t, resp); resp.StatusCode != 200 || got != "app GET /dash?tab=2" {
		t.Fatalf("proxied: %d %q", resp.StatusCode, got)
	}
	checkForwarded(t, e, e.app.last(t))
}

// checkForwarded checks what the preview was sent: the pairwise subject and
// the gateway's own forwarding headers, its own Authorization, and nothing
// the viewer forged or the gateway keeps.
func checkForwarded(t *testing.T, e *gatewayEnv, seen *http.Request) {
	t.Helper()
	wantSub := e.ring.Sub(previewauth.ClientID(1), "uid-7", label, "github:alice")
	if seen.Header.Get("X-Patchy-Viewer") != wantSub || len(seen.Header.Values("X-Patchy-Viewer")) != 1 {
		t.Errorf("X-Patchy-Viewer = %q, want the pairwise %q", seen.Header.Values("X-Patchy-Viewer"), wantSub)
	}
	for _, h := range []string{"X-Forwarded-User", "X-Amzn-Oidc-Data"} {
		if seen.Header.Get(h) != "" {
			t.Errorf("the preview saw %s: %q", h, seen.Header.Get(h))
		}
	}
	if seen.Header.Get("X-Forwarded-For") != "127.0.0.1" || seen.Header.Get("X-Forwarded-Proto") != "https" ||
		seen.Header.Get("X-Forwarded-Host") != previewHost || seen.Host != previewHost {
		t.Errorf("forwarding headers: XFF %q proto %q host %q Host %q", seen.Header.Get("X-Forwarded-For"),
			seen.Header.Get("X-Forwarded-Proto"), seen.Header.Get("X-Forwarded-Host"), seen.Host)
	}
	if seen.Header.Get("Authorization") != "Bearer app-token" {
		t.Errorf("the preview's own Authorization was not passed: %q", seen.Header.Get("Authorization"))
	}
	if c := seen.Header.Get("Cookie"); strings.Contains(strings.ToLower(c), "patchy") || !strings.Contains(c, "app=1") {
		t.Errorf("the preview's Cookie header: %q", c)
	}
	for _, r := range e.app.requests() {
		if strings.HasPrefix(r.URL.Path, "/.patchy") || r.URL.Query().Has("code") {
			t.Errorf("the preview saw %s", r.URL)
		}
	}
}

// TestGatewayFlowWithFakeDex runs the whole sign-in with the real Dex
// adapter against an in-memory Dex, then shows the relay session is reused
// for a second preview without Dex.
func TestGatewayFlowWithFakeDex(t *testing.T) {
	r, d := newFlowRelay(t)
	up := r.upstream
	e := newGatewayEnv(t, func(c *Config) { c.Upstream = up }, nil)
	b := e.browser()
	start := redirect(t, nav(t, b, previewOrigin+"/"), "https://"+relayHost+"/start?")
	dexAuth := redirect(t, nav(t, b, start), d.URL+"/auth?")
	back := redirect(t, nav(t, b, dexAuth), "https://"+relayHost+"/dex/callback?")
	cb := redirect(t, nav(t, b, back), previewOrigin+previewauth.HandoffPath+"?code=pa1.o.")
	redirect(t, nav(t, b, cb), previewOrigin+"/")
	if resp := nav(t, b, previewOrigin+"/"); resp.StatusCode != 200 || body(t, resp) != "app GET /" {
		t.Fatalf("after sign-in: %d", resp.StatusCode)
	}
	if got := e.viewers.asked[len(e.viewers.asked)-1]; got.Username != "github:alice" || got.Project != "demo" ||
		got.Subresource != "previews" {
		t.Errorf("access review = %+v", got)
	}

	// A second Preview: the relay session is reused, Dex is not asked.
	e.previews.set(previewauth.View{UID: "uid-8", Label: "other-8", Project: "demo", Slot: 0, Live: true})
	n := d.Authorizations()
	other := "https://other-8." + suffix
	start = redirect(t, nav(t, b, other+"/"), "https://"+relayHost+"/start?h=other-8&")
	redirect(t, nav(t, b, start), other+previewauth.HandoffPath+"?code=")
	if d.Authorizations() != n {
		t.Error("a second preview went back to Dex")
	}
}

// TestGatewayKeepsTheRelay: with the gateway on, the provider endpoints
// still answer on the issuer host, the ALB sign-in works, and nothing of the
// gateway answers there but /start.
func TestGatewayKeepsTheRelay(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	b := e.browser()
	d := get(t, b, issuer+"/.well-known/openid-configuration")
	if d.StatusCode != 200 || !strings.Contains(body(t, d), `"issuer":"`+issuer+`"`) {
		t.Fatalf("discovery: %d", d.StatusCode)
	}
	if d.Header.Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Error("a relay page has no COOP with the gateway on")
	}
	rr := get(t, b, authorizeURL(nil))
	dexURL, err := url.Parse(redirect(t, rr, "https://dex.example.com/"))
	if err != nil {
		t.Fatal(err)
	}
	cb := get(t, b, issuer+"/dex/callback?"+url.Values{"state": {dexURL.Query().Get("state")}, "code": {"ok"}}.Encode())
	code, _ := codeFrom(t, cb)
	tok := decodeToken(t, e.tokenRequest("basic", 1, e.ring.ClientSecret(1), url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {callback}}))
	if tok.AccessToken == "" {
		t.Fatalf("token: %+v", tok)
	}
}

// TestGatewaySeparateSignInHost: a sign-in host of its own serves /start,
// its own Dex callback, the home page and logout, and no provider endpoint.
func TestGatewaySeparateSignInHost(t *testing.T) {
	const signIn = "signin.example.com"
	stub := &stubUpstream{id: previewauth.Identity{Username: "github:alice"}}
	e := newGatewayEnv(t, nil, func(g *GatewayConfig) {
		h, err := previewauth.NewHosts(suffix, signIn)
		if err != nil {
			t.Fatal(err)
		}
		g.Hosts, g.SignInUpstream = h, stub
	})
	b := e.browser()
	redirect(t, nav(t, b, e.toCallback(t, b, "/x")), previewOrigin+"/x")
	for _, p := range []string{"/authorize", "/token", "/userinfo", "/jwks", "/.well-known/openid-configuration"} {
		resp := nav(t, b, "https://"+signIn+p)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s on the sign-in host: %d", p, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if resp := nav(t, b, "https://"+signIn+"/"); resp.StatusCode != 200 {
		t.Errorf("sign-in home: %d", resp.StatusCode)
	}
	// A gateway login state presented to the issuer host is not one of its
	// sign-ins.
	resp := nav(t, b, e.toCallbackStateOnly(t, b))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a gateway login at the issuer's callback: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// toCallbackStateOnly starts a gateway sign-in on a fresh browser and
// returns the issuer host's Dex callback with its state.
func (e *gatewayEnv) toCallbackStateOnly(t *testing.T, _ *http.Client) string {
	t.Helper()
	b := e.browser()
	start := redirect(t, nav(t, b, previewOrigin+"/"), "https://")
	dexURL, err := url.Parse(redirect(t, nav(t, b, start), "https://dex.example.com/"))
	if err != nil {
		t.Fatal(err)
	}
	return issuer + "/dex/callback?" + url.Values{"state": {dexURL.Query().Get("state")}, "code": {"ok"}}.Encode()
}

// TestGatewayFixation: a handoff code is honoured only in the browser that
// started its sign-in on that host, and only once.
func TestGatewayFixation(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	attacker := e.browser()
	code := e.toCallback(t, attacker, "/")

	// The victim, who never started this sign-in, is sent the code.
	victim := e.browser()
	redirect(t, nav(t, victim, previewOrigin+"/"), "https://") // the victim's own sign-in in flight
	resp := nav(t, victim, code)
	if resp.StatusCode != http.StatusBadRequest || len(resp.Cookies()) != 0 {
		t.Fatalf("fixation: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	_ = resp.Body.Close()
	if _, err := hostCookie(victim); err == nil {
		t.Fatal("the victim was signed in as the attacker")
	}

	// The code was not spent: its own browser can still use it, once.
	jarCopy, _ := cookiejar.New(nil)
	u, _ := url.Parse(previewOrigin + "/")
	jarCopy.SetCookies(u, attacker.Jar.Cookies(u))
	redirect(t, nav(t, attacker, code), previewOrigin+"/")
	replay := &http.Client{Jar: jarCopy, Transport: attacker.Transport, CheckRedirect: attacker.CheckRedirect}
	resp = nav(t, replay, code)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a replayed code: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// A code for another preview host is not this host's.
	other := e.browser()
	e.previews.set(servingView(previewauth.View{UID: "uid-8", Label: "other-8", Project: "demo", Slot: 0,
		Live: true}))
	otherOrigin := "https://other-8." + suffix
	cb := e.toCallbackAt(t, other, otherOrigin, "/")
	resp = nav(t, other, strings.Replace(cb, otherOrigin, previewOrigin, 1))
	if resp.StatusCode == http.StatusFound || len(resp.Cookies()) != 0 {
		t.Errorf("another host's code: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	_ = resp.Body.Close()
}

// TestGatewayLoginCSRF: the Dex callback refuses a gateway sign-in this
// browser did not start.
func TestGatewayLoginCSRF(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	cb := e.toCallbackStateOnly(t, nil)
	stranger := e.browser()
	resp := nav(t, stranger, cb)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a stranger's Dex callback: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == previewauth.RelaySessionCookie && c.MaxAge >= 0 {
			t.Error("a stranger's Dex callback set a relay session")
		}
	}
	_ = resp.Body.Close()
}

// TestGatewayUnauthenticated: only top-level navigations start a sign-in;
// everything else is a 401 with no cookie; unknown and dead labels look the
// same; the reserved prefix is never forwarded.
func TestGatewayUnauthenticated(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	b := e.browser()
	for name, hdr := range map[string]http.Header{
		"fetch":        {"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}, "Sec-Fetch-Site": {"same-origin"}},
		"iframe":       {"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}},
		"xhr, no meta": {"Sec-Fetch-Mode": {""}, "Sec-Fetch-Dest": {""}, "Sec-Fetch-Site": {""}},
	} {
		resp := do(t, b, http.MethodGet, previewOrigin+"/api", hdr, nil)
		if resp.StatusCode != http.StatusUnauthorized || len(resp.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: %d, Set-Cookie %v", name, resp.StatusCode, resp.Header.Values("Set-Cookie"))
		}
		_ = resp.Body.Close()
	}
	resp := do(t, b, http.MethodGet, previewOrigin+"/", http.Header{"Sec-Fetch-Mode": {""}, "Sec-Fetch-Dest": {""},
		"Sec-Fetch-Site": {""}, "Accept": {"text/html"}}, nil)
	redirect(t, resp, "https://"+relayHost+"/start?")

	e.previews.set(previewauth.View{UID: "uid-9", Label: "dead-9", Project: "demo", Slot: 0})
	unknown := nav(t, b, "https://nobody-1."+suffix+"/")
	dead := nav(t, b, "https://dead-9."+suffix+"/")
	if unknown.StatusCode != 404 || dead.StatusCode != 404 || body(t, unknown) != body(t, dead) {
		t.Errorf("unknown %d and dead %d labels answer differently", unknown.StatusCode, dead.StatusCode)
	}
	for _, host := range []string{previewHost, "nobody-1." + suffix, relayHost, "x"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+e.gw.Listener.Addr().String()+previewauth.HealthPath, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || body(t, resp) != "ok\n" {
			t.Errorf("healthz on %q: %d", host, resp.StatusCode)
		}
	}

	e.signInPreview(t, b, "/")
	for _, p := range []string{"/.patchy/anything", "/.patchy", "/%2Epatchy/callback?code=x", "/a/../.patchy/x"} {
		resp := nav(t, b, previewOrigin+p)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s answered 200", p)
		}
	}
	for _, r := range e.app.requests() {
		if previewauth.ReservedPath(r.URL.Path) {
			t.Errorf("the preview saw the reserved %s", r.URL.Path)
		}
	}
	if resp := do(t, b, http.MethodGet, previewOrigin+"/sw.js", http.Header{"Service-Worker": {"script"},
		"Sec-Fetch-Mode": {"same-origin"}, "Sec-Fetch-Dest": {"serviceworker"}, "Sec-Fetch-Site": {"same-origin"}},
		nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a service worker script: %d", resp.StatusCode)
	}
}

// TestGatewayRateLimit: unauthenticated hits are limited, signed-in
// traffic is not.
func TestGatewayRateLimit(t *testing.T) {
	e := newGatewayEnv(t, nil, func(g *GatewayConfig) { g.RateLimit = RateLimit{PerSecond: 0.001, Burst: 6} })
	b := e.browser()
	e.signInPreview(t, b, "/")
	for i := range 20 {
		if resp := nav(t, b, previewOrigin+"/asset"); resp.StatusCode != 200 {
			t.Fatalf("signed-in request %d: %d", i, resp.StatusCode)
		}
	}
	stranger := e.browser()
	limited := false
	for range 10 {
		resp := do(t, stranger, http.MethodGet, previewOrigin+"/api", http.Header{"Sec-Fetch-Mode": {"cors"}}, nil)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("unauthenticated requests were never limited")
	}
}

// TestGatewayCrossOrigin: cross-preview CSRF and same-site subresources are
// refused before the preview sees them; the host's own requests and
// top-level navigations from elsewhere pass.
func TestGatewayCrossOrigin(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	b := e.browser()
	e.signInPreview(t, b, "/")
	evil := "https://evil-1." + suffix
	before := len(e.app.requests())
	for name, tc := range map[string]struct {
		method string
		hdr    http.Header
	}{
		"form post from another preview": {"POST", http.Header{"Origin": {evil}, "Sec-Fetch-Site": {"same-site"}}},
		"post with no origin":            {"POST", http.Header{"Origin": {""}, "Sec-Fetch-Site": {""}}},
		"same-site image": {"GET", http.Header{"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"no-cors"},
			"Sec-Fetch-Dest": {"image"}}},
		"cross-site fetch": {"GET", http.Header{"Sec-Fetch-Site": {"cross-site"}, "Sec-Fetch-Mode": {"cors"}}},
		"delete from another origin": {"DELETE", http.Header{"Origin": {evil}, "Sec-Fetch-Site": {""},
			"Sec-Fetch-Mode": {""}}},
	} {
		resp := do(t, b, tc.method, previewOrigin+"/api", tc.hdr, strings.NewReader("x=1"))
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if got := len(e.app.requests()); got != before {
		t.Errorf("%d refused requests reached the preview", got-before)
	}
	resp := do(t, b, "POST", previewOrigin+"/api", http.Header{"Origin": {previewOrigin},
		"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}}, strings.NewReader("x=1"))
	if resp.StatusCode != 200 {
		t.Errorf("the preview's own POST: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp = do(t, b, "GET", previewOrigin+"/", http.Header{"Sec-Fetch-Site": {"same-site"}}, nil)
	if resp.StatusCode != 200 {
		t.Errorf("a top-level navigation from another preview: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestGatewayCrossPreviewWebsocket: another preview's page cannot open a
// WebSocket to this one, with or without Sec-Fetch; the host's own can, and
// the upgraded response's Set-Cookie for the session is still dropped.
func TestGatewayCrossPreviewWebsocket(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	b := e.browser()
	e.signInPreview(t, b, "/")
	evil := "https://evil-1." + suffix
	for name, hdr := range map[string]http.Header{
		"sec-fetch":    {"Origin": {evil}, "Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"websocket"}},
		"no sec-fetch": {"Origin": {evil}},
		"no origin":    {},
	} {
		_, _, resp := e.rawUpgrade(t, b, hdr)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	conn, br, resp := e.rawUpgrade(t, b, http.Header{"Origin": {previewOrigin}, "Sec-Fetch-Site": {"same-origin"},
		"Sec-Fetch-Mode": {"websocket"}})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("own websocket: %d", resp.StatusCode)
	}
	if v := resp.Header.Values("Set-Cookie"); len(v) != 0 {
		t.Errorf("the upgrade passed Set-Cookie %v", v)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q, %v", buf, err)
	}
}

// TestGatewayResponseHygiene: a hostile preview cannot set or clear the
// gateway's cookies, clear site data, allow credentialed CORS, widen a
// service worker or be cached by a shared cache.
func TestGatewayResponseHygiene(t *testing.T) {
	e := newGatewayEnv(t, nil, func(g *GatewayConfig) { g.HSTS = true })
	b := e.browser()
	e.signInPreview(t, b, "/")
	session, _ := hostCookie(b)
	resp := nav(t, b, previewOrigin+"/hostile")
	_ = body(t, resp)
	h := resp.Header
	if got := h.Values("Set-Cookie"); len(got) != 1 || got[0] != "app=1; Path=/" {
		t.Errorf("Set-Cookie = %v", got)
	}
	for _, name := range []string{"Clear-Site-Data", "Access-Control-Allow-Credentials", "Service-Worker-Allowed"} {
		if h.Get(name) != "" {
			t.Errorf("%s passed: %q", name, h.Get(name))
		}
	}
	if h.Get("Cache-Control") != "max-age=60, private" || !strings.Contains(strings.Join(h.Values("Vary"), ","),
		"Cookie") {
		t.Errorf("Cache-Control %q, Vary %v", h.Get("Cache-Control"), h.Values("Vary"))
	}
	if h.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Errorf("HSTS = %q", h.Get("Strict-Transport-Security"))
	}
	if h.Get("Content-Security-Policy") != "" {
		t.Error("the relay's CSP was put on a proxied response")
	}
	if now, _ := hostCookie(b); now != session {
		t.Error("the preview replaced the host session")
	}
	resp = nav(t, b, previewOrigin+"/missing")
	_ = body(t, resp)
	if resp.StatusCode != 404 || resp.Header.Get("Cache-Control") != "max-age=600, private, no-store" {
		t.Errorf("a 404: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	// The gateway's own pages: never cached, never framed, no opener.
	page := nav(t, b, "https://nobody-1."+suffix+"/")
	_ = body(t, page)
	if page.Header.Get("Cache-Control") != "no-store" || page.Header.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
		page.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("gateway page headers: %v", page.Header)
	}
}

// TestGatewayRevocation: a revoked viewer's next request is refused and its
// cookie cleared; a replaced Preview starts a new sign-in; a transient
// failure keeps the cookie.
func TestGatewayRevocation(t *testing.T) {
	e := newGatewayEnv(t, nil, nil)
	b := e.browser()
	e.signInPreview(t, b, "/")

	e.viewers.setErr(errors.New("api server down"))
	resp := nav(t, b, previewOrigin+"/")
	if resp.StatusCode != http.StatusServiceUnavailable || len(resp.Cookies()) != 0 {
		t.Errorf("a failed review: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	_ = resp.Body.Close()
	e.viewers.setErr(nil)

	e.previews.set(servingView(previewauth.View{UID: "uid-new", Label: label, Project: "demo", Slot: 1, Live: true}))
	resp = nav(t, b, previewOrigin+"/again")
	redirect(t, resp, "https://"+relayHost+"/start?")
	if _, err := hostCookie(b); err == nil {
		t.Error("the session for the old Preview was kept")
	}
	e.signInPreview(t, b, "/")

	e.viewers.revoke("github:alice")
	resp = nav(t, b, previewOrigin+"/")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a revoked viewer: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if _, err := hostCookie(b); err == nil {
		t.Error("a revoked viewer kept the session cookie")
	}
}

// openStream opens /stream as the signed-in browser and reads its first
// event.
func openStream(t *testing.T, b *http.Client) *http.Response {
	t.Helper()
	resp := do(t, b, http.MethodGet, previewOrigin+"/stream", http.Header{"Sec-Fetch-Mode": {"cors"},
		"Sec-Fetch-Dest": {"empty"}, "Sec-Fetch-Site": {"same-origin"}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first event %q, %v", line, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestGatewayStreamRevocation: a live stream and an upgraded connection are
// closed when a re-check refuses the viewer, when the Preview changes, and
// at the session's absolute end.
func TestGatewayStreamRevocation(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		e := newGatewayEnv(t, nil, nil)
		b := e.browser()
		e.signInPreview(t, b, "/")
		s := openStream(t, b)
		conn, br, resp := e.rawUpgrade(t, b, http.Header{"Origin": {previewOrigin}})
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("upgrade: %d", resp.StatusCode)
		}
		_ = conn
		e.viewers.revoke("github:bob")
		e.srv.Recheck(context.Background())
		if closedWithin(s.Body, 200*time.Millisecond) {
			t.Fatal("a re-check that passed closed the stream")
		}
		e.viewers.revoke("github:alice")
		e.srv.Recheck(context.Background())
		if !closedWithin(s.Body, 2*time.Second) {
			t.Error("the stream outlived the revocation")
		}
		if !closedWithin(br, 2*time.Second) {
			t.Error("the upgraded connection outlived the revocation")
		}
		if n := e.srv.gw.streams.len(); n != 0 {
			t.Errorf("%d connections still tracked", n)
		}
	})
	t.Run("transient failure keeps it", func(t *testing.T) {
		e := newGatewayEnv(t, nil, nil)
		b := e.browser()
		e.signInPreview(t, b, "/")
		s := openStream(t, b)
		e.viewers.setErr(errors.New("api server down"))
		e.previews.fail(errors.New("cache down"))
		e.srv.Recheck(context.Background())
		if closedWithin(s.Body, 300*time.Millisecond) {
			t.Error("a transient failure closed the stream")
		}
	})
	t.Run("preview changed", func(t *testing.T) {
		e := newGatewayEnv(t, nil, nil)
		b := e.browser()
		e.signInPreview(t, b, "/")
		s := openStream(t, b)
		e.srv.PreviewChanged(context.Background(), "other-1")
		if closedWithin(s.Body, 200*time.Millisecond) {
			t.Fatal("another Preview's change closed the stream")
		}
		e.previews.set(servingView(previewauth.View{UID: "uid-new", Label: label, Project: "demo", Slot: 1,
			Live: true}))
		e.srv.PreviewChanged(context.Background(), label)
		if !closedWithin(s.Body, 2*time.Second) {
			t.Error("the stream outlived its Preview")
		}
	})
	t.Run("session end", func(t *testing.T) {
		e := newGatewayEnv(t, nil, func(g *GatewayConfig) { g.Lifetimes.HostSession = time.Minute })
		// Every gateway token is minted 57 s in the past, so the host
		// session ends three seconds from now on the wall clock.
		e.clock.advance(-57 * time.Second)
		b := e.browser()
		e.signInPreview(t, b, "/")
		s := openStream(t, b)
		if !closedWithin(s.Body, 5*time.Second) {
			t.Error("the stream outlived the host session")
		}
	})
}

// TestGatewayLimits: the inbound allowlist (after the trusted hops), the
// body limit, and a Preview that is live but not serving.
func TestGatewayLimits(t *testing.T) {
	e := newGatewayEnv(t, nil, func(g *GatewayConfig) {
		g.InboundCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
		g.RateLimit.ForwardedHops = 1
		g.BodyLimit = 16
	})
	for ip, want := range map[string]int{"203.0.113.7": 403, "198.51.100.7": 302} {
		b := e.browser()
		b.Transport = gwTransport{addr: e.gw.Listener.Addr().String(), ip: ip}
		resp := nav(t, b, previewOrigin+"/")
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("from %s: %d, want %d", ip, resp.StatusCode, want)
		}
	}
	// A spoofed leftmost entry does not help: the edge appends the real one.
	spoof := e.browser()
	spoof.Transport = gwTransport{addr: e.gw.Listener.Addr().String(), ip: "198.51.100.7, 203.0.113.7"}
	if resp := nav(t, spoof, previewOrigin+"/"); resp.StatusCode != 403 {
		t.Errorf("a spoofed X-Forwarded-For: %d", resp.StatusCode)
	}

	b := e.browser()
	b.Transport = gwTransport{addr: e.gw.Listener.Addr().String(), ip: "198.51.100.7"}
	e.signInPreview(t, b, "/")
	big := do(t, b, http.MethodPost, previewOrigin+"/upload", http.Header{"Origin": {previewOrigin}},
		strings.NewReader(strings.Repeat("x", 64)))
	if big.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a large body: %d", big.StatusCode)
	}
	_ = big.Body.Close()

	v := servingView(e.previews.views[label])
	v.Routes = nil
	e.previews.set(v)
	if resp := nav(t, b, previewOrigin+"/"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("not serving: %d", resp.StatusCode)
	}
}

func TestGatewayConfigRefusals(t *testing.T) {
	hosts := func(s, in string) previewauth.Hosts {
		h, err := previewauth.NewHosts(s, in)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for name, g := range map[string]GatewayConfig{
		"no hosts":        {Lifetimes: previewauth.DefaultGatewayLifetimes()},
		"another suffix":  {Hosts: hosts("other.example.com", relayHost), Lifetimes: previewauth.DefaultGatewayLifetimes()},
		"no Dex client":   {Hosts: hosts(suffix, "signin.example.com"), Lifetimes: previewauth.DefaultGatewayLifetimes()},
		"bad lifetimes":   {Hosts: hosts(suffix, relayHost)},
		"negative limits": {Hosts: hosts(suffix, relayHost), Lifetimes: previewauth.DefaultGatewayLifetimes(), BodyLimit: -1},
	} {
		r := newRelay(t, nil)
		cfg := r.srv.cfg
		cfg.Gateway = &g
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
