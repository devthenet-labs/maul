// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// echoHandler answers with its inbound Authorization inside a body of the
// given size, at offset at.
func echoHandler(size, at int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		body := bytes.Repeat([]byte("."), size)
		copy(body[min(at, size-len(auth)):], auth)
		if f, ok := w.(http.Flusher); ok && size > echoPrefixBytes {
			_, _ = w.Write(body[:echoPrefixBytes])
			f.Flush()
			_, _ = w.Write(body[echoPrefixBytes:])
			return
		}
		_, _ = w.Write(body)
	}
}

// TestCredentialEcho: a registry that echoes the credential into a
// packument or a go .info answer gets a 502 credential_echo when the echo
// is within the first bytes, and an aborted stream after them; no form of
// the secret ever reaches the client, whole or begun.
func TestCredentialEcho(t *testing.T) {
	for _, tt := range []struct {
		name     string
		size, at int
		abort    bool
	}{
		{"buffered", 1024, 100, false},
		{"streamed", 3 * echoPrefixBytes, 2 * echoPrefixBytes, true},
		{"straddling", 3 * echoPrefixBytes, 2*echoPrefixBytes - 5, true},
		{"straddling the prefix", 3 * echoPrefixBytes, echoPrefixBytes - 5, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := npmRig(t, echoHandler(tt.size, tt.at), nil)
			res := h.web(t, "/r/art-npm/left-pad", nil)
			if !tt.abort && (res.status != http.StatusBadGateway || h.lastLine(t)["outcome"] != OutcomeCredentialEcho) {
				t.Errorf("echo = %d %v, want 502 credential_echo", res.status, h.lastLine(t))
			}
			if tt.abort {
				if res.err == nil && res.status == http.StatusOK && len(res.body) == tt.size {
					t.Errorf("a streamed echo was not aborted")
				}
				waitForLine(t, h.log, func(l map[string]any) bool { return l["outcome"] == OutcomeCredentialEcho })
			}
			for _, leak := range []string{sentinel, sentinel[:6], "Bearer s3"} {
				if strings.Contains(res.body, leak) {
					t.Errorf("the client saw %q", leak)
				}
			}
		})
	}
	// An archive answered 200 is not scanned: it is opaque and verified
	// by hash.
	h, _ := npmRig(t, echoHandler(1024, 10), nil)
	if res := h.web(t, "/r/art-npm/left-pad/-/left-pad-1.0.0.tgz", nil); res.status != http.StatusOK {
		t.Errorf("tarball = %d, want 200 (archives are not scanned)", res.status)
	}
}

// TestBasicCredentialEcho: for basic auth the encoded pair and the
// password alone are both needles.
func TestBasicCredentialEcho(t *testing.T) {
	for name, echo := range map[string]func(r *http.Request) string{
		"pair":     func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ") },
		"password": func(*http.Request) string { return sentinel },
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := goRig(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"Version":"v1.0.0","x":"`+echo(r)+`"}`)
			}, goRegistry(false), nil, nil)
			res := h.do(t, "gr", http.MethodGet, "/r/art-go/example.com/lib/y/@v/v1.0.0.info", nil)
			if res.status != http.StatusBadGateway || h.lastLine(t)["outcome"] != OutcomeCredentialEcho {
				t.Errorf("echo = %d %q", res.status, res.body)
			}
		})
	}
}

// TestNeverEchoed: across a run of every answer kind with the sentinel
// credential (a 401 carrying it, 404s of a tarball and a go zip carrying it
// and an internal URL, a refusal, a success), the sentinel and its encoded
// forms appear in no response body or header and no audit line.
func TestNeverEchoed(t *testing.T) {
	leaky := func(w http.ResponseWriter, r *http.Request) {
		body := "echo " + r.Header.Get("Authorization") + " " + sentinel + " https://internal.example/x"
		switch {
		case strings.Contains(r.URL.Path, "unauth"):
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, ".tgz"), strings.HasSuffix(r.URL.Path, ".zip"):
			w.WriteHeader(http.StatusNotFound)
		default:
			body = `{"name":"ok"}`
		}
		_, _ = io.WriteString(w, body)
	}
	npm, _ := npmRig(t, leaky, func(g *netprofile.Grant) { g.Packages = append(g.Packages, "unauth") })
	gor, _ := goRig(t, leaky, goRegistry(false), nil, nil)
	seen := make([]string, 0, 12)
	for _, p := range []string{"/r/art-npm/unauth", "/r/art-npm/left-pad/-/left-pad-1.0.0.tgz",
		"/r/art-npm/left-pad", "/r/art-npm/express"} {
		res := npm.web(t, p, nil)
		seen = append(seen, res.body, headerText(res.header))
	}
	res := gor.do(t, "gr", http.MethodGet, "/r/art-go/example.com/lib/y/@v/v1.0.0.zip", nil)
	if res.status != http.StatusNotFound {
		t.Errorf("go zip 404 = %d", res.status)
	}
	seen = append(seen, res.body, headerText(res.header), npm.log.String(), gor.log.String())
	pair := base64.StdEncoding.EncodeToString([]byte("patchy:" + sentinel))
	for _, s := range seen {
		for _, leak := range []string{sentinel, pair, "internal.example", base64.StdEncoding.EncodeToString(
			[]byte(sentinel))} {
			if strings.Contains(s, leak) {
				t.Errorf("%q leaked into %q", leak, s)
			}
		}
	}
}

func headerText(h http.Header) string {
	var b strings.Builder
	_ = h.Write(&b)
	return b.String()
}

// TestRedirects: a 3xx to a listed host is followed afresh without the
// credential and with its query; an unlisted, http, userinfo or
// request-time-denied target is 502; a fourth hop is refused; no Location
// ever reaches the client.
func TestRedirects(t *testing.T) {
	w := newWorld(t)
	reg := w.host(npmHost, func(rw http.ResponseWriter, r *http.Request) {
		target := map[string]string{
			"listed":    "https://cdn.example.org/blob/x.tgz?X-Amz-Signature=abc&X-Amz-Expires=60",
			"pattern":   "https://eu.blobs.example.net/x.tgz",
			"unlisted":  "https://other.example/x.tgz",
			"plain":     "http://cdn.example.org/x.tgz",
			"userinfo":  "https://u:p@cdn.example.org/x.tgz",
			"port":      "https://cdn.example.org:8443/x.tgz",
			"loop":      "https://cdn.example.org/loop/1",
			"denied":    "https://a.evil.blobs.example.net/x.tgz",
			"relative":  "/elsewhere/x.tgz",
			"threehops": "https://cdn.example.org/loop/3",
		}
		for k, v := range target {
			if strings.Contains(r.URL.Path, "/"+k+"/") {
				rw.Header().Set("Location", v)
				rw.WriteHeader(http.StatusFound)
				return
			}
		}
		rw.WriteHeader(http.StatusNotFound)
	})
	cdn := w.host("cdn.example.org", func(rw http.ResponseWriter, r *http.Request) {
		if n, ok := strings.CutPrefix(r.URL.Path, "/loop/"); ok {
			i, _ := strconv.Atoi(n)
			if i < 4 {
				rw.Header().Set("Location", "/loop/"+strconv.Itoa(i+1))
				rw.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
		}
		_, _ = io.WriteString(rw, "tarball")
	})
	blobs := w.host("eu.blobs.example.net", okBody("tarball"))
	evil := w.host("a.evil.blobs.example.net", okBody("tarball"))
	creds := newFakeCreds()
	c, _ := BearerCredential(sentinel)
	creds.set("art-npm", c)
	r := npmRegistry()
	h := newRegHarness(t, w, creds, Config{
		Profiles: mustProfiles(t, netprofile.Profile{Name: "web", Port: 8095,
			Registries: []netprofile.Grant{grantOf(r, nil)}}),
		Registries: mustRegistries(t, r),
		DenyHosts:  []string{"evil.blobs.example.net"},
	}, nil, nil)
	tgz := func(k string) string { return "/r/art-npm/" + k + "/-/" + k + "-1.0.0.tgz" }
	for k, want := range map[string]struct {
		status   int
		outcome  string
		redirect string
	}{
		"listed":    {200, OutcomeProxied, "cdn.example.org"},
		"pattern":   {200, OutcomeProxied, "eu.blobs.example.net"},
		"threehops": {200, OutcomeProxied, "cdn.example.org"},
		"unlisted":  {502, OutcomeRedirectRefused, "other.example"},
		"plain":     {502, OutcomeRedirectRefused, "cdn.example.org"},
		"userinfo":  {502, OutcomeRedirectRefused, "cdn.example.org"},
		"port":      {502, OutcomeRedirectRefused, "cdn.example.org"},
		"loop":      {502, OutcomeRedirectRefused, "cdn.example.org"},
		"denied":    {502, OutcomeDeniedHost, "a.evil.blobs.example.net"},
		"relative":  {502, OutcomeRedirectRefused, npmHost},
	} {
		res := h.web(t, tgz(k), nil)
		l := h.lastLine(t)
		if res.status != want.status || l["outcome"] != want.outcome || l["redirect_host"] != want.redirect {
			t.Errorf("%s: %d %v, want %d %s via %s", k, res.status, l, want.status, want.outcome, want.redirect)
		}
		if res.header.Get("Location") != "" {
			t.Errorf("%s: a Location reached the client", k)
		}
	}
	for _, u := range []*recordingUpstream{cdn, blobs} {
		for _, hit := range u.Hits() {
			if hit.header.Get("Authorization") != "" {
				t.Errorf("redirect hop %s carried the credential", hit.requestURI)
			}
		}
	}
	presigned := false
	for _, hit := range cdn.Hits() {
		presigned = presigned || hit.requestURI == "/blob/x.tgz?X-Amz-Signature=abc&X-Amz-Expires=60"
	}
	if !presigned {
		t.Errorf("listed hops = %v, want the presigned query kept", cdn.Hits())
	}
	if len(evil.Hits()) != 0 {
		t.Errorf("a host denied at request time was dialled")
	}
	if got := len(reg.Hits()); got != 10 {
		t.Errorf("registry saw %d requests, want 10", got)
	}
	for _, hit := range reg.Hits() {
		if hit.header.Get("Authorization") != "Bearer "+sentinel {
			t.Errorf("the registry request %s lacked the credential", hit.requestURI)
		}
	}
}

// TestJudgeRedirect: the request-time deny check holds even for a pattern
// that should never have passed validation, and a hop host must be listed.
func TestJudgeRedirect(t *testing.T) {
	r := netprofile.Registry{RedirectHosts: []string{"*.anthropic.com", "cdn.example.org"}}
	from := mustURL(t, "https://npm.reg.example/x")
	for loc, outcome := range map[string]string{
		"https://api.anthropic.com/v1/messages": OutcomeDeniedHost,
		"https://cdn.example.org/a?sig=1":       "",
		"https://CDN.example.org./a":            "",
		"https://x.cdn.example.org/a":           OutcomeRedirectRefused,
		"https://example.org/a":                 OutcomeRedirectRefused,
		"ftp://cdn.example.org/a":               OutcomeRedirectRefused,
		"":                                      OutcomeRedirectRefused,
		"https://[::1]/a":                       OutcomeRedirectRefused,
	} {
		v := judgeRedirect(r, from, loc, nil)
		if v.outcome != outcome || (outcome == "") != (v.next != nil) {
			t.Errorf("judgeRedirect(%q) = %+v, want outcome %q", loc, v, outcome)
		}
	}
}

// TestRegistryPolicy: an internal registry admits its CIDRs on its port
// only, never the always-denied set (built in, the Kubernetes Service IP,
// --always-deny-cidrs) even when listed; every other address is the
// public policy's; a public registry is exactly the public policy.
func TestRegistryPolicy(t *testing.T) {
	internal := netprofile.Registry{URL: "https://nexus.corp.internal:8443/repository/npm/",
		Internal: &netprofile.Internal{CIDRs: []string{"10.20.30.0/24", "10.100.0.0/24", "169.254.169.254/32"}}}
	always := []netip.Prefix{netip.MustParsePrefix("10.100.0.1/32"), netip.MustParsePrefix("10.20.30.99/32")}
	p := AddrPolicy{}.ForRegistry(internal, always)
	for ap, ok := range map[string]bool{
		"10.20.30.40:8443":     true,
		"10.20.30.40:443":      false,
		"10.20.31.1:8443":      false,
		"10.100.0.20:8443":     true,
		"10.100.0.1:8443":      false,
		"10.20.30.99:8443":     false,
		"169.254.169.254:8443": false,
		"127.0.0.1:8443":       false,
		"8.8.8.8:443":          true,
		"8.8.8.8:8443":         false,
	} {
		if err := p.Check(netip.MustParseAddrPort(ap)); (err == nil) != ok {
			t.Errorf("internal registry Check(%s) = %v, want ok=%v", ap, err, ok)
		}
	}
	public := AddrPolicy{}.ForRegistry(netprofile.Registry{URL: "https://npm.reg.example/"}, always)
	for ap, ok := range map[string]bool{"8.8.8.8:443": true, "10.20.30.40:8443": false, "10.20.30.40:443": false,
		"127.0.0.1:443": false} {
		if err := public.Check(netip.MustParseAddrPort(ap)); (err == nil) != ok {
			t.Errorf("public registry Check(%s) = %v, want ok=%v", ap, err, ok)
		}
	}
	// The listed IMDS address is refused by validation too.
	if _, err := netprofile.NewRegistrySet([]netprofile.Registry{{Name: "x", Ecosystem: "npm",
		URL: internal.URL, Auth: netprofile.Auth{Type: "none"}, Internal: &netprofile.Internal{
			CIDRs: []string{"169.254.169.254/32"}}}}, netprofile.RegistryContext{}); err == nil {
		t.Error("an internal CIDR holding IMDS validated")
	}
}

// TestRegistryPolicyPortOutOfRange: an internal registry whose URL names a
// port past 65535 (one validation refuses, built here unvalidated) admits
// no internal address, neither on the port truncated to 16 bits (70000 is
// 4464) nor on any other; the public policy still judges the rest.
func TestRegistryPolicyPortOutOfRange(t *testing.T) {
	for _, url := range []string{"https://nexus.corp.internal:70000/npm/", "https://nexus.corp.internal:0/npm/"} {
		p := AddrPolicy{}.ForRegistry(netprofile.Registry{URL: url,
			Internal: &netprofile.Internal{CIDRs: []string{"10.20.30.0/24"}}}, nil)
		for ap, ok := range map[string]bool{
			"10.20.30.40:4464":  false,
			"10.20.30.40:0":     false,
			"10.20.30.40:443":   false,
			"10.20.30.40:70000": false,
			"8.8.8.8:443":       true,
		} {
			addrPort, err := netip.ParseAddrPort(ap)
			if err != nil {
				continue // 70000 is no AddrPort at all
			}
			if err := p.Check(addrPort); (err == nil) != ok {
				t.Errorf("%s: Check(%s) = %v, want ok=%v", url, ap, err, ok)
			}
		}
	}
}

// TestRegistryDialRebinding: a public registry's name resolving to a
// private address is refused at dial time, as step 1's upstreams are; the
// public (redirect) client never uses an internal admission.
func TestRegistryDialRebinding(t *testing.T) {
	fl := &fixedLookup{answers: [][]netip.Addr{addrs("10.20.30.40")}}
	r := npmRegistry()
	d := newGuardedDialer(AddrPolicy{}.ForRegistry(r, nil), fl.lookup)
	if _, err := d.DialContext(t.Context(), "tcp", npmHost+":443"); !errors.Is(err, ErrDeniedAddress) {
		t.Errorf("public registry resolving to 10.20.30.40 = %v, want ErrDeniedAddress", err)
	}
	creds := newFakeCreds()
	reg := netprofile.Registry{Name: "nexus", Ecosystem: "npm", URL: "https://nexus.corp.internal:8443/npm/",
		Auth: netprofile.Auth{Type: "none"}, Internal: &netprofile.Internal{CIDRs: []string{"10.20.30.40/32"}}}
	srv, err := New(Config{
		Profiles: mustProfiles(t, netprofile.Profile{Name: "web", Port: 8095,
			Registries: []netprofile.Grant{grantOf(reg, nil)}}),
		Registries:  mustRegistries(t, reg),
		Credentials: creds,
		lookup:      fl.lookup,
	}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	pub := srv.public.Transport.(*http.Transport)
	if _, err := pub.DialContext(t.Context(), "tcp", "cdn.example.org:8443"); !errors.Is(err, ErrDeniedAddress) {
		t.Errorf("redirect client dialled an internal address: %v", err)
	}
}

// TestNewRefusesRegistryConfig: a grant naming an unknown registry, a
// login without a source, a CA without its bundle and an internal CIDR
// over an always-denied address are refused at startup.
func TestNewRefusesRegistryConfig(t *testing.T) {
	r := npmRegistry()
	prof := mustProfiles(t, netprofile.Profile{Name: "web", Port: 8095, Registries: []netprofile.Grant{grantOf(r, nil)}})
	internal := netprofile.Registry{Name: "art-npm", Ecosystem: "npm", URL: "https://nexus.corp.internal:8443/npm/",
		Auth: netprofile.Auth{Type: "none"}, Internal: &netprofile.Internal{CIDRs: []string{"10.100.0.0/24"}}}
	withCA := r
	withCA.CA = true
	for name, cfg := range map[string]Config{
		"unknown registry": {Profiles: prof},
		"no source":        {Profiles: prof, Registries: mustRegistries(t, r)},
		"no CA bundle":     {Profiles: prof, Registries: mustRegistries(t, withCA), Credentials: newFakeCreds()},
		"always denied": {Profiles: mustProfiles(t, netprofile.Profile{Name: "web", Port: 8095,
			Registries: []netprofile.Grant{grantOf(internal, nil)}}), Registries: mustRegistries(t, internal),
			AlwaysDeny: []netip.Prefix{netip.MustParsePrefix("10.100.0.1/32")}},
	} {
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

// TestJoinBase: the remainder leaves exactly as admitted.
func TestJoinBase(t *testing.T) {
	for _, tt := range []struct{ base, esc, want string }{
		{"https://h/a/b/", "/@acme%2fbar", "/a/b/@acme%2fbar"},
		{"https://h/a/b", "/@acme%2Fbar", "/a/b/@acme%2Fbar"},
		{"https://h/", "/x/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl",
			"/x/torch-2.3.0%2Bcu121-cp312-cp312-linux_x86_64.whl"},
		{"https://h/p+q/", "/left-pad", "/p+q/left-pad"},
	} {
		u, err := joinBase(mustURL(t, tt.base), tt.esc)
		if err != nil || u.RequestURI() != tt.want {
			t.Errorf("joinBase(%s, %s) = %v %v, want %s", tt.base, tt.esc, u, err, tt.want)
		}
	}
}

// registryAuditScript is a fixed run over npm and go registry routes.
var registryAuditScript = []struct{ profile, path string }{
	{"web", "/r/art-npm/left-pad"},
	{"web", "/r/art-npm/@acme%2fbar"},
	{"web", "/r/art-npm/left-pad/-/left-pad-1.3.0.tgz"},
	{"web", "/r/art-npm/express"},
	{"web", "/r/art-npm/left-pad/-/left-pad-1.3.0-rc.1.tgz"},
	{"web", "/r/elsewhere/x"},
	{"gr", "/r/art-go/example.com/lib/y/@v/list"},
	{"gr", "/r/art-go/example.com/lib/y/@v/v1.0.0.zip"},
	{"gr", "/r/art-go/sumdb/sum.golang.org/lookup/example.com/acme/x@v1.0.0"},
	{"gr", "/r/art-go/sumdb/sum.golang.org/supported"},
	{"gr", "/go/example.com/acme/x/@v/list"},
}

// TestRegistryAuditGolden: /r/ lines carry step 1's attributes, then the
// registry's after reason; go registry lines use module and version and
// carry no package.
func TestRegistryAuditGolden(t *testing.T) {
	w := newWorld(t)
	w.host(npmHost, okBody(`{"name":"x"}`))
	w.host(goHost, okBody("v1.0.0\n"))
	creds := newFakeCreds()
	bearer, _ := BearerCredential(sentinel)
	basic, _ := BasicCredential("patchy", sentinel)
	creds.set("art-npm", bearer)
	creds.set("art-go", basic)
	goReg := goRegistry(false)
	h := newRegHarness(t, w, creds, Config{
		Profiles:   mustProfiles(t, npmProfile(nil), goProfile(goReg, nil)),
		Registries: mustRegistries(t, npmRegistry(), goReg),
	}, nil, nil)
	var buf lockedBuffer
	h.srv.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string,
		a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == "duration" {
			return slog.Attr{}
		}
		return a
	}}))
	for _, step := range registryAuditScript {
		if res := h.do(t, step.profile, http.MethodGet, step.path, nil); res.err != nil {
			t.Fatal(res.err)
		}
	}
	got := []byte(buf.String())
	path := filepath.Join("testdata", "audit_registry.golden.jsonl")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("registry audit lines changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRegistryMetrics: /r/ requests carry ecosystem and registry; /go
// requests keep step 1's attribute set.
func TestRegistryMetrics(t *testing.T) {
	testMetrics()
	h, _ := npmRig(t, okBody("{}"), nil)
	attrs := map[string]string{"profile": "web", "route": "r/art-npm", "outcome": "proxied", "ecosystem": "npm",
		"registry": "art-npm"}
	before := sum(t, "patchy.egress_proxy.requests", attrs)
	h.web(t, "/r/art-npm/left-pad", nil)
	h.web(t, "/r/art-npm/left-pad", nil)
	if got := sum(t, "patchy.egress_proxy.requests", attrs) - before; got != 2 {
		t.Errorf("requests grew by %d, want 2", got)
	}
	CountCredentialRefresh(t.Context(), "art-npm", CredentialStatic, "reloaded")
	if got := sum(t, "patchy.egress_proxy.credential.refreshes", map[string]string{"registry": "art-npm",
		"kind": "static", "result": "reloaded"}); got < 1 {
		t.Errorf("refreshes = %d", got)
	}
	stop, err := RegisterCredentialExpiry([]string{"ca-npm"}, func(string) (time.Time, bool) {
		return time.Unix(1000, 0), true
	}, func() time.Time { return time.Unix(400, 0) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop() }()
	if got := gauge(t, "patchy.egress_proxy.credential.expiry", "ca-npm"); got != 600 {
		t.Errorf("expiry gauge = %d, want 600", got)
	}
}

// TestOpenRegistry401: a registry without a login and no credential
// source answering 401 is a 502 upstream_auth, never a crash.
func TestOpenRegistry401(t *testing.T) {
	w := newWorld(t)
	w.host(npmHost, func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusUnauthorized) })
	r := npmRegistry()
	r.Auth = netprofile.Auth{Type: netprofile.AuthNone}
	r.HealthPackage = ""
	h := newHarness(t, Config{
		Profiles:    mustProfiles(t, webProfile(r)),
		Registries:  mustRegistries(t, r),
		rootCAs:     w.pki.pool,
		dialRewrite: w.rewrite,
	}, failingUpstream(t), failingUpstream(t))
	if res := h.do(t, "web", http.MethodGet, "/r/art-npm/left-pad", nil); res.status != http.StatusBadGateway ||
		h.lastLine(t)["outcome"] != OutcomeUpstreamAuth || h.lastLine(t)["credential"] != "none" {
		t.Errorf("open registry 401 = %d %v", res.status, h.lastLine(t))
	}
}
