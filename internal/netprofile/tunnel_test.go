// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// tunnelProfile is a valid route-tunnel profile every rejection case below
// breaks in one way.
func tunnelProfile() Profile {
	return Profile{
		Name: "web", Route: RouteTunnel, Port: 8092, Host: "github.com",
		Repositories: []string{"devthenet-labs/maul-pip-fixture"},
		Hosts:        Hosts{"pypi.org", "files.pythonhosted.org"},
		Clients:      []string{ClientPip},
	}
}

func TestParseTunnelProfiles(t *testing.T) {
	const doc = `[
	  {"name":"web","route":"tunnel","port":8092,"repositories":["devthenet-labs/maul-pip-fixture"],
	   "modules":[],"bakedModCache":"","hosts":["pypi.org","files.pythonhosted.org"],"clients":["pip","uv"]},
	  {"name":"go","route":"go","port":8090,"repositories":["devthenet-labs/maul-preview-demo"],
	   "modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod","allowPseudoVersions":false,
	   "hosts":["registry.npmjs.org"],"clients":["npm"],"upload":{"perTunnel":1048576,"perSource":8388608}}]`
	set, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	web, _ := set.Get("web")
	if web.HasGoRoute() || !web.HasTunnel() || !web.Hosts.Allows("files.pythonhosted.org") ||
		!slices.Equal(web.Clients, []string{"pip", "uv"}) || web.Upload != nil {
		t.Errorf("web = %+v", web)
	}
	gop, _ := set.Get("go")
	if !gop.HasGoRoute() || !gop.HasTunnel() || gop.Upload == nil || *gop.Upload != (Upload{1 << 20, 8 << 20}) {
		t.Errorf("go = %+v", gop)
	}
}

// TestStepOneProfileHasNoTunnel: a step-1 profile has neither a tunnel nor
// its keys.
func TestStepOneProfileHasNoTunnel(t *testing.T) {
	step1, err := Parse([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := step1.Get("go")
	if p.HasTunnel() || !p.HasGoRoute() || p.Hosts != nil || p.Clients != nil || p.Upload != nil {
		t.Errorf("step-1 profile = %+v", p)
	}
	out, err := json.Marshal(step1.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"hosts"`, `"clients"`, `"upload"`} {
		if bytes.Contains(out, []byte(key)) {
			t.Errorf("a profile without a tunnel renders %s: %s", key, out)
		}
	}
}

// TestTunnelFieldsCopied: profiles handed out do not alias the set's
// hosts, clients or upload caps.
func TestTunnelFieldsCopied(t *testing.T) {
	set, err := NewSet([]Profile{tunnelProfile(), func() Profile {
		p := validProfile()
		p.Hosts, p.Upload = Hosts{"registry.npmjs.org"}, &Upload{PerTunnel: 1 << 20}
		return p
	}()})
	if err != nil {
		t.Fatal(err)
	}
	web, _ := set.Get("web")
	gop, _ := set.Get("go")
	web.Hosts[0], web.Clients[0] = "evil.example.com", "jvm"
	gop.Upload.PerTunnel = 1
	again, _ := set.Get("web")
	againGo, _ := set.Get("go")
	if again.Hosts[0] != "pypi.org" || again.Clients[0] != "pip" || againGo.Upload.PerTunnel != 1<<20 {
		t.Error("a returned profile aliases the set's hosts, clients or upload")
	}
}

func TestTunnelValidateRejects(t *testing.T) {
	npmGrant := Grant{Name: "npm-mirror", Ecosystem: EcosystemNPM, Hosts: []string{"acme.jfrog.io"},
		PackagePolicy: PackagePolicyRegistry}
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"tunnel without hosts", func(p *Profile) { p.Hosts, p.Clients = nil, nil }, "needs hosts"},
		{"tunnel with modules", func(p *Profile) { p.Modules = Prefixes{"example.com/m"} }, "go route's"},
		{"tunnel with bakedModCache", func(p *Profile) { p.BakedModCache = "/opt/go/pkg/mod" }, "go route's"},
		{"tunnel with allowPseudoVersions", func(p *Profile) { p.AllowPseudoVersions = true }, "go route's"},
		{"hosts without a route", func(p *Profile) {
			p.Route, p.Registries = "", []Grant{npmGrant}
			p.Clients = nil
		}, "need route: tunnel"},
		{"route bogus", func(p *Profile) { p.Route = "connect" }, "not supported"},
		{"wildcard", func(p *Profile) { p.Hosts = Hosts{"*.npmjs.org"} }, "not an exact"},
		{"dotted IP", func(p *Profile) { p.Hosts = Hosts{"1.2.3.4"} }, "not an exact"},
		{"hex IP", func(p *Profile) { p.Hosts = Hosts{"0x7f.1"} }, "not an exact"},
		{"decimal IP", func(p *Profile) { p.Hosts = Hosts{"2130706433"} }, "not an exact"},
		{"short IP", func(p *Profile) { p.Hosts = Hosts{"127.1"} }, "not an exact"},
		{"trailing dot", func(p *Profile) { p.Hosts = Hosts{"pypi.org."} }, "not an exact"},
		{"uppercase", func(p *Profile) { p.Hosts = Hosts{"PyPI.org"} }, "not an exact"},
		{"underscore", func(p *Profile) { p.Hosts = Hosts{"py_pi.org"} }, "not an exact"},
		{"single label", func(p *Profile) { p.Hosts = Hosts{"pypi"} }, "not an exact"},
		{"svc", func(p *Profile) { p.Hosts = Hosts{"svc"} }, "not an exact"},
		{"in-cluster name", func(p *Profile) { p.Hosts = Hosts{"nexus.tools.svc"} }, "svc"},
		{"forge", func(p *Profile) { p.Hosts = Hosts{"github.com"} }, "github.com"},
		{"below a forge", func(p *Profile) { p.Hosts = Hosts{"objects.githubusercontent.com"} }, "githubusercontent"},
		{"model API", func(p *Profile) { p.Hosts = Hosts{"api.anthropic.com"} }, "anthropic.com"},
		{"control plane", func(p *Profile) { p.Hosts = Hosts{"abc.gr7.us-east-1.eks.amazonaws.com"} }, "eks"},
		{"publish endpoint", func(p *Profile) { p.Hosts = Hosts{"upload.pypi.org"} }, "upload.pypi.org"},
		{"the profile's forge host", func(p *Profile) {
			p.Host, p.Hosts = "ghe.corp.example.com", Hosts{"ghe.corp.example.com"}
		}, "ghe.corp.example.com"},
		{"below the profile's forge host", func(p *Profile) {
			p.Host, p.Hosts = "ghe.corp.example.com", Hosts{"codeload.ghe.corp.example.com"}
		}, "ghe.corp.example.com"},
		{"duplicate host", func(p *Profile) { p.Hosts = Hosts{"pypi.org", "pypi.org"} }, "twice"},
		{"too many hosts", func(p *Profile) {
			p.Hosts = nil
			for i := range MaxTunnelHosts + 1 {
				p.Hosts = append(p.Hosts, fmt.Sprintf("h%d.example.org", i))
			}
		}, "at most 64"},
		{"a grant's host", func(p *Profile) {
			p.Registries = []Grant{npmGrant}
			p.Hosts = Hosts{"acme.jfrog.io"}
			p.Clients = nil
		}, "bypass the grant"},
		{"a grant's pattern", func(p *Profile) {
			g := npmGrant
			g.Hosts = []string{"acme.jfrog.io", "*.cdn.acme.example.com"}
			p.Registries = []Grant{g}
			p.Hosts = Hosts{"eu.cdn.acme.example.com"}
			p.Clients = nil
		}, "bypass the grant"},
		{"the go route's module proxy", func(p *Profile) {
			*p = validProfile()
			p.Hosts = Hosts{"registry.npmjs.org", "proxy.golang.org"}
		}, "bypass the route's module allowlist"},
		{"the go route's checksum database", func(p *Profile) {
			*p = validProfile()
			p.Hosts = Hosts{"sum.golang.org"}
		}, "bypass the route's module allowlist"},
		{"clients without hosts", func(p *Profile) {
			*p = validProfile()
			p.Clients = []string{ClientNPM}
		}, "set hosts"},
		{"unknown client", func(p *Profile) { p.Clients = []string{"pnpm"} }, "not one of"},
		{"duplicate client", func(p *Profile) { p.Clients = []string{ClientPip, ClientPip} }, "twice"},
		{"too many clients", func(p *Profile) { p.Clients = append(slices.Clone(Clients), ClientPip) }, "at most 7"},
		{"npm client beside an npm grant", func(p *Profile) {
			p.Registries = []Grant{npmGrant}
			p.Clients = []string{ClientNPM}
		}, "same package manager"},
		{"yarn client beside an npm grant", func(p *Profile) {
			p.Registries = []Grant{npmGrant}
			p.Clients = []string{ClientYarn}
		}, "same package manager"},
		{"uv client beside a pypi grant", func(p *Profile) {
			p.Registries = []Grant{{Name: "py", Ecosystem: EcosystemPyPI, Hosts: []string{"pypi.acme.example.com"},
				PackagePolicy: PackagePolicyRegistry}}
			p.Clients = []string{ClientUV}
		}, "same package manager"},
		{"upload without hosts", func(p *Profile) {
			*p = validProfile()
			p.Upload = &Upload{PerTunnel: 1 << 20}
		}, "set hosts"},
		{"empty upload", func(p *Profile) { p.Upload = &Upload{} }, "neither"},
		{"upload below the floor", func(p *Profile) { p.Upload = &Upload{PerTunnel: MinUploadBytes - 1} }, "outside"},
		{"upload above the ceiling", func(p *Profile) { p.Upload = &Upload{PerSource: MaxUploadBytes + 1} }, "outside"},
		{"negative upload", func(p *Profile) { p.Upload = &Upload{PerSource: -1} }, "outside"},
		{"perTunnel above perSource", func(p *Profile) {
			p.Upload = &Upload{PerTunnel: 2 << 20, PerSource: 1 << 20}
		}, "above"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tunnelProfile()
			tt.mutate(&p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want an error naming %q", err, tt.want)
			}
		})
	}
}

func TestTunnelValidateAdmits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{"tunnel", func(*Profile) {}},
		{"go and hosts", func(p *Profile) {
			*p = validProfile()
			p.Hosts, p.Clients = Hosts{"registry.npmjs.org"}, []string{ClientNPM}
		}},
		{"a go upstream on a tunnel-only profile", func(p *Profile) {
			p.Hosts = Hosts{"proxy.golang.org", "sum.golang.org"}
		}},
		{"go and a host beside its upstreams", func(p *Profile) {
			*p = validProfile()
			p.Hosts = Hosts{"golang.org", "go.dev"}
		}},
		{"every client", func(p *Profile) { p.Clients = slices.Clone(Clients) }},
		{"punycode and an alphanumeric TLD", func(p *Profile) { p.Hosts = Hosts{"xn--p1ai.xn--p1ai", "mirror.a1"} }},
		{"64 hosts", func(p *Profile) {
			p.Hosts = nil
			for i := range MaxTunnelHosts {
				p.Hosts = append(p.Hosts, fmt.Sprintf("h%d.example.org", i))
			}
		}},
		{"upload bounds", func(p *Profile) { p.Upload = &Upload{PerTunnel: MinUploadBytes, PerSource: MaxUploadBytes} }},
		{"perTunnel alone", func(p *Profile) { p.Upload = &Upload{PerTunnel: 1 << 20} }},
		{"perSource alone", func(p *Profile) { p.Upload = &Upload{PerSource: 8 << 20} }},
		{"tunnel beside grants", func(p *Profile) {
			p.Registries = []Grant{{Name: "npm-mirror", Ecosystem: EcosystemNPM, Hosts: []string{"acme.jfrog.io"},
				PackagePolicy: PackagePolicyRegistry}}
			p.Clients = []string{ClientPip, ClientCargo}
		}},
		{"a host beside, not below, a grant's pattern", func(p *Profile) {
			p.Registries = []Grant{{Name: "npm-mirror", Ecosystem: EcosystemNPM,
				Hosts: []string{"acme.jfrog.io", "*.cdn.acme.example.com"}, PackagePolicy: PackagePolicyRegistry}}
			p.Hosts = Hosts{"cdn.acme.example.com", "pypi.org"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tunnelProfile()
			tt.mutate(&p)
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}

func TestClientWarnings(t *testing.T) {
	p := tunnelProfile()
	if w := p.ClientWarnings(); len(w) != 0 {
		t.Errorf("pip with both PyPI hosts warns %q", w)
	}
	p.Hosts = Hosts{"pypi.org"}
	w := p.ClientWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "files.pythonhosted.org") || !strings.Contains(w[0], "client pip") {
		t.Errorf("pip without files.pythonhosted.org warns %q", w)
	}
	// yarn needs either registry host; one is enough.
	p.Hosts, p.Clients = Hosts{"registry.npmjs.org"}, []string{ClientYarn}
	if w := p.ClientWarnings(); len(w) != 0 {
		t.Errorf("yarn with registry.npmjs.org warns %q", w)
	}
	p.Hosts = Hosts{"npm.acme.example.com"}
	p.Clients = []string{ClientNPM, ClientYarn, ClientJVM}
	if w := p.ClientWarnings(); len(w) != 3 {
		t.Errorf("three clients with a private mirror only warn %q, want three", w)
	}
	for _, c := range Clients {
		if _, ok := clientHosts[c]; !ok {
			t.Errorf("client %s has no usual hosts", c)
		}
	}
}

func TestTunnelDeniedHost(t *testing.T) {
	ctx := DenyContext{Hosts: []string{"ghe.corp.example.com"}}
	for _, h := range []string{
		// DeniedHost's table, and every name below it
		"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
		"api.anthropic.com", "x.openai.azure.com", "bedrock-runtime.us-east-1.amazonaws.com",
		"us-east5-aiplatform.googleapis.com", "x.us-east5-aiplatform.googleapis.com",
		"kubernetes.default", "x.kubernetes.default", "kubernetes.default.svc.cluster.local",
		"ghe.corp.example.com", "uploads.ghe.corp.example.com",
		// the tunnel's own tables
		"ghcr.io", "pkg-containers.ghcr.io", "api.githubcopilot.com", "gitlab.com", "registry.gitlab.com",
		"bitbucket.org", "codeberg.org", "dev.azure.com", "pkgs.dev.azure.com", "x.visualstudio.com", "git.sr.ht",
		"claude.ai", "claude.com", "api.claude.com", "chatgpt.com",
		"abc.gr7.us-east-1.eks.amazonaws.com", "x.hcp.westeurope.azmk8s.io", "container.googleapis.com",
		"x.k8s.ondigitalocean.com", "x.linodelke.net",
		"localhost", "a.localhost", "printer.local", "metadata.google.internal", "nexus.corp.internal",
		"nexus.tools.svc", "x.cluster.local", "router.home.arpa", "4.3.2.1.in-addr.arpa", "x.ip6.arpa",
		"x.invalid", "x.test", "x.example",
		"upload.pypi.org", "www.nuget.org",
		// addresses and single labels
		"1.2.3.4", "127.1", "0x7f.1", "2130706433", "::1", "[::1]", "pypi", "",
	} {
		if err := DeniedTunnelHost(h, ctx); err == nil || !errors.Is(err, ErrDeniedHost) {
			t.Errorf("DeniedTunnelHost(%q) = %v, want a refusal", h, err)
		}
	}
	for _, h := range []string{
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "registry.yarnpkg.com", "api.nuget.org",
		"index.crates.io", "static.crates.io", "repo.maven.apache.org", "plugins.gradle.org",
		"plugins-artifacts.gradle.org", "dl.google.com", "example.com", "notgithub.com", "gitlab.example.com",
		"storage.googleapis.com", "bucket.s3.us-east-1.amazonaws.com", "xn--p1ai.xn--p1ai",
	} {
		if err := DeniedTunnelHost(h, ctx); err != nil {
			t.Errorf("DeniedTunnelHost(%q) = %v, want admitted", h, err)
		}
	}
}

// TestDeniedHostCoversKnownModelHosts: the model hosts maul's runners
// reach that no constant in this repository spells. The codex and copilot
// runners take their endpoints from their own CLIs, so there is nothing to
// pin to; this list is hard-coded on purpose, and is the place to add a
// host when a runner's CLI moves.
func TestDeniedHostCoversKnownModelHosts(t *testing.T) {
	for _, h := range []string{
		"api.openai.com", "chatgpt.com", "auth.openai.com", "x.openai.azure.com",
		"api.githubcopilot.com", "api.individual.githubcopilot.com", "copilot-proxy.githubusercontent.com",
		"api.anthropic.com", "claude.ai", "claude.com", "statsig.anthropic.com",
		"generativelanguage.googleapis.com", "us-central1-aiplatform.googleapis.com",
	} {
		if err := DeniedTunnelHost(h, DenyContext{}); err == nil {
			t.Errorf("the tunnel admits the model host %s", h)
		}
	}
}

// TestPropertyDeniedTunnelMonotone: a name below a denied tunnel host is
// denied, whichever rule denied the parent.
func TestPropertyDeniedTunnelMonotone(t *testing.T) {
	denied := 0
	prop := func(d genHost, l uint8) bool {
		if strings.Count(string(d), ".") == 0 || DeniedTunnelHost(string(d), DenyContext{}) == nil {
			return true
		}
		denied++
		label := tunnelLabelPool[int(l)%len(tunnelLabelPool)]
		return DeniedTunnelHost(label+"."+string(d), DenyContext{}) != nil
	}
	if err := quick.Check(prop, quickConfig(20000)); err != nil {
		t.Fatal(err)
	}
	if denied < 500 {
		t.Fatalf("only %d generated hosts were denied; the generator no longer reaches the table", denied)
	}
}

// TestPropertyDeniedTunnelCoversDeniedHost: the tunnel denies everything a
// registry may not be, bar the internal admissions (a registry host may be
// an internal address or in-cluster name; a tunnel host never).
func TestPropertyDeniedTunnelCoversDeniedHost(t *testing.T) {
	prop := func(h genHost) bool {
		return DeniedHost(string(h), DenyContext{}) == nil || DeniedTunnelHost(string(h), DenyContext{}) != nil
	}
	if err := quick.Check(prop, quickConfig(20000)); err != nil {
		t.Fatal(err)
	}
}

// genTunnelSet is a valid profile set of 1-4 profiles, mixing go, tunnel
// and go-plus-hosts profiles with and without clients and upload caps.
type genTunnelSet []Profile

func (genTunnelSet) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(4)
	out := make([]Profile, 0, n)
	for i := range n {
		p := Profile{
			Name: fmt.Sprintf("p%d", i), Port: 9000 + i, Host: "github.com",
			Repositories: []string{fmt.Sprintf("acme/r%d", i)},
		}
		kind := r.Intn(3)
		if kind != 1 {
			p.Route, p.Modules = RouteGo, Prefixes{"example.com/m"}
			p.AllowPseudoVersions = r.Intn(2) == 0
		} else {
			p.Route = RouteTunnel
		}
		if kind > 0 {
			seen := map[string]bool{}
			for range 1 + r.Intn(4) {
				h := string(genTunnelHost("").Generate(r, 0).Interface().(genTunnelHost))
				if !seen[h] && DeniedTunnelHost(h, DenyContext{Hosts: []string{p.Host}}) == nil {
					seen[h] = true
					p.Hosts = append(p.Hosts, h)
				}
			}
			if len(p.Hosts) == 0 {
				p.Hosts = Hosts{"pypi.org"}
			}
			for _, c := range Clients {
				if r.Intn(3) == 0 {
					p.Clients = append(p.Clients, c)
				}
			}
			if r.Intn(2) == 0 {
				p.Upload = &Upload{PerTunnel: MinUploadBytes + int64(r.Intn(1<<20))}
				if r.Intn(2) == 0 {
					p.Upload.PerSource = p.Upload.PerTunnel + int64(r.Intn(1<<20))
				}
			}
		}
		out = append(out, p)
	}
	return reflect.ValueOf(genTunnelSet(out))
}

// TestPropertyTunnelJSONRoundTrip: a valid set renders to JSON that
// parses back to the same set, and a profile without hosts renders none
// of the tunnel's keys.
func TestPropertyTunnelJSONRoundTrip(t *testing.T) {
	prop := func(g genTunnelSet) bool {
		set, err := NewSet(g)
		if err != nil {
			t.Logf("generated set invalid: %v", err)
			return false
		}
		first, err := json.Marshal(set.Profiles())
		if err != nil {
			return false
		}
		back, err := Parse(first)
		if err != nil {
			t.Logf("Parse(%s): %v", first, err)
			return false
		}
		second, err := json.Marshal(back.Profiles())
		if err != nil || !bytes.Equal(first, second) {
			return false
		}
		for _, p := range set.Profiles() {
			one, _ := json.Marshal(p)
			if !p.HasTunnel() && (bytes.Contains(one, []byte(`"hosts"`)) || bytes.Contains(one, []byte(`"clients"`)) ||
				bytes.Contains(one, []byte(`"upload"`))) {
				return false
			}
			q, _ := back.Get(p.Name)
			if !slices.Equal(p.Hosts, q.Hosts) || !slices.Equal(p.Clients, q.Clients) ||
				!reflect.DeepEqual(p.Upload, q.Upload) || p.Route != q.Route {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(2000)); err != nil {
		t.Fatal(err)
	}
}

// TestClientEcosystem: the npm and yarn presets configure npm's registry
// settings and pip and uv pypi's; the rest have no grant ecosystem.
func TestClientEcosystem(t *testing.T) {
	want := map[string]string{ClientNPM: EcosystemNPM, ClientYarn: EcosystemNPM, ClientPip: EcosystemPyPI,
		ClientUV: EcosystemPyPI}
	for _, c := range Clients {
		eco, ok := ClientEcosystem(c)
		if w, has := want[c]; eco != w || ok != has {
			t.Errorf("ClientEcosystem(%s) = %q, %v; want %q, %v", c, eco, ok, w, has)
		}
	}
}
