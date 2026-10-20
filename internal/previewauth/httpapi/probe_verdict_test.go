// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpapi

import (
	"io"
	"net/http"
	"net/netip"
	"testing"

	"github.com/devthenet-labs/maul/internal/previewauth"
)

// TestGatewayProbeVerdictsMatchThePages holds the probes' verdicts
// (previewauth.ProbeExpect, JudgeGatewayUnknown, IsAllowlistRefusal) to the
// pages and redirects the gateway really answers with, as the relay's
// background probe and maul check project ask: no cookies, an HTML
// navigation without Sec-Fetch metadata, redirects not followed.
func TestGatewayProbeVerdictsMatchThePages(t *testing.T) {
	e := newGatewayEnv(t, nil, func(g *GatewayConfig) {
		g.InboundCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
		g.RateLimit.ForwardedHops = 1
	})
	probe := func(ip, host string) (*http.Response, []byte) {
		t.Helper()
		b := e.browser()
		b.Jar = nil
		b.Transport = gwTransport{addr: e.gw.Listener.Addr().String(), ip: ip}
		req, err := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "text/html")
		resp, err := b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, data
	}
	expect := previewauth.ProbeExpect{Mode: previewauth.ProbeGateway, Gateway: e.srv.cfg.Gateway.Hosts}

	live, _ := probe("198.51.100.7", previewHost)
	if err := expect.Live(live.StatusCode, live.Header.Get("Location"), 1, label); err != nil {
		t.Errorf("a live host: %v (%d %q)", err, live.StatusCode, live.Header.Get("Location"))
	}
	unknown, page := probe("198.51.100.7", previewauth.UnknownProbeLabel+"."+suffix)
	if err := previewauth.JudgeGatewayUnknown(unknown.StatusCode, unknown.Header, page); err != nil {
		t.Errorf("an unknown host: %v (%d %s)", err, unknown.StatusCode, page)
	}
	if previewauth.IsAllowlistRefusal(unknown.StatusCode, unknown.Header, page) {
		t.Error("the 404 was taken for the allowlist page")
	}
	refused, page := probe("203.0.113.7", previewauth.UnknownProbeLabel+"."+suffix)
	if !previewauth.IsAllowlistRefusal(refused.StatusCode, refused.Header, page) {
		t.Errorf("outside the allowlist: %d %s", refused.StatusCode, page)
	}
	if previewauth.JudgeGatewayUnknown(refused.StatusCode, refused.Header, page) == nil {
		t.Error("the allowlist page was taken for the 404")
	}
}
