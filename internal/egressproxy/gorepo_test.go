// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

const goHost = "go.reg.example"

// goBase is the Artifactory-shaped Go registry's base path.
const goBase = "/api/go/go-virtual"

// goRegistry is a basic-auth Go registry whose private modules are
// example.com/acme/.
func goRegistry(publicLookups bool) netprofile.Registry {
	return netprofile.Registry{Name: "art-go", Ecosystem: netprofile.EcosystemGo, URL: "https://" + goHost + goBase,
		Auth: netprofile.Auth{Type: netprofile.AuthBasic}, PrivateModules: strsp("example.com/acme/"),
		PublicChecksumLookups: boolp(publicLookups)}
}

// goProfile has the public go route for golang.org/x/ and example.com/pub/
// and a grant of reg with an allowlist, edited by mod.
func goProfile(reg netprofile.Registry, mod func(*netprofile.Grant)) netprofile.Profile {
	return netprofile.Profile{Name: "gr", Port: 8096, Route: netprofile.RouteGo,
		Modules: netprofile.Prefixes{"golang.org/x/", "example.com/pub/"},
		Registries: []netprofile.Grant{grantOf(reg, func(g *netprofile.Grant) {
			g.PackagePolicy, g.Packages = "", []string{"example.com/acme/", "example.com/lib/"}
			if mod != nil {
				mod(g)
			}
		})}}
}

// goRig is an engine with reg granted to profile gr, the registry answering
// h with the basic credential patchy:<sentinel>.
func goRig(t *testing.T, h http.HandlerFunc, reg netprofile.Registry, mod func(*netprofile.Grant),
	sumUp *recordingUpstream,
) (*regHarness, *recordingUpstream) {
	t.Helper()
	w := newWorld(t)
	up := w.host(goHost, h)
	creds := newFakeCreds()
	c, err := BasicCredential("patchy", sentinel)
	if err != nil {
		t.Fatal(err)
	}
	creds.set(reg.Name, c)
	return newRegHarness(t, w, creds, Config{
		Profiles:   mustProfiles(t, goProfile(reg, mod)),
		Registries: mustRegistries(t, reg),
	}, nil, sumUp), up
}

func (h *regHarness) gr(t *testing.T, path string) result {
	t.Helper()
	return h.do(t, "gr", http.MethodGet, path, nil)
}

// TestGoRegistryForwarded: step 1's GOPROXY kinds reach the registry
// below its base path with the basic credential; the grant's allowlist and
// version gate refuse 410 with no contact; HEAD stays HEAD.
func TestGoRegistryForwarded(t *testing.T) {
	h, up := goRig(t, okBody("v1.0.0\n"), goRegistry(false), nil, nil)
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("patchy:"+sentinel))
	for _, p := range []string{
		"example.com/acme/x/@v/list",
		"example.com/acme/x/@v/v1.0.0.info",
		"example.com/acme/x/@v/v1.0.0.mod",
		"example.com/acme/x/@v/v1.0.0.zip",
		"example.com/acme/x/@latest",
		"example.com/lib/%21burnt%21sushi/@v/list",
	} {
		before := len(up.Hits())
		res := h.gr(t, "/r/art-go/"+p)
		hits := up.Hits()
		if res.status != http.StatusOK || len(hits) != before+1 {
			t.Errorf("%s = %d %q, %d hits", p, res.status, res.body, len(hits)-before)
			continue
		}
		if hits[before].requestURI != goBase+"/"+p || hits[before].header.Get("Authorization") != wantAuth {
			t.Errorf("%s reached %s with %q", p, hits[before].requestURI, hits[before].header.Get("Authorization"))
		}
		if l := h.lastLine(t); l["module"] == "" || l["route"] != "r/art-go" || l["ecosystem"] != "go" {
			t.Errorf("%s audit line = %v", p, l)
		} else if _, ok := l["package"]; ok {
			t.Errorf("a go registry line carries package: %v", l)
		}
	}
	if res := h.do(t, "gr", http.MethodHead, "/r/art-go/example.com/acme/x/@v/v1.0.0.info", nil); res.status != 200 {
		t.Errorf("HEAD = %d", res.status)
	}
	if hits := up.Hits(); hits[len(hits)-1].method != http.MethodHead {
		t.Errorf("HEAD left as %s", hits[len(hits)-1].method)
	}
}

// TestGoRegistryRefusals: the grant's allowlist and version gate and step
// 1's grammar refuse 410 with no contact; a registry 403 is 410
// registry_refused (the go column of the status table).
func TestGoRegistryRefusals(t *testing.T) {
	h, up := goRig(t, okBody("v1.0.0\n"), goRegistry(false), nil, nil)
	before := len(up.Hits())
	for p, reason := range map[string]string{
		"/r/art-go/rsc.io/quote/@v/list":                                          ReasonPrefix,
		"/r/art-go/example.com/acme/x/@v/v0.0.0-20230101000000-abcdefabcdef.info": ReasonVersion,
		"/r/art-go/example.com/acme/x/@v/master.info":                             ReasonVersion,
	} {
		res := h.gr(t, p)
		if l := h.lastLine(t); res.status != http.StatusGone || l["outcome"] != OutcomeNotAllowed || l["reason"] != reason {
			t.Errorf("%s = %d %v, want 410 not_allowed %s", p, res.status, l, reason)
		}
	}
	for _, p := range []string{"/r/art-go/example.com/acme/x/@v/bad", "/r/art-go/Example.com/x/@v/list",
		"/r/art-go/example.com/%61cme/x/@v/list"} {
		if res := h.gr(t, p); res.status != http.StatusGone || h.lastLine(t)["outcome"] != OutcomeBadPath {
			t.Errorf("%s = %d %v, want 410 bad_path", p, res.status, h.lastLine(t))
		}
	}
	if res := h.gr(t, "/r/art-go/sumdb/other.example/latest"); res.status != http.StatusNotFound {
		t.Errorf("unknown checksum database = %d, want 404", res.status)
	}
	if n := len(up.Hits()) - before; n != 0 {
		t.Errorf("refused go requests reached the registry %d times", n)
	}
	// The go column of the status table: 403 is 410 registry_refused.
	h403, _ := goRig(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		goRegistry(false), nil, nil)
	if res := h403.gr(t, "/r/art-go/example.com/acme/x/@v/list"); res.status != http.StatusGone ||
		h403.lastLine(t)["outcome"] != OutcomeRegistryRefused {
		t.Errorf("registry 403 = %d %v, want 410 registry_refused", res.status, h403.lastLine(t))
	}
}

// lookupPath is a checksum lookup through the art-go registry route.
const lookupPath = "/r/art-go/sumdb/sum.golang.org/lookup/"

// modHandler answers the registry paths in status and 404s the rest.
func modHandler(status map[string]int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s, ok := status[r.URL.Path]; ok {
			w.WriteHeader(s)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestGoRegistryLookups: the checksum-database order of §2.5 without
// public lookups. supported is local; a private module is never looked up;
// a module of the profile's public allowlist is forwarded as step 1 would;
// everything else is 410 checksum_not_public. The checksum database never
// sees the credential, and the registry is never asked.
func TestGoRegistryLookups(t *testing.T) {
	sumUp := newUpstream(t, okBody("lookup\n"))
	h, up := goRig(t, modHandler(nil), goRegistry(false), nil, sumUp)
	if res := h.gr(t, "/r/art-go/sumdb/sum.golang.org/supported"); res.status != 200 {
		t.Errorf("supported = %d", res.status)
	}
	for p, want := range map[string]struct {
		status  int
		outcome string
	}{
		"example.com/acme/x@v1.0.0":                           {410, OutcomePrivateModule},
		"example.com/lib/y@v1.0.0":                            {410, OutcomeChecksumNotPublic},
		"golang.org/x/mod@v0.20.0":                            {200, OutcomeProxied},
		"example.com/pub/z@v1.2.3":                            {200, OutcomeProxied},
		"rsc.io/quote@v1.5.2":                                 {410, OutcomeChecksumNotPublic},
		"golang.org/x/mod@v0.0.0-20230101000000-abcdefabcdef": {410, OutcomeChecksumNotPublic},
	} {
		res := h.gr(t, lookupPath+p)
		if res.status != want.status || h.lastLine(t)["outcome"] != want.outcome {
			t.Errorf("lookup %s = %d %v, want %d %s", p, res.status, h.lastLine(t), want.status, want.outcome)
		}
	}
	if res := h.gr(t, lookupPath+"example.com/lib/y@v1.0.0"); !strings.Contains(res.body, "checksum_not_public") {
		t.Errorf("checksum_not_public body = %q", res.body)
	}
	for _, p := range []string{"/r/art-go/sumdb/sum.golang.org/latest", "/r/art-go/sumdb/sum.golang.org/tile/8/0/001"} {
		if res := h.gr(t, p); res.status != 200 {
			t.Errorf("%s = %d", p, res.status)
		}
	}
	if len(up.Hits()) != 0 {
		t.Errorf("lookups contacted the registry: %v", up.Hits())
	}
	checkSumDBHits(t, sumUp, 4)
}

// checkSumDBHits: the checksum database saw want requests, none for a
// registry-only module and none with a credential.
func checkSumDBHits(t *testing.T, sumUp *recordingUpstream, want int) {
	t.Helper()
	for _, hit := range sumUp.Hits() {
		if strings.Contains(hit.requestURI, "acme") || strings.Contains(hit.requestURI, "example.com/lib") {
			t.Errorf("the checksum database saw %s", hit.requestURI)
		}
		if hit.header.Get("Authorization") != "" {
			t.Errorf("the checksum database got a credential")
		}
	}
	if len(sumUp.Hits()) != want {
		t.Errorf("checksum database saw %d requests, want %d", len(sumUp.Hits()), want)
	}
}

// TestGoRegistryPublicLookupsByAllowlist: with publicChecksumLookups a
// lookup the grant's allowlist admits is forwarded without asking the
// registry; private and unlisted modules stay refused.
func TestGoRegistryPublicLookupsByAllowlist(t *testing.T) {
	sumUp := newUpstream(t, okBody("lookup\n"))
	h, up := goRig(t, modHandler(nil), goRegistry(true), nil, sumUp)
	for p, status := range map[string]int{"example.com/lib/y@v1.0.0": 200, "example.com/acme/x@v1.0.0": 410,
		"rsc.io/quote@v1.5.2": 410} {
		if res := h.gr(t, lookupPath+p); res.status != status {
			t.Errorf("lookup %s = %d, want %d", p, res.status, status)
		}
	}
	if len(up.Hits()) != 0 {
		t.Errorf("an allowlist decision probed the registry")
	}
	if len(sumUp.Hits()) != 1 {
		t.Errorf("checksum database saw %d requests, want 1", len(sumUp.Hits()))
	}
}

// TestGoRegistryPublicLookupsByProbe: under packagePolicy registry a
// lookup is forwarded only when a credentialed HEAD of the module's .mod
// answers 200; a 401 there invalidates the credential.
func TestGoRegistryPublicLookupsByProbe(t *testing.T) {
	sumUp := newUpstream(t, okBody("lookup\n"))
	h, up := goRig(t, modHandler(map[string]int{
		goBase + "/rsc.io/quote/@v/v1.5.2.mod": http.StatusOK,
		goBase + "/rsc.io/auth/@v/v1.0.0.mod":  http.StatusUnauthorized,
	}), goRegistry(true), func(g *netprofile.Grant) {
		g.Packages, g.PackagePolicy = nil, netprofile.PackagePolicyRegistry
	}, sumUp)
	if res := h.gr(t, lookupPath+"rsc.io/quote@v1.5.2"); res.status != 200 {
		t.Errorf("probed lookup = %d %q", res.status, res.body)
	}
	for _, p := range []string{"rsc.io/other@v1.0.0", "rsc.io/auth@v1.0.0"} {
		if res := h.gr(t, lookupPath+p); res.status != 410 || h.lastLine(t)["outcome"] != OutcomeChecksumNotPublic {
			t.Errorf("lookup %s = %d %v", p, res.status, h.lastLine(t))
		}
	}
	hits := up.Hits()
	if len(hits) != 3 || hits[0].method != http.MethodHead || hits[0].header.Get("Authorization") == "" {
		t.Errorf("probes = %+v", hits)
	}
	if n := h.creds.invalidations("art-go"); n != 1 {
		t.Errorf("a probe's 401 invalidated %d times, want 1", n)
	}
	checkSumDBHits(t, sumUp, 1)
}

// TestPrivateModulesNeverFallThrough: a profile with route go and a go
// grant refuses the grant's private modules on /go and /go/sumdb with 410
// private_module before any contact, while its public modules (overlap
// included) still flow; a module the registry 404s is served by /go.
func TestPrivateModulesNeverFallThrough(t *testing.T) {
	goUp := newUpstream(t, okBody("v1.0.0\n"))
	sumUp := newUpstream(t, okBody("lookup\n"))
	h, _ := goRig(t, modHandler(nil), goRegistry(false), nil, nil)
	// Widen the public route to overlap the private prefix.
	p := goProfile(goRegistry(false), nil)
	p.Modules = netprofile.Prefixes{"example.com/"}
	h2 := newRegHarnessFrom(t, h, p, goUp, sumUp)
	for _, path := range []string{
		"/go/example.com/acme/x/@v/list",
		"/go/example.com/acme/x/@v/v1.0.0.zip",
		"/go/sumdb/sum.golang.org/lookup/example.com/acme/x@v1.0.0",
	} {
		res := h2.gr(t, path)
		l := h2.lastLine(t)
		if res.status != http.StatusGone || l["outcome"] != OutcomePrivateModule {
			t.Errorf("%s = %d %v, want 410 private_module", path, res.status, l)
		}
		if _, ok := l["registry"]; ok {
			t.Errorf("a /go line carries registry attributes: %v", l)
		}
	}
	if len(goUp.Hits())+len(sumUp.Hits()) != 0 {
		t.Errorf("a private module reached Google: %v %v", goUp.Hits(), sumUp.Hits())
	}
	if res := h2.gr(t, "/r/art-go/example.com/lib/z/@v/list"); res.status != http.StatusNotFound {
		t.Errorf("registry 404 = %d", res.status)
	}
	if res := h2.gr(t, "/go/example.com/lib/z/@v/list"); res.status != http.StatusOK || len(goUp.Hits()) != 1 {
		t.Errorf("public fall-through = %d, %d hits", res.status, len(goUp.Hits()))
	}
	if res := h2.gr(t, "/r/art-go/sumdb/sum.golang.org/lookup/example.com/lib/z@v1.0.0"); res.status != http.StatusOK {
		t.Errorf("public lookup through the registry route = %d %q", res.status, res.body)
	}
}

// newRegHarnessFrom is a second engine over h's world and credentials with
// profile p.
func newRegHarnessFrom(t *testing.T, h *regHarness, p netprofile.Profile, goUp, sumUp *recordingUpstream) *regHarness {
	t.Helper()
	return newRegHarness(t, h.world, h.creds, Config{
		Profiles:   mustProfiles(t, p),
		Registries: h.srv.cfg.Registries,
	}, goUp, sumUp)
}

// gauge is the named int64 gauge's value for the registry.
func gauge(t *testing.T, name, registry string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetrics().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is %T", name, m.Data)
			}
			for _, dp := range g.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("registry")); ok && v.AsString() == registry {
					return dp.Value
				}
			}
		}
	}
	t.Fatalf("no %s point for %s", name, registry)
	return 0
}
