// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// TestPolicyRefusalsPinnedToTheWire: the job controllers end an intent build
// without another attempt when its agent reports one of the proxy's policy
// refusals (netprofile.PolicyRefusal), so every phrase that function reads
// is pinned here to what this proxy actually writes, by driving the proxy
// to it: each policy refusal on the wire is read as one, of its kind, and
// the transient ones are not. A reworded refusal fails here rather than
// quietly sending its builds back to a second attempt.
func TestPolicyRefusalsPinnedToTheWire(t *testing.T) {
	tunnel := []struct {
		name, line string
		kind       netprofile.RefusalKind
		host       string
	}{
		{"not in the profile's hosts", "CONNECT other.registry.dev:443 HTTP/1.1", netprofile.RefusalHost,
			"other.registry.dev"},
		{"forge", "CONNECT github.com:443 HTTP/1.1", netprofile.RefusalNever, ""},
		{"ip literal", "CONNECT 1.2.3.4:443 HTTP/1.1", netprofile.RefusalNever, ""},
		{"port", "CONNECT pkg.registry.dev:80 HTTP/1.1", netprofile.RefusalNever, ""},
		{"HTTP/1.0 (not a policy refusal)", "CONNECT pkg.registry.dev:443 HTTP/1.0", 0, ""},
	}
	for _, tt := range tunnel {
		t.Run(tt.name, func(t *testing.T) {
			h := newTunnelHarness(t, Config{})
			c, br := h.connect(t, "web", tt.line)
			r := readRaw(t, br)
			_ = c.Close()
			got, ok := netprofile.PolicyRefusal(r.statusLine)
			if ok != (tt.kind != 0) || got.Kind != tt.kind || got.Host != tt.host {
				t.Errorf("PolicyRefusal(%q) = %+v, %v; want kind %d host %q", r.statusLine, got, ok, tt.kind, tt.host)
			}
		})
	}

	t.Run("go route allowlist", func(t *testing.T) {
		h := newHarness(t, Config{}, failingUpstream(t), failingUpstream(t))
		r := h.get(t, "/go/rsc.io/quote/@v/v1.5.2.info")
		line := "410 " + strings.TrimSpace(r.body)
		if got, ok := netprofile.PolicyRefusal(line); r.status != http.StatusGone || !ok ||
			got.Kind != netprofile.RefusalPackage {
			t.Errorf("%d %q read as %+v, %v; want a package refusal", r.status, line, got, ok)
		}
	})

	t.Run("registry grant allowlist", func(t *testing.T) {
		h, _ := npmRig(t, okBody("x"), nil)
		r := h.web(t, "/r/art-npm/express", nil)
		line := "403 " + jsonError(t, r.body)
		if got, ok := netprofile.PolicyRefusal(line); r.status != http.StatusForbidden || !ok ||
			got.Kind != netprofile.RefusalPackage {
			t.Errorf("%d %q read as %+v, %v; want a package refusal", r.status, line, got, ok)
		}
	})

	for _, phrase := range []string{suspendedPhrase, shuttingDownPhrase} {
		if got, ok := netprofile.PolicyRefusal("403 " + phrase); ok {
			t.Errorf("transient %q read as a policy refusal %+v", phrase, got)
		}
	}

	// Each case drives the proxy to one refusal and reads it as an agent
	// would quote it: the status, then the body.
	packageRefusals := []struct {
		name   string
		status int
		answer func(t *testing.T) result
	}{
		{"registry not granted", http.StatusForbidden, func(t *testing.T) result {
			h, _ := npmRig(t, okBody("x"), nil)
			return h.web(t, "/r/nope/left-pad", nil)
		}},
		{"registry policy", http.StatusForbidden, func(t *testing.T) result {
			h, _ := npmRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
				func(g *netprofile.Grant) { g.Packages, g.PackagePolicy = nil, netprofile.PackagePolicyRegistry })
			return h.web(t, "/r/art-npm/cur", nil)
		}},
		{"private module, checksum lookup", http.StatusGone, func(t *testing.T) result {
			h, _ := goRig(t, modHandler(nil), goRegistry(false), nil, nil)
			return h.gr(t, lookupPath+"example.com/acme/x@v1.0.0")
		}},
		{"private module, public go route", http.StatusGone, func(t *testing.T) result {
			h, _ := goRig(t, modHandler(nil), goRegistry(false), nil, nil)
			p := goProfile(goRegistry(false), nil)
			p.Modules = netprofile.Prefixes{"example.com/"}
			return newRegHarnessFrom(t, h, p, failingUpstream(t), failingUpstream(t)).
				gr(t, "/go/example.com/acme/x/@v/list")
		}},
		{"checksum not public", http.StatusGone, func(t *testing.T) result {
			h, _ := goRig(t, modHandler(nil), goRegistry(false), nil, nil)
			return h.gr(t, lookupPath+"example.com/lib/y@v1.0.0")
		}},
	}
	for _, tt := range packageRefusals {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.answer(t)
			line := strconv.Itoa(r.status) + " " + strings.TrimSpace(r.body)
			if got, ok := netprofile.PolicyRefusal(line); r.status != tt.status || !ok ||
				got.Kind != netprofile.RefusalPackage {
				t.Errorf("%d %q read as %+v, %v; want %d and a package refusal", r.status, line, got, ok, tt.status)
			}
		})
	}

	t.Run("plain http (absolute-form)", func(t *testing.T) {
		h := newHarness(t, Config{}, failingUpstream(t), failingUpstream(t))
		status, body := rawAnswer(t, h.urls["go"], "GET http://proxy.golang.org/go/github.com/google/uuid/@v/list "+
			"HTTP/1.1")
		line := "400 " + strings.TrimSpace(body)
		if got, ok := netprofile.PolicyRefusal(line); status != http.StatusBadRequest || !ok ||
			got.Kind != netprofile.RefusalNever {
			t.Errorf("%d %q read as %+v, %v; want a never refusal", status, line, got, ok)
		}
	})
}

// rawAnswer writes requestLine to the proxy at base as is (an absolute-form
// URI no client library sends to a server) and returns its status and body.
func rawAnswer(t *testing.T, base, requestLine string) (int, string) {
	t.Helper()
	u := mustURL(t, base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, requestLine+"\r\nHost: "+u.Host+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}
