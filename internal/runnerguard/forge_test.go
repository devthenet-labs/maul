// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnerguard

import (
	"context"
	"errors"
	"math/rand"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// registryProfiles is a set with one profile, web, for the target
// repository: an npm grant on a JFrog host with a redirect pattern, and a
// go grant with private modules beside the public route.
func registryProfiles(t testing.TB) netprofile.Set {
	t.Helper()
	set, err := netprofile.Parse([]byte(`[{"name":"web","route":"go","port":8091,` +
		`"repositories":["devthenet-labs/maul-target"],"modules":["github.com/google/uuid"],"registries":[` +
		`{"name":"art-npm","ecosystem":"npm","hosts":["acme.jfrog.io","*.cdn.acme.example"],` +
		`"packagePolicy":"registry"},` +
		`{"name":"art-go","ecosystem":"go","hosts":["acme.jfrog.io"],"packages":["acme.example/"],` +
		`"privateModules":["acme.example/"]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// fakeForges is a Forge host list, or a list failure.
type fakeForges struct {
	hosts []string
	err   error
	calls int
}

func (f *fakeForges) WebHosts(context.Context, string) ([]string, error) {
	f.calls++
	return f.hosts, f.err
}

// TestProfileCopiesGrants: the requested profile carries the operator's
// grants exactly, as the profile set holds them.
func TestProfileCopiesGrants(t *testing.T) {
	set := registryProfiles(t)
	g := Guard{Enabled: true, ProfilesEnabled: true, Profiles: set, ProxyBase: proxyBase(t)}
	spec := remediateSpec(pinned)
	if applied, skipped := g.Profile(&spec, targetURL); applied != "web" || skipped != "" {
		t.Fatalf("Profile = (%q, %q), want web", applied, skipped)
	}
	p, _ := set.Get("web")
	want := jobs.NetworkProfile{Name: "web", Route: "go", Registries: p.Registries,
		ProxyURL: "http://maul-egress-proxy.maul.svc.cluster.local:8091"}
	if !reflect.DeepEqual(*spec.NetworkProfile, want) {
		t.Errorf("spec.NetworkProfile = %+v, want %+v", spec.NetworkProfile, want)
	}
	spec.NetworkProfile.Registries[0].Hosts[0] = "mutated.example"
	if again, _ := set.Get("web"); again.Registries[0].Hosts[0] != "acme.jfrog.io" {
		t.Error("the spec's grants alias the profile set's")
	}
}

// TestCheckForges: a grant host that is, lies below or is matched by a
// pattern against a Forge's host refuses the profile naming the host; a
// spec without a profile or grants lists nothing; a list failure is
// returned for a retry; no lister refuses a profile with grants.
func TestCheckForges(t *testing.T) {
	ctx := context.Background()
	withGrants := func(t *testing.T) *jobs.Spec {
		spec := remediateSpec(pinned)
		g := Guard{ProfilesEnabled: true, Profiles: registryProfiles(t), ProxyBase: proxyBase(t)}
		if applied, _ := g.Profile(&spec, targetURL); applied == "" {
			t.Fatal("no profile")
		}
		return &spec
	}
	for _, tt := range []struct {
		name   string
		forges []string
		host   string
	}{
		{"exact", []string{"github.com", "acme.jfrog.io"}, "acme.jfrog.io"},
		{"below", []string{"jfrog.io"}, "acme.jfrog.io"},
		{"pattern", []string{"git.cdn.acme.example"}, "*.cdn.acme.example"},
		{"case", []string{"ACME.JFROG.IO"}, "acme.jfrog.io"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForges{hosts: tt.forges}
			err := Guard{Forges: f}.CheckForges(ctx, "maul", withGrants(t))
			if !errors.Is(err, jobs.ErrNetworkProfileRefused) || !strings.Contains(err.Error(), tt.host) {
				t.Errorf("CheckForges = %v, want a refusal naming %s", err, tt.host)
			}
		})
	}

	f := &fakeForges{hosts: []string{"github.com", "ghe.corp.example"}}
	if err := (Guard{Forges: f}).CheckForges(ctx, "maul", withGrants(t)); err != nil {
		t.Errorf("no Forge matches: %v", err)
	}
	step1 := remediateSpec(pinned)
	step1.NetworkProfile = &jobs.NetworkProfile{Name: "go", Route: "go"}
	f = &fakeForges{err: errors.New("boom")}
	for _, spec := range []*jobs.Spec{{}, &step1} {
		if err := (Guard{Forges: f}).CheckForges(ctx, "maul", spec); err != nil {
			t.Errorf("a spec without grants: %v", err)
		}
		if err := (Guard{}).CheckForges(ctx, "maul", spec); err != nil {
			t.Errorf("a spec without grants and no lister: %v", err)
		}
	}
	if f.calls != 0 {
		t.Errorf("Forges listed %d times for specs without grants, want none", f.calls)
	}
	if err := (Guard{Forges: f}).CheckForges(ctx, "maul", withGrants(t)); err == nil ||
		errors.Is(err, jobs.ErrNetworkProfileRefused) {
		t.Errorf("a list failure = %v, want it returned unrefused", err)
	}
	if err := (Guard{}).CheckForges(ctx, "maul", withGrants(t)); !errors.Is(err, jobs.ErrNetworkProfileRefused) {
		t.Errorf("no lister = %v, want a refusal", err)
	}
}

// TestCheckForgesRefusesHostlessGrant: a grant that lists no hosts (a
// hand-written or pre-chart profile, where the registry URL host is not
// rendered first) is refused at launch, whether or not any Forge exists,
// rather than passing a check that loops over nothing.
func TestCheckForgesRefusesHostlessGrant(t *testing.T) {
	ctx := context.Background()
	spec := remediateSpec(pinned)
	spec.NetworkProfile = &jobs.NetworkProfile{Name: "npm", Route: "npm", Registries: []netprofile.Grant{
		{Name: "ok", Ecosystem: "go", Hosts: []string{"goproxy.acme.example"},
			PackagePolicy: netprofile.PackagePolicyRegistry},
		{Name: "ghe-npm", Ecosystem: "npm", PackagePolicy: netprofile.PackagePolicyRegistry},
	}}
	for _, forges := range [][]string{nil, {"github.com", "ghe.corp.example"}} {
		err := Guard{Forges: &fakeForges{hosts: forges}}.CheckForges(ctx, "maul", &spec)
		if !errors.Is(err, jobs.ErrNetworkProfileRefused) || !strings.Contains(err.Error(), "ghe-npm") {
			t.Errorf("Forges %v: CheckForges = %v, want a refusal naming grant ghe-npm", forges, err)
		}
	}
}

// TestPropertyForgeRefusal: for generated grant host sets and Forge host
// sets disjoint from them (no Forge host equal to, above or below a grant
// host), the decision is step 1's (no refusal); adding a grant host, or a
// name below it, to the Forge hosts refuses the profile naming it. Seeded
// so the gate is deterministic.
func TestPropertyForgeRefusal(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	label := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyz"
		b := make([]byte, 2+rng.Intn(6))
		for i := range b {
			b[i] = letters[rng.Intn(len(letters))]
		}
		return string(b)
	}
	host := func(tld string) string { return label() + "." + label() + "." + tld }
	for i := range 1000 {
		grantHosts := make([]string, 0, 3)
		var forgeHosts []string
		for range 1 + rng.Intn(3) {
			grantHosts = append(grantHosts, host("example"))
		}
		for range rng.Intn(4) {
			forgeHosts = append(forgeHosts, host("test"))
		}
		p := &jobs.NetworkProfile{Name: "p", Registries: []netprofile.Grant{{Name: "r", Ecosystem: "npm",
			Hosts: grantHosts, PackagePolicy: netprofile.PackagePolicyRegistry}}}
		if err := ForgeRefusal(p, forgeHosts); err != nil {
			t.Fatalf("iteration %d: disjoint %v vs %v refused: %v", i, grantHosts, forgeHosts, err)
		}
		pick := grantHosts[rng.Intn(len(grantHosts))]
		forge := pick
		if rng.Intn(2) == 0 {
			forge = strings.Join(strings.Split(pick, ".")[1:], ".") // a Forge above the grant host
		}
		forgeHosts = append(slices.Clone(forgeHosts), forge)
		rng.Shuffle(len(forgeHosts), func(a, b int) { forgeHosts[a], forgeHosts[b] = forgeHosts[b], forgeHosts[a] })
		err := ForgeRefusal(p, forgeHosts)
		if !errors.Is(err, jobs.ErrNetworkProfileRefused) || !strings.Contains(err.Error(), pick) {
			t.Fatalf("iteration %d: %v with Forge %s = %v, want a refusal naming %s", i, grantHosts, forge, err, pick)
		}
	}
}

// TestProxyHostForms: an in-cluster Service name gives its four spellings,
// another dotted host itself, a single label only its http URL, and no
// proxy base none.
func TestProxyHostForms(t *testing.T) {
	for raw, want := range map[string][]string{
		"http://maul-egress-proxy.maul.svc.cluster.local": {"maul-egress-proxy.maul.svc.cluster.local",
			"maul-egress-proxy.maul.svc", "maul-egress-proxy.maul", "http://maul-egress-proxy"},
		"http://Egress.Corp.Example": {"egress.corp.example"},
		"http://10.100.50.9":         {"10.100.50.9"},
		"http://localhost":           {"http://localhost"},
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := (Guard{ProxyBase: u}).ProxyHostForms(); !slices.Equal(got, want) {
			t.Errorf("ProxyHostForms(%s) = %v, want %v", raw, got, want)
		}
	}
	if got := (Guard{}).ProxyHostForms(); got != nil {
		t.Errorf("no proxy base: %v", got)
	}
}

// tunnelSpec is a remediate spec carrying the target's profile from a set
// whose one profile, web, is a tunnel to the Python index and its file
// host with the pip preset and its own upload caps.
func tunnelSpec(t *testing.T) *jobs.Spec {
	t.Helper()
	spec, _ := tunnelSpecSet(t)
	return spec
}

// tunnelSpecSet is tunnelSpec with the profile set it was applied from, so
// a test can check the set after changing the spec.
func tunnelSpecSet(t *testing.T) (*jobs.Spec, netprofile.Set) {
	t.Helper()
	set, err := netprofile.Parse([]byte(`[{"name":"web","route":"tunnel","port":8092,` +
		`"repositories":["devthenet-labs/maul-target"],"modules":[],"bakedModCache":"",` +
		`"hosts":["pypi.org","files.pythonhosted.org"],"clients":["pip"],` +
		`"upload":{"perTunnel":1048576,"perSource":8388608}}]`))
	if err != nil {
		t.Fatal(err)
	}
	spec := remediateSpec(pinned)
	g := Guard{ProfilesEnabled: true, Profiles: set, ProxyBase: proxyBase(t)}
	if applied, skipped := g.Profile(&spec, targetURL); applied != "web" {
		t.Fatalf("Profile = (%q, %q), want web", applied, skipped)
	}
	return &spec, set
}

// TestProfileCopiesTunnel: the requested profile carries the operator's
// tunnel hosts and client presets exactly (step 3), never aliasing the
// profile set's; the upload caps stay with the egress proxy.
func TestProfileCopiesTunnel(t *testing.T) {
	spec, set := tunnelSpecSet(t)
	want := jobs.NetworkProfile{Name: "web", Route: "tunnel",
		ProxyURL: "http://maul-egress-proxy.maul.svc.cluster.local:8092",
		Hosts:    []string{"pypi.org", "files.pythonhosted.org"}, Clients: []string{"pip"}}
	if !reflect.DeepEqual(*spec.NetworkProfile, want) {
		t.Fatalf("spec.NetworkProfile = %+v, want %+v", spec.NetworkProfile, want)
	}
	spec.NetworkProfile.Hosts[0] = "evil.example"
	spec.NetworkProfile.Clients[0] = "npm"
	again, _ := set.Get("web")
	if again.Hosts[0] != "pypi.org" || again.Clients[0] != "pip" {
		t.Error("the spec's tunnel hosts or clients alias the profile set's")
	}
}

// TestCheckForgesTunnel: a tunnel host that is, or lies below, a web host
// of a Forge (forge.WebHosts: the parent of a data-residency api. host
// included) refuses the whole profile naming the host; a tunnel-only
// profile with no grants is still checked; unrelated Forges pass; a list
// failure is returned unrefused, for a retry, so nothing launches; no
// lister refuses.
func TestCheckForgesTunnel(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		forges []string
		host   string
	}{
		{"exact", []string{"github.com", "pypi.org"}, "pypi.org"},
		{"below a forge", []string{"pythonhosted.org"}, "files.pythonhosted.org"},
		{"case", []string{"PyPI.org"}, "pypi.org"},
		{"web host of an api. forge", []string{"api.pythonhosted.org", "pythonhosted.org"},
			"files.pythonhosted.org"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForges{hosts: tt.forges}
			err := Guard{Forges: f}.CheckForges(ctx, "maul", tunnelSpec(t))
			if !errors.Is(err, jobs.ErrNetworkProfileRefused) || !strings.Contains(err.Error(), tt.host) ||
				!strings.Contains(err.Error(), "profile web") {
				t.Errorf("CheckForges = %v, want a refusal of profile web naming %s", err, tt.host)
			}
			if f.calls != 1 {
				t.Errorf("Forges listed %d times, want once", f.calls)
			}
		})
	}

	f := &fakeForges{hosts: []string{"github.com", "ghe.corp.example", "api.octo.ghe.com", "octo.ghe.com"}}
	if err := (Guard{Forges: f}).CheckForges(ctx, "maul", tunnelSpec(t)); err != nil {
		t.Errorf("no Forge matches: %v", err)
	}
	f = &fakeForges{err: errors.New("boom")}
	if err := (Guard{Forges: f}).CheckForges(ctx, "maul", tunnelSpec(t)); err == nil ||
		errors.Is(err, jobs.ErrNetworkProfileRefused) {
		t.Errorf("a list failure = %v, want it returned unrefused", err)
	}
	if err := (Guard{}).CheckForges(ctx, "maul", tunnelSpec(t)); !errors.Is(err, jobs.ErrNetworkProfileRefused) {
		t.Errorf("no lister = %v, want a refusal", err)
	}
}

// TestPropertyForgeRefusalTunnel: for generated tunnel hosts and Forge web
// hosts disjoint from them (none equal to, above or below a tunnel host),
// nothing is refused; adding a tunnel host, or a name above it, to the
// Forge hosts refuses the profile naming it. Seeded so the gate is
// deterministic.
func TestPropertyForgeRefusalTunnel(t *testing.T) {
	rng := rand.New(rand.NewSource(20261016))
	label := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyz"
		b := make([]byte, 3+rng.Intn(6))
		for i := range b {
			b[i] = letters[rng.Intn(len(letters))]
		}
		return string(b)
	}
	host := func(tld string) string { return label() + "." + label() + "." + tld }
	for i := range 1000 {
		var forgeHosts []string
		tunnelHosts := make([]string, 0, 4)
		for range 1 + rng.Intn(4) {
			tunnelHosts = append(tunnelHosts, host("net"))
		}
		for range rng.Intn(4) {
			forgeHosts = append(forgeHosts, host("org"))
		}
		p := &jobs.NetworkProfile{Name: "p", Route: "tunnel", Hosts: tunnelHosts}
		if err := ForgeRefusal(p, forgeHosts); err != nil {
			t.Fatalf("iteration %d: disjoint %v vs %v refused: %v", i, tunnelHosts, forgeHosts, err)
		}
		pick := tunnelHosts[rng.Intn(len(tunnelHosts))]
		forge := pick
		if rng.Intn(2) == 0 {
			forge = strings.Join(strings.Split(pick, ".")[1:], ".") // a Forge above the tunnel host
		}
		forgeHosts = append(slices.Clone(forgeHosts), forge)
		rng.Shuffle(len(forgeHosts), func(a, b int) { forgeHosts[a], forgeHosts[b] = forgeHosts[b], forgeHosts[a] })
		err := ForgeRefusal(p, forgeHosts)
		if !errors.Is(err, jobs.ErrNetworkProfileRefused) || !strings.Contains(err.Error(), pick) {
			t.Fatalf("iteration %d: %v with Forge %s = %v, want a refusal naming %s", i, tunnelHosts, forge, err,
				pick)
		}
	}
}
