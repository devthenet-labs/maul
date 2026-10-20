// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

const npmHost = "npm.reg.example"

// npmBase is the Artifactory-shaped npm registry's base path.
const npmBase = "/artifactory/api/npm/npm-virtual"

func npmRegistry() netprofile.Registry {
	return netprofile.Registry{Name: "art-npm", Ecosystem: netprofile.EcosystemNPM,
		URL: "https://" + npmHost + npmBase + "/", Auth: netprofile.Auth{Type: netprofile.AuthBearer},
		RedirectHosts: []string{"cdn.example.org", "*.blobs.example.net"}, HealthPackage: "left-pad"}
}

// npmProfile grants art-npm with an allowlist, edited by mod.
func npmProfile(mod func(*netprofile.Grant)) netprofile.Profile {
	return netprofile.Profile{Name: "web", Port: 8095, Registries: []netprofile.Grant{
		grantOf(npmRegistry(), func(g *netprofile.Grant) {
			g.PackagePolicy, g.Packages = "", []string{"left-pad", "@acme/*", "JSONStream"}
			if mod != nil {
				mod(g)
			}
		})}}
}

// npmRig is an engine with art-npm granted to profile web, the registry
// answering h, and the sentinel bearer token loaded.
func npmRig(t *testing.T, h http.HandlerFunc, mod func(*netprofile.Grant)) (*regHarness, *recordingUpstream) {
	t.Helper()
	w := newWorld(t)
	up := w.host(npmHost, h)
	creds := newFakeCreds()
	c, err := BearerCredential(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	creds.set("art-npm", c)
	cfg := Config{Profiles: mustProfiles(t, npmProfile(mod)), Registries: mustRegistries(t, npmRegistry())}
	return newRegHarness(t, w, creds, cfg, nil, nil), up
}

func (h *regHarness) web(t *testing.T, path string, header http.Header) result {
	t.Helper()
	return h.do(t, "web", http.MethodGet, path, header)
}

// TestNPMPackument: unscoped and scoped packuments reach the registry below
// its base path with the scope separator escaped exactly as sent, carrying
// the bearer credential and the Accept of the table, and nothing inbound.
func TestNPMPackument(t *testing.T) {
	h, up := npmRig(t, okBody(`{"name":"x"}`), nil)
	inbound := http.Header{"Authorization": {"Bearer agent-token"}, "Cookie": {"c=1"},
		"Accept": {npmCorgi}, "Npm-Session": {"abc"}}
	for _, tt := range []struct{ path, wantURI, wantAccept string }{
		{"/r/art-npm/left-pad", npmBase + "/left-pad", npmCorgi},
		{"/r/art-npm/JSONStream", npmBase + "/JSONStream", npmCorgi},
		{"/r/art-npm/@acme%2fbar", npmBase + "/@acme%2fbar", npmCorgi},
		{"/r/art-npm/@acme%2Fbar", npmBase + "/@acme%2Fbar", npmCorgi},
	} {
		before := len(up.Hits())
		res := h.web(t, tt.path, inbound)
		if res.status != http.StatusOK || res.body != `{"name":"x"}` {
			t.Errorf("GET %s = %d %q, want 200", tt.path, res.status, res.body)
			continue
		}
		hits := up.Hits()
		if len(hits) != before+1 {
			t.Fatalf("GET %s: %d upstream hits, want 1", tt.path, len(hits)-before)
		}
		hit := hits[len(hits)-1]
		if hit.requestURI != tt.wantURI {
			t.Errorf("GET %s reached %s, want %s", tt.path, hit.requestURI, tt.wantURI)
		}
		if got := hit.header.Get("Authorization"); got != "Bearer "+sentinel {
			t.Errorf("GET %s Authorization = %q, want the registry's bearer token", tt.path, got)
		}
		if got := hit.header.Get("Accept"); got != tt.wantAccept {
			t.Errorf("GET %s Accept = %q, want %q", tt.path, got, tt.wantAccept)
		}
		for _, k := range []string{"Cookie", "Npm-Session"} {
			if hit.header.Get(k) != "" {
				t.Errorf("GET %s forwarded the inbound %s", tt.path, k)
			}
		}
	}
	// The Accept table: anything but npm's corgi value or
	// application/json becomes application/json.
	for in, want := range map[string]string{"application/json": "application/json", "text/html": "application/json",
		"": "application/json"} {
		hdr := http.Header{}
		if in != "" {
			hdr.Set("Accept", in)
		}
		h.web(t, "/r/art-npm/left-pad", hdr)
		hits := up.Hits()
		if got := hits[len(hits)-1].header.Get("Accept"); got != want {
			t.Errorf("Accept %q left as %q, want %q", in, got, want)
		}
	}
}

// TestNPMTarballShapes: the three tarball shapes, bare and below the
// registry's base path (npm's replace-registry-host=always shape), reach
// the registry at the same path; a scope-repeated file part with another
// scope, or a file of another package, is refused.
func TestNPMTarballShapes(t *testing.T) {
	h, up := npmRig(t, okBody("tgz"), nil)
	for _, tt := range []struct{ path, want string }{
		{"/left-pad/-/left-pad-1.3.0.tgz", npmBase + "/left-pad/-/left-pad-1.3.0.tgz"},
		{"/@acme/bar/-/bar-2.0.0.tgz", npmBase + "/@acme/bar/-/bar-2.0.0.tgz"},
		{"/@acme/bar/-/@acme/bar-2.0.0.tgz", npmBase + "/@acme/bar/-/@acme/bar-2.0.0.tgz"},
	} {
		for _, prefix := range []string{"/r/art-npm", "/r/art-npm" + npmBase} {
			before := len(up.Hits())
			res := h.web(t, prefix+tt.path, nil)
			hits := up.Hits()
			if res.status != http.StatusOK || len(hits) != before+1 || hits[len(hits)-1].requestURI != tt.want {
				t.Errorf("GET %s%s = %d, upstream %v, want 200 at %s", prefix, tt.path, res.status,
					hits[before:], tt.want)
			}
			if l := h.lastLine(t); l["kind"] != kindTarball || l["version"] == "" || l["package"] == "" {
				t.Errorf("tarball audit line = %v", l)
			}
		}
	}
	before := len(up.Hits())
	for _, p := range []string{
		"/r/art-npm/@acme/bar/-/@other/bar-2.0.0.tgz",
		"/r/art-npm/left-pad/-/right-pad-1.0.0.tgz",
		"/r/art-npm/left-pad/-/left-pad-01.0.0.tgz",
	} {
		res := h.web(t, p, nil)
		if res.status != http.StatusForbidden || !strings.Contains(jsonError(t, res.body), "egress-proxy") {
			t.Errorf("GET %s = %d %q, want a 403 JSON refusal", p, res.status, res.body)
		}
	}
	if n := len(up.Hits()) - before; n != 0 {
		t.Errorf("refused tarballs reached the registry %d times", n)
	}
}

// TestNPMPrerelease: a prerelease tarball is refused 403 prerelease unless
// the grant admits prereleases.
func TestNPMPrerelease(t *testing.T) {
	const p = "/r/art-npm/left-pad/-/left-pad-1.3.0-rc.1.tgz"
	h, up := npmRig(t, okBody("tgz"), nil)
	res := h.web(t, p, nil)
	if res.status != http.StatusForbidden || h.lastLine(t)["outcome"] != OutcomePrerelease || len(up.Hits()) != 0 {
		t.Errorf("prerelease without allowPrerelease = %d %q, %d hits", res.status, res.body, len(up.Hits()))
	}
	h, up = npmRig(t, okBody("tgz"), func(g *netprofile.Grant) { g.AllowPrerelease = true })
	if res := h.web(t, p, nil); res.status != http.StatusOK || len(up.Hits()) != 1 {
		t.Errorf("prerelease with allowPrerelease = %d %q", res.status, res.body)
	}
}

// TestNPMRefusals: paths outside the packument and tarball grammar, and a
// package outside the allowlist, are refused with npm's JSON body and never
// reach the registry.
func TestNPMRefusals(t *testing.T) {
	h, up := npmRig(t, okBody("x"), nil)
	for path, outcome := range map[string]string{
		"/r/art-npm/express":                                 OutcomeNotAllowed,
		"/r/art-npm/@other%2fbar":                            OutcomeNotAllowed,
		"/r/art-npm/express/-/express-4.0.0.tgz":             OutcomeNotAllowed,
		"/r/art-npm/a%2fb":                                   OutcomeBadPath,
		"/r/art-npm/%2e%2e/left-pad":                         OutcomeBadPath,
		"/r/art-npm/left-pad%2e":                             OutcomeBadPath,
		"/r/art-npm/-/v1/search":                             OutcomeBadPath,
		"/r/art-npm/-/npm/v1/security/advisories/bulk":       OutcomeBadPath,
		"/r/art-npm/-/package/left-pad/dist-tags":            OutcomeBadPath,
		"/r/art-npm/-/whoami":                                OutcomeBadPath,
		"/r/art-npm/-/ping":                                  OutcomeBadPath,
		"/r/art-npm/left-pad/1.3.0":                          OutcomeBadPath,
		"/r/art-npm/left-pad/dist-tags":                      OutcomeBadPath,
		"/r/art-npm/":                                        OutcomeBadPath,
		"/r/art-npm":                                         OutcomeBadPath,
		"/r/art-npm/left-pad//-/left-pad-1.0.0.tgz":          OutcomeBadPath,
		"/r/art-npm/@acme%2fbar/-/bar-1.0.0.tgz":             OutcomeBadPath,
		"/r/art-npm" + npmBase + "/left-pad":                 OutcomeBadPath,
		"/r/art-npm/left-pad/-/left-pad-1.0.0.tgz%00":        OutcomeBadPath,
		"/r/art-npm/node_modules":                            OutcomeBadPath,
		"/r/art-npm/" + strings.Repeat("a", MaxPathLen) + "": OutcomeBadPath,
	} {
		res := h.web(t, path, nil)
		if res.status != http.StatusForbidden {
			t.Errorf("GET %s = %d %q, want 403", path, res.status, res.body)
			continue
		}
		if msg := jsonError(t, res.body); !strings.HasPrefix(msg, "egress-proxy: ") {
			t.Errorf("GET %s error = %q", path, msg)
		}
		if got := h.lastLine(t)["outcome"]; got != outcome {
			t.Errorf("GET %s outcome = %v, want %s", path, got, outcome)
		}
	}
	// A dot segment, sent raw: no client normalises it away.
	if s := raw(t, h.urls["web"], "GET /r/art-npm/left-pad/../x HTTP/1.1"); s != http.StatusForbidden {
		t.Errorf("dot segment = %d, want 403", s)
	}
	// The audit endpoint is a POST: the method rule refuses it first.
	if res := h.do(t, "web", http.MethodPost, "/r/art-npm/-/npm/v1/security/advisories/bulk", nil); res.status !=
		http.StatusMethodNotAllowed {
		t.Errorf("POST advisories = %d, want 405", res.status)
	}
	if n := len(up.Hits()); n != 0 {
		t.Errorf("refused requests reached the registry %d times: %v", n, up.Hits())
	}
}

// TestNotGranted: a registry the profile does not hold (another profile's,
// or none at all) is refused not_granted, 410 for a go-shaped path and 403
// otherwise, and its name is never echoed.
func TestNotGranted(t *testing.T) {
	w := newWorld(t)
	up := w.host(npmHost, okBody("x"))
	other := npmRegistry()
	other.Name = "other-npm"
	creds := newFakeCreds()
	creds.set("art-npm", NoCredential())
	creds.set("other-npm", NoCredential())
	cfg := Config{
		Profiles: mustProfiles(t, npmProfile(nil),
			netprofile.Profile{Name: "app", Port: 8097, Registries: []netprofile.Grant{grantOf(other, nil)}}),
		Registries: mustRegistries(t, npmRegistry(), other),
	}
	h := newRegHarness(t, w, creds, cfg, nil, nil)
	for path, status := range map[string]int{
		"/r/other-npm/left-pad":                    http.StatusForbidden,
		"/r/nope/left-pad":                         http.StatusForbidden,
		"/r/Bad_Name/x":                            http.StatusForbidden,
		"/r/other-npm/example.com/m/@v/list":       http.StatusGone,
		"/r/other-npm/example.com/m/@latest":       http.StatusGone,
		"/r/other-npm/sumdb/sum.golang.org/latest": http.StatusGone,
	} {
		res := h.web(t, path, nil)
		l := h.lastLine(t)
		if res.status != status || l["outcome"] != OutcomeNotGranted || l["registry"] != "" || l["route"] != "r" {
			t.Errorf("GET %s = %d, line %v; want %d not_granted with no registry named", path, res.status, l, status)
		}
		if strings.Contains(res.body, "other-npm") || strings.Contains(res.body, "nope") {
			t.Errorf("GET %s echoed the registry name: %q", path, res.body)
		}
	}
	if len(up.Hits()) != 0 {
		t.Errorf("not-granted requests reached the registry: %v", up.Hits())
	}
}

// TestNPMStatusTable: 404 and 410 are a proxy-written 404; 401 is a 502
// upstream_auth that invalidates the credential; 403 is the registry's
// policy, 403 registry_refused with no invalidation; anything else is 502.
// No upstream error body ever reaches the agent.
func TestNPMStatusTable(t *testing.T) {
	status := map[string]int{"/nf": 404, "/gone": 410, "/auth": 401, "/cur": 403, "/boom": 500, "/teapot": 418}
	h, _ := npmRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status[strings.TrimPrefix(r.URL.Path, npmBase)])
		_, _ = io.WriteString(w, "upstream body "+sentinel+" https://internal.example/secret")
	}, func(g *netprofile.Grant) { g.Packages, g.PackagePolicy = nil, netprofile.PackagePolicyRegistry })
	for path, want := range map[string]struct {
		status  int
		outcome string
	}{
		"nf":     {404, OutcomeProxied},
		"gone":   {404, OutcomeProxied},
		"auth":   {502, OutcomeUpstreamAuth},
		"cur":    {403, OutcomeRegistryRefused},
		"boom":   {502, OutcomeUpstreamError},
		"teapot": {502, OutcomeUpstreamError},
	} {
		res := h.web(t, "/r/art-npm/"+path, nil)
		if res.status != want.status || h.lastLine(t)["outcome"] != want.outcome {
			t.Errorf("%s: %d %v, want %d %s", path, res.status, h.lastLine(t)["outcome"], want.status, want.outcome)
		}
		jsonError(t, res.body)
		if strings.Contains(res.body, "upstream body") || strings.Contains(res.body, sentinel) ||
			strings.Contains(res.body, "internal.example") {
			t.Errorf("%s: the upstream body reached the agent: %q", path, res.body)
		}
	}
	if n := h.creds.invalidations("art-npm"); n != 1 {
		t.Errorf("Invalidate called %d times, want once (for the 401 only)", n)
	}
	if !strings.Contains(jsonError(t, h.web(t, "/r/art-npm/cur", nil).body), "refused by the policy of registry") {
		t.Error("a registry 403 does not read as the registry's policy")
	}
}

// TestCredentialSelfCheck: three 403s within a minute fetch the
// healthPackage once with the credential; a 200 leaves the credential
// alone, a 401 invalidates it and logs credential_rejected. Without a
// healthPackage there is no check.
func TestCredentialSelfCheck(t *testing.T) {
	run := func(t *testing.T, health int, healthPkg string) (*regHarness, *recordingUpstream) {
		reg := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == npmBase+"/left-pad" {
				w.WriteHeader(health)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		}
		w := newWorld(t)
		up := w.host(npmHost, reg)
		creds := newFakeCreds()
		c, _ := BearerCredential(sentinel)
		creds.set("art-npm", c)
		r := npmRegistry()
		r.HealthPackage = healthPkg
		g := grantOf(r, nil)
		h := newRegHarness(t, w, creds, Config{
			Profiles:   mustProfiles(t, netprofile.Profile{Name: "web", Port: 8095, Registries: []netprofile.Grant{g}}),
			Registries: mustRegistries(t, r),
		}, nil, nil)
		for range 3 {
			if res := h.web(t, "/r/art-npm/curated", nil); res.status != http.StatusForbidden {
				t.Fatalf("curated = %d", res.status)
			}
		}
		h.srv.selfChecks.Wait()
		return h, up
	}
	t.Run("healthy", func(t *testing.T) {
		h, up := run(t, http.StatusOK, "left-pad")
		if n := h.creds.invalidations("art-npm"); n != 0 {
			t.Errorf("Invalidate called %d times after a healthy self-check", n)
		}
		hits := up.Hits()
		last := hits[len(hits)-1]
		if last.requestURI != npmBase+"/left-pad" || last.header.Get("Authorization") != "Bearer "+sentinel {
			t.Errorf("self-check request = %+v", last)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		h, _ := run(t, http.StatusUnauthorized, "left-pad")
		if n := h.creds.invalidations("art-npm"); n != 1 {
			t.Errorf("Invalidate called %d times, want 1", n)
		}
		if !strings.Contains(h.log.String(), `"msg":"credential_rejected"`) ||
			strings.Contains(h.log.String(), sentinel) {
			t.Errorf("credential_rejected line missing or leaking:\n%s", h.log.String())
		}
		// Within the interval, more 403s start no second check.
		for range 3 {
			h.web(t, "/r/art-npm/curated", nil)
		}
		h.srv.selfChecks.Wait()
		if n := h.creds.invalidations("art-npm"); n != 1 {
			t.Errorf("Invalidate called %d times within one interval, want 1", n)
		}
	})
	t.Run("no health package", func(t *testing.T) {
		h, up := run(t, http.StatusUnauthorized, "")
		if n := h.creds.invalidations("art-npm"); n != 0 || len(up.Hits()) != 3 {
			t.Errorf("without healthPackage: %d invalidations, %d hits", n, len(up.Hits()))
		}
	})
}

// TestCredentialUnavailable: no credential, or an expired one, is 503
// credential_unavailable before any upstream contact.
func TestCredentialUnavailable(t *testing.T) {
	h, up := npmRig(t, okBody("x"), nil)
	h.creds.mu.Lock()
	delete(h.creds.creds, "art-npm")
	h.creds.mu.Unlock()
	if res := h.web(t, "/r/art-npm/left-pad", nil); res.status != http.StatusServiceUnavailable ||
		h.lastLine(t)["outcome"] != OutcomeCredentialUnavailable {
		t.Errorf("no credential = %d %v", res.status, h.lastLine(t))
	}
	c, _ := CodeArtifactCredential(netprofile.EcosystemNPM, sentinel, time.Now().Add(-time.Minute))
	h.creds.set("art-npm", c)
	if res := h.web(t, "/r/art-npm/left-pad", nil); res.status != http.StatusServiceUnavailable {
		t.Errorf("expired credential = %d", res.status)
	}
	if len(up.Hits()) != 0 {
		t.Errorf("the registry was contacted without a credential")
	}
}

// TestNPMSizeCaps: a tarball over the registry's maxArtifactBytes is a 502,
// by Content-Length or mid-stream.
func TestNPMSizeCaps(t *testing.T) {
	w := newWorld(t)
	w.host(npmHost, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "chunked-1.0.0.tgz") {
			rw.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(rw, strings.Repeat("x", 2048))
	})
	creds := newFakeCreds()
	creds.set("art-npm", NoCredential())
	r := npmRegistry()
	r.MaxArtifactBytes = 1024
	h := newRegHarness(t, w, creds, Config{
		Profiles:   mustProfiles(t, webProfile(r)),
		Registries: mustRegistries(t, r),
	}, nil, nil)
	if res := h.web(t, "/r/art-npm/big/-/big-1.0.0.tgz", nil); res.status != http.StatusBadGateway ||
		h.lastLine(t)["outcome"] != OutcomeTooLarge {
		t.Errorf("oversized tarball = %d %v", res.status, h.lastLine(t))
	}
	res := h.web(t, "/r/art-npm/chunked/-/chunked-1.0.0.tgz", nil)
	if res.err == nil && len(res.body) > 1024 {
		t.Errorf("a chunked tarball over the cap passed: %d bytes", len(res.body))
	}
	waitForLine(t, h.log, func(l map[string]any) bool {
		return l["outcome"] == OutcomeTooLarge && l["reason"] == "stream"
	})
	// A packument has its own 64 MiB cap: 2 KiB passes.
	if res := h.web(t, "/r/art-npm/big", nil); res.status != http.StatusOK || len(res.body) != 2048 {
		t.Errorf("packument = %d, %d bytes", res.status, len(res.body))
	}
}

// TestCodeArtifactNPM: a CodeArtifact npm registry is sent its minted
// token as a bearer, and the audit names the credential's kind only.
func TestCodeArtifactNPM(t *testing.T) {
	const host = "patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com"
	w := newWorld(t)
	up := w.host(host, okBody("{}"))
	r := netprofile.Registry{Name: "ca-npm", Ecosystem: netprofile.EcosystemNPM,
		URL: "https://" + host + "/npm/npm-store/", Auth: netprofile.Auth{Type: netprofile.AuthCodeArtifact,
			CodeArtifact: &netprofile.CodeArtifact{Domain: "patchy-gate", DomainOwner: "377946145366",
				Region: "us-east-1"}}}
	creds := newFakeCreds()
	c, err := CodeArtifactCredential(netprofile.EcosystemNPM, sentinel, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	creds.set("ca-npm", c)
	h := newRegHarness(t, w, creds, Config{
		Profiles:   mustProfiles(t, webProfile(r)),
		Registries: mustRegistries(t, r),
	}, nil, nil)
	for _, p := range []string{"/r/ca-npm/left-pad", "/r/ca-npm/npm/npm-store/left-pad/-/left-pad-1.3.0.tgz"} {
		if res := h.web(t, p, nil); res.status != http.StatusOK {
			t.Errorf("%s = %d %q", p, res.status, res.body)
		}
		if l := h.lastLine(t); l["credential"] != string(CredentialCodeArtifact) {
			t.Errorf("%s audit credential = %v", p, l["credential"])
		}
	}
	hits := up.Hits()
	if len(hits) != 2 || hits[1].requestURI != "/npm/npm-store/left-pad/-/left-pad-1.3.0.tgz" {
		t.Fatalf("hits = %+v", hits)
	}
	for _, hit := range hits {
		if hit.header.Get("Authorization") != "Bearer "+sentinel {
			t.Errorf("%s Authorization = %q", hit.requestURI, hit.header.Get("Authorization"))
		}
	}
	if strings.Contains(h.log.String(), sentinel) {
		t.Error("the token reached the audit log")
	}
}

// webProfile grants r to profile web under packagePolicy registry.
func webProfile(r netprofile.Registry) netprofile.Profile {
	return netprofile.Profile{Name: "web", Port: 8095, Registries: []netprofile.Grant{grantOf(r, nil)}}
}
