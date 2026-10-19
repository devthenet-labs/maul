// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/previewauth"
)

// The same-site attacker: preview code on <x>.<suffix> when the sign-in host
// shares the suffix's registrable domain. Its background requests to the
// sign-in host carry the relay session's SameSite=Lax cookie, so every
// endpoint that reads that session or sets a cookie refuses them; a
// navigation, same-site or not, still signs in.

// background is what a page can send another host of its site without the
// viewer opening it: a fetch, an image and an iframe.
var background = map[string]http.Header{
	"fetch":  {"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}},
	"image":  {"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"no-cors"}, "Sec-Fetch-Dest": {"image"}},
	"iframe": {"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"iframe"}},
}

var sameSiteNavigation = http.Header{"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"navigate"},
	"Sec-Fetch-Dest": {"document"}}

// send is one GET from b with exactly hdr's Sec-Fetch headers.
func send(t *testing.T, b *http.Client, u string, hdr http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// refusedQuietly checks a 403 page with no Location and no cookie.
func refusedQuietly(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" ||
		len(resp.Header.Values("Set-Cookie")) > 0 {
		t.Errorf("%s: status %d, Location %q, Set-Cookie %q; want a 403 page with neither", what,
			resp.StatusCode, resp.Header.Get("Location"), resp.Header.Values("Set-Cookie"))
	}
}

// TestRelayRefusesSameSiteBackgroundRequests: /authorize and /dex/callback
// answer a signed-in viewer's background request from a same-site preview
// with a 403 page, never a code or a cookie. Before the rule, /authorize
// redirected a code to the preview callback and, signed out, set a login
// cookie on the relay host.
func TestRelayRefusesSameSiteBackgroundRequests(t *testing.T) {
	r := newRelay(t, nil)
	signedIn, signedOut := r.signedIn(), r.browser()
	for kind, hdr := range background {
		refusedQuietly(t, "signed-in authorize by "+kind, send(t, signedIn, authorizeURL(nil), hdr))
		refusedQuietly(t, "signed-out authorize by "+kind, send(t, signedOut, authorizeURL(nil), hdr))
		cb := issuer + "/dex/callback?" + url.Values{"state": {"x"}, "code": {"ok"}}.Encode()
		refusedQuietly(t, "callback by "+kind, send(t, signedIn, cb, hdr))
	}
	resp := send(t, signedIn, authorizeURL(nil), sameSiteNavigation)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, callback) {
		t.Errorf("a same-site navigation to authorize: %d to %q, want a code at the callback", resp.StatusCode, loc)
	}
	_ = resp.Body.Close()
}

// TestGatewayStartRefusesSameSiteBackgroundRequests: /start, on the issuer's
// host and on a sign-in host of its own, never issues a handoff code or
// starts a sign-in for a background request, and the separate sign-in
// host's /dex/callback refuses one too. A same-site navigation still gets
// its handoff.
func TestGatewayStartRefusesSameSiteBackgroundRequests(t *testing.T) {
	const ownHost = "signin.example.com"
	for name, edit := range map[string]func(*testing.T) func(*GatewayConfig){
		"issuer's host": func(*testing.T) func(*GatewayConfig) { return nil },
		"own host": func(t *testing.T) func(*GatewayConfig) {
			return func(g *GatewayConfig) {
				h, err := previewauth.NewHosts(suffix, ownHost)
				if err != nil {
					t.Fatal(err)
				}
				g.Hosts = h
				g.SignInUpstream = &stubUpstream{id: previewauth.Identity{Username: "github:alice"}}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newGatewayEnv(t, nil, edit(t))
			viewer := e.browser()
			e.signInPreview(t, viewer, "/")
			// A fresh /start URL for the label, as the preview host hands one out.
			start := redirect(t, nav(t, e.browser(), previewOrigin+"/"), "https://"+e.cfgSignIn()+"/start?")
			for kind, hdr := range background {
				refusedQuietly(t, "start by "+kind, send(t, viewer, start, hdr))
				refusedQuietly(t, "signed-out start by "+kind, send(t, e.browser(), start, hdr))
				cb := "https://" + e.cfgSignIn() + "/dex/callback?" + url.Values{"state": {"x"}, "code": {"ok"}}.Encode()
				refusedQuietly(t, "callback by "+kind, send(t, viewer, cb, hdr))
			}
			redirect(t, send(t, viewer, start, sameSiteNavigation), previewOrigin+previewauth.HandoffPath+"?")
		})
	}
}
