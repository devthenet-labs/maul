// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"net/netip"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// TestChartNetworkProfilesParse: the profile set the chart renders, from
// the on-fixture, is one netprofile.Parse accepts (what the egress proxy and
// both job controllers do at startup), it is the same string in all three
// ConfigMaps, and the defaults the chart applies are the package's.
func TestChartNetworkProfilesParse(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := append([]string{"template", "maul", ".", "--namespace", "maul"}, egressOnArgs...)
	output, err := exec.CommandContext(t.Context(), "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	got := map[string]string{}
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() != "ConfigMap" {
			continue
		}
		if v, ok, _ := unstructured.NestedString(u.Object, "data", netprofile.EnvProfiles); ok {
			got[u.GetName()] = v
		}
	}
	want := []string{
		"maul-egress-proxy-config", "maul-intent-controller-config", "maul-remediation-controller-config",
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	if len(names) != len(want) {
		t.Fatalf("%s is in %v, want exactly %v", netprofile.EnvProfiles, names, want)
	}
	first := got[want[0]]
	for _, n := range want {
		if got[n] != first {
			t.Errorf("%s in %s differs from %s's:\n%s\n%s", netprofile.EnvProfiles, n, want[0], got[n], first)
		}
	}
	set, err := netprofile.Parse([]byte(first))
	if err != nil {
		t.Fatalf("netprofile.Parse refuses the chart's profiles: %v\n%s", err, first)
	}
	goProfile, ok := set.Get("go")
	if !ok || set.Len() != 2 {
		t.Fatalf("parsed %d profiles, want go and npm", set.Len())
	}
	if goProfile.Host != netprofile.DefaultHost || goProfile.BakedModCache != "/opt/go/pkg/mod" ||
		goProfile.AllowPseudoVersions {
		t.Errorf("go profile defaults = host %q, bakedModCache %q, allowPseudoVersions %v; want %q, /opt/go/pkg/mod, false",
			goProfile.Host, goProfile.BakedModCache, goProfile.AllowPseudoVersions, netprofile.DefaultHost)
	}
	if !reflect.DeepEqual([]string(goProfile.Modules), []string{"github.com/google/uuid", "golang.org/x"}) {
		t.Errorf("go profile modules = %v, want the trailing slash stripped", goProfile.Modules)
	}
	if p, reason := set.For("https://github.com/devthenet-labs/maul-target"); p.Name != "go" || reason != "" {
		t.Error("the go profile does not match devthenet-labs/maul-target")
	}
}

// egressRegistryArgs layers the registries fixture over the on-fixture.
var egressRegistryArgs = append(append([]string{}, egressOnArgs...),
	"-f", "../../hack/testdata/chart-render/egress-registries.yaml")

// TestChartRegistriesParse: what the chart renders for the registries
// fixture is what the binaries start with. The proxy's registries JSON
// passes netprofile.ParseRegistries in the context the proxy builds (the
// profiles' forge hosts plus MAUL_DENY_HOSTS, the always-denied ranges
// plus a Kubernetes Service IP), every grant agrees with its registry
// (netprofile.CheckGrants, the proxy's startup check), the profile set
// parses in all three ConfigMaps, and each grant's hosts clear a job
// controller's Forge check against the profiles' own forge hosts.
func TestChartRegistriesParse(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	data := renderConfigMaps(t, egressRegistryArgs)
	proxy := data["maul-egress-proxy-config"]
	set, err := netprofile.Parse([]byte(proxy[netprofile.EnvProfiles]))
	if err != nil {
		t.Fatalf("netprofile.Parse refuses the chart's profiles: %v", err)
	}
	for _, c := range []string{"maul-remediation-controller-config", "maul-intent-controller-config"} {
		if data[c][netprofile.EnvProfiles] != proxy[netprofile.EnvProfiles] {
			t.Errorf("%s's profiles differ from the proxy's", c)
		}
		if _, ok := data[c]["MAUL_EGRESS_REGISTRIES"]; ok {
			t.Errorf("%s carries the registries; only the proxy reads them", c)
		}
	}
	regs, err := netprofile.ParseRegistries([]byte(proxy["MAUL_EGRESS_REGISTRIES"]), proxyRegistryContext(set, proxy))
	if err != nil {
		t.Fatalf("netprofile.ParseRegistries refuses the chart's registries: %v", err)
	}
	if regs.Len() != 4 {
		t.Errorf("parsed %d registries, want 4", regs.Len())
	}
	if err := netprofile.CheckGrants(set, regs); err != nil {
		t.Errorf("the chart's grants disagree with its registries: %v", err)
	}
	web, ok := set.Get("web")
	if !ok || len(web.Registries) != 3 || web.Route != "" {
		t.Fatalf("web profile = %+v, want three grants and no route", web)
	}
	for _, p := range set.Profiles() {
		for _, g := range p.Registries {
			if err := g.DeniedHosts(netprofile.DenyContext{Hosts: set.Hosts()}); err != nil {
				t.Errorf("profile %s: a job controller's Forge check refuses grant %s: %v", p.Name, g.Name, err)
			}
		}
	}
	if g, _ := web.GrantFor(netprofile.EcosystemGo); !reflect.DeepEqual(g.PrivateModules, []string{"acme.example"}) {
		t.Errorf("web's go grant privateModules = %v, want [acme.example]", g.PrivateModules)
	}
	if p, reason := set.For("https://github.com/acme/web"); p.Name != "web" || reason != "" {
		t.Errorf("acme/web matched profile %q (%s), want web", p.Name, reason)
	}
}

// renderConfigMaps renders the chart with args and returns every
// ConfigMap's data by name.
func renderConfigMaps(t *testing.T, args []string) map[string]map[string]string {
	t.Helper()
	all := append([]string{"template", "maul", ".", "--namespace", "maul"}, args...)
	output, err := exec.CommandContext(t.Context(), "helm", all...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	data := map[string]map[string]string{}
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() == "ConfigMap" {
			d, _, _ := unstructured.NestedStringMap(u.Object, "data")
			data[u.GetName()] = d
		}
	}
	return data
}

// proxyRegistryContext is the context the egress proxy validates its
// registries in: the profiles' forge hosts plus MAUL_DENY_HOSTS, and
// MAUL_ALWAYS_DENY_CIDRS plus a Kubernetes Service IP.
func proxyRegistryContext(set netprofile.Set, proxy map[string]string) netprofile.RegistryContext {
	cidrs := strings.Split(proxy["MAUL_ALWAYS_DENY_CIDRS"], ",")
	always := make([]netip.Prefix, 0, len(cidrs)+1)
	always = append(always, netip.MustParsePrefix("10.100.0.1/32"))
	for _, c := range cidrs {
		always = append(always, netip.MustParsePrefix(c))
	}
	deny := set.Hosts()
	if h := proxy["MAUL_DENY_HOSTS"]; h != "" {
		deny = append(deny, strings.Split(h, ",")...)
	}
	return netprofile.RegistryContext{DenyHosts: deny, AlwaysDeny: always}
}

// egressTunnelArgs renders the tunnel fixture: the on-fixture's two profiles
// on the same ports, go with tunnel hosts beside its Go route, npm a
// tunnel-only profile without a modules key.
var egressTunnelArgs = []string{
	"-f", "../../hack/testdata/chart-render/repository-images.yaml",
	"-f", "../../hack/testdata/chart-render/intent-controller.yaml",
	"-f", "../../hack/testdata/chart-render/egress-tunnel.yaml",
}

// TestChartTunnelProfilesParse: the profile set the chart renders for the
// tunnel fixture is one netprofile.Parse accepts in all three ConfigMaps,
// carrying the hosts, clients and upload caps as written; no client preset
// warns (each lists its usual hosts); and every host clears a job
// controller's Forge check against the profiles' own forge hosts.
func TestChartTunnelProfilesParse(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	data := renderConfigMaps(t, egressTunnelArgs)
	proxy := data["maul-egress-proxy-config"][netprofile.EnvProfiles]
	for _, c := range []string{"maul-remediation-controller-config", "maul-intent-controller-config"} {
		if data[c][netprofile.EnvProfiles] != proxy {
			t.Errorf("%s's profiles differ from the proxy's", c)
		}
		for k := range data[c] {
			if strings.HasPrefix(k, "MAUL_TUNNEL_") {
				t.Errorf("%s carries %s; only the proxy reads the tunnel limits", c, k)
			}
		}
	}
	set, err := netprofile.Parse([]byte(proxy))
	if err != nil {
		t.Fatalf("netprofile.Parse refuses the chart's tunnel profiles: %v\n%s", err, proxy)
	}
	checkTunnelProfiles(t, set)
}

// checkTunnelProfiles holds the tunnel fixture's parsed profiles to what
// the fixture says, and to the startup warnings and Forge check.
func checkTunnelProfiles(t *testing.T, set netprofile.Set) {
	t.Helper()
	goProfile, _ := set.Get("go")
	npm, _ := set.Get("npm")
	if !goProfile.HasGoRoute() || !goProfile.HasTunnel() || npm.HasGoRoute() || !npm.HasTunnel() {
		t.Fatalf("routes: go %v/%v, npm %v/%v; want go both, npm the tunnel alone",
			goProfile.HasGoRoute(), goProfile.HasTunnel(), npm.HasGoRoute(), npm.HasTunnel())
	}
	if !reflect.DeepEqual([]string(goProfile.Hosts), []string{"pypi.org", "files.pythonhosted.org"}) ||
		!reflect.DeepEqual(goProfile.Clients, []string{netprofile.ClientPip, netprofile.ClientUV}) {
		t.Errorf("go hosts %v, clients %v", goProfile.Hosts, goProfile.Clients)
	}
	if u := goProfile.Upload; u == nil || u.PerTunnel != 512<<10 || u.PerSource != 16<<20 {
		t.Errorf("go upload = %+v, want 512 KiB a tunnel and 16 MiB a source", u)
	}
	if npm.Upload != nil || len(npm.Modules) != 0 || npm.BakedModCache != "" || npm.AllowPseudoVersions {
		t.Errorf("npm = %+v, want no upload and none of the go route's fields", npm)
	}
	for _, p := range set.Profiles() {
		if w := p.ClientWarnings(); len(w) != 0 {
			t.Errorf("profile %s warns: %v", p.Name, w)
		}
		for _, h := range p.Hosts {
			if err := netprofile.DeniedTunnelHost(h, netprofile.DenyContext{Hosts: set.Hosts()}); err != nil {
				t.Errorf("profile %s: a job controller's Forge check refuses host %s: %v", p.Name, h, err)
			}
		}
	}
}
