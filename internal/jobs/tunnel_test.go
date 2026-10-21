// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"net/url"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/provider"
)

// The fixtures' broker and artifact URLs, as brokeredConfig and testSpec
// carry them.
const (
	fixtureBrokerURL   = "http://" + brokerHost + ":8080/anthropic"
	fixtureArtifactURL = "http://" + artifactHost + ":9790/artifacts/deadbeef.tar.gz"
)

// wantNoProxy is NO_PROXY for the fixtures: the artifact server, the
// broker and the proxy, then loopback and the cluster suffixes, sorted.
var wantNoProxy = ".cluster.local,.svc,127.0.0.1,::1,localhost," + proxyHost + "," + brokerHost + "," + artifactHost

func init() {
	// Sorted as TunnelEnv sorts it.
	wantNoProxy = strings.Join(sortedUnique(strings.Split(wantNoProxy, ",")), ",")
}

// tunnelProfile is a tunnel-only profile for pip, as a controller hands it
// to a Job.
func tunnelProfile() *NetworkProfile {
	return &NetworkProfile{Name: "py", Route: netprofile.RouteTunnel, ProxyURL: "http://" + proxyHost + ":8092",
		Hosts: []string{"pypi.org", "files.pythonhosted.org"}, Clients: []string{netprofile.ClientPip}}
}

// goTunnelProfile is the go profile with an npm tunnel beside its route.
func goTunnelProfile() *NetworkProfile {
	p := goProfile()
	p.Hosts = []string{"registry.npmjs.org"}
	p.Clients = []string{netprofile.ClientNPM}
	return p
}

// jvmTunnelProfile is a tunnel-only profile for Maven Central.
func jvmTunnelProfile() *NetworkProfile {
	return &NetworkProfile{Name: "jvm", Route: netprofile.RouteTunnel, ProxyURL: "http://" + proxyHost + ":8093",
		Hosts: []string{"repo.maven.apache.org"}, Clients: []string{netprofile.ClientJVM}}
}

// TestGoldenTunnelProfileJobs pins the three tunnel Jobs whole (§4.5): a
// tunnel-only build Job without a resolver (the hosts file pins the
// broker, the artifact server and the proxy), a go route with an npm
// tunnel, and a tunnel-only remediate Job with the jvm preset. Clearing
// the label and the profile's env gives back the profile-less Job.
func TestGoldenTunnelProfileJobs(t *testing.T) {
	build := buildProfileSpec()
	build.NetworkProfile = tunnelProfile()
	combined := buildProfileSpec()
	combined.NetworkProfile = goTunnelProfile()
	rem := remediateProfileSpec()
	rem.NetworkProfile = jvmTunnelProfile()

	r := clusterResolver()
	r.answers[proxyHost] = []string{"10.100.50.9"}
	cfg := profileConfig()
	cfg.DNS, cfg.Resolver = DNSNone, r
	job, err := New(fake.NewClientset(), cfg, nil).build(context.Background(),
		NameFor(build.Finding, build.Kind, int32(build.Attempt)), build)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	goldenJob(t, "job_build_network_tunnel", job)
	pinned := map[string]bool{}
	for _, ha := range job.Spec.Template.Spec.HostAliases {
		for _, h := range ha.Hostnames {
			pinned[h] = true
		}
	}
	for _, h := range []string{brokerHost, artifactHost, proxyHost} {
		if !pinned[h] {
			t.Errorf("hostAliases %+v do not pin %s", job.Spec.Template.Spec.HostAliases, h)
		}
	}

	for _, tt := range []struct {
		golden string
		spec   Spec
	}{
		{"job_build_network_go_tunnel_npm", combined},
		{"job_remediate_network_tunnel_jvm", rem},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			job := buildJobForTest(t, profileConfig(), tt.spec)
			goldenJob(t, tt.golden, job)

			plain := tt.spec
			plain.NetworkProfile = nil
			want := buildJobForTest(t, profileConfig(), plain)
			key := netprofile.LabelKey(tt.spec.NetworkProfile.Name)
			delete(job.Labels, key)
			delete(job.Spec.Template.Labels, key)
			tunnel, err := TunnelEnv(*tt.spec.NetworkProfile, []string{fixtureArtifactURL,
				tt.spec.NetworkProfile.ProxyURL, fixtureBrokerURL})
			if err != nil {
				t.Fatal(err)
			}
			ct := &job.Spec.Template.Spec.Containers[0]
			ct.Env = ct.Env[:len(ct.Env)-len(profileEnv(tt.spec.NetworkProfile))-len(tunnel)]
			// The proxy names a tunnel Job owns were injectEnv's blanks on
			// the profile-less Job; nothing else moves.
			wct := &want.Spec.Template.Spec.Containers[0]
			wct.Env = slices.DeleteFunc(wct.Env, func(e corev1.EnvVar) bool {
				return slices.Contains(tunnelBlockNames, e.Name)
			})
			if got, w := marshal(t, job), marshal(t, want); got != w {
				t.Errorf("the profile changed more than its label and env:\n--- want\n%s\n--- got\n%s", w, got)
			}
		})
	}
}

// TestTunnelEnvVerbatim pins §4.2's tunnel block for a combined profile,
// in order: the https proxy and NO_PROXY in both cases, the routes and the
// sorted hosts, then the npm preset.
func TestTunnelEnvVerbatim(t *testing.T) {
	p := *goTunnelProfile()
	p.Hosts = []string{"registry.npmjs.org", "registry.yarnpkg.com"}
	proxy := "http://" + proxyHost + ":8090"
	got, err := TunnelEnv(p, []string{fixtureArtifactURL, proxy, fixtureBrokerURL})
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.EnvVar{
		{Name: "HTTPS_PROXY", Value: proxy},
		{Name: "https_proxy", Value: proxy},
		{Name: "NO_PROXY", Value: wantNoProxy},
		{Name: "no_proxy", Value: wantNoProxy},
		{Name: "MAUL_NETWORK_ROUTES", Value: "go,tunnel"},
		{Name: "MAUL_NETWORK_HOSTS", Value: "registry.npmjs.org,registry.yarnpkg.com"},
		{Name: "npm_config_https_proxy", Value: proxy},
		{Name: "npm_config_noproxy", Value: wantNoProxy},
		{Name: "npm_config_update_notifier", Value: "false"},
		{Name: "npm_config_fund", Value: "false"},
		{Name: "npm_config_audit", Value: "false"},
		{Name: "pnpm_config_https_proxy", Value: proxy},
		{Name: "pnpm_config_noproxy", Value: wantNoProxy},
		{Name: "pnpm_config_update_notifier", Value: "false"},
		{Name: "COREPACK_ENABLE_DOWNLOAD_PROMPT", Value: "0"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("TunnelEnv =\n%v\nwant\n%v", got, want)
	}
	if !strings.HasPrefix(wantNoProxy, ".cluster.local,.svc,127.0.0.1,::1,localhost,") {
		t.Errorf("NO_PROXY = %q, want the fixed entries sorted in", wantNoProxy)
	}

	tunnelOnly := *tunnelProfile()
	got, err = TunnelEnv(tunnelOnly, []string{fixtureArtifactURL})
	if err != nil {
		t.Fatal(err)
	}
	if e := envMapOf(got); e[netprofile.EnvRoutes].Value != "tunnel" ||
		e[netprofile.EnvHosts].Value != "files.pythonhosted.org,pypi.org" {
		t.Errorf("tunnel-only routes/hosts = %q/%q", e[netprofile.EnvRoutes].Value, e[netprofile.EnvHosts].Value)
	}

	if env, err := TunnelEnv(*goProfile(), []string{fixtureArtifactURL}); env != nil || err != nil {
		t.Errorf("TunnelEnv without hosts = %v, %v; want nothing", env, err)
	}
}

func envMapOf(env []corev1.EnvVar) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}

// presetNames are the names each client preset owns (§4.2's table).
var presetNames = map[string][]string{
	netprofile.ClientNPM: {"npm_config_https_proxy", "npm_config_noproxy", "npm_config_update_notifier",
		"npm_config_fund", "npm_config_audit", "pnpm_config_https_proxy", "pnpm_config_noproxy",
		"pnpm_config_update_notifier", "COREPACK_ENABLE_DOWNLOAD_PROMPT"},
	netprofile.ClientYarn: {"YARN_HTTPS_PROXY", "YARN_ENABLE_TELEMETRY"},
	netprofile.ClientPip:  {"PIP_PROXY", "PIP_DISABLE_PIP_VERSION_CHECK", "PIP_NO_INPUT"},
	netprofile.ClientUV:   {"UV_PYTHON_DOWNLOADS"},
	netprofile.ClientNuGet: {"NUGET_CERT_REVOCATION_MODE", "DOTNET_CLI_TELEMETRY_OPTOUT", "DOTNET_NOLOGO",
		"DOTNET_CLI_WORKLOAD_UPDATE_NOTIFY_DISABLE"},
	netprofile.ClientCargo: {"CARGO_HTTP_PROXY", "CARGO_REGISTRIES_CRATES_IO_PROTOCOL"},
	netprofile.ClientJVM:   {"JAVA_TOOL_OPTIONS"},
}

// tunnelBlockNames are the names every tunnel Job owns, presets aside.
var tunnelBlockNames = []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
	netprofile.EnvRoutes, netprofile.EnvHosts}

// TestClientPresets: each client sets its preset's names and only those,
// with the values §4.2 gives; the proxy-carrying names carry the profile's
// URL, and NO_PROXY's twins its value.
func TestClientPresets(t *testing.T) {
	if got := slices.Sorted(maps.Keys(presetNames)); !slices.Equal(got, slices.Sorted(slices.Values(netprofile.Clients))) {
		t.Fatalf("presetNames covers %v, want every client %v", got, netprofile.Clients)
	}
	proxy := "http://" + proxyHost + ":8092"
	values := map[string]string{
		"npm_config_https_proxy": proxy, "npm_config_noproxy": wantNoProxy, "npm_config_update_notifier": "false",
		"npm_config_fund": "false", "npm_config_audit": "false", "pnpm_config_https_proxy": proxy,
		"pnpm_config_noproxy": wantNoProxy, "pnpm_config_update_notifier": "false",
		"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
		"YARN_HTTPS_PROXY":                proxy, "YARN_ENABLE_TELEMETRY": "0",
		"PIP_PROXY": proxy, "PIP_DISABLE_PIP_VERSION_CHECK": "1", "PIP_NO_INPUT": "1",
		"UV_PYTHON_DOWNLOADS":        "never",
		"NUGET_CERT_REVOCATION_MODE": "offline", "DOTNET_CLI_TELEMETRY_OPTOUT": "1", "DOTNET_NOLOGO": "1",
		"DOTNET_CLI_WORKLOAD_UPDATE_NOTIFY_DISABLE": "1",
		"CARGO_HTTP_PROXY":                          proxy, "CARGO_REGISTRIES_CRATES_IO_PROTOCOL": "sparse",
		"JAVA_TOOL_OPTIONS": "-Dhttps.proxyHost=" + proxyHost + " -Dhttps.proxyPort=8092 -Dhttp.nonProxyHosts=" +
			jvmWant(),
	}
	for _, c := range netprofile.Clients {
		t.Run(c, func(t *testing.T) {
			p := *tunnelProfile()
			p.Clients = []string{c}
			env, err := TunnelEnv(p, []string{fixtureArtifactURL, p.ProxyURL, fixtureBrokerURL})
			if err != nil {
				t.Fatal(err)
			}
			got := envNames(env[len(tunnelBlockNames):])
			if !slices.Equal(envNames(env[:len(tunnelBlockNames)]), tunnelBlockNames) {
				t.Errorf("tunnel block = %v, want %v", envNames(env[:len(tunnelBlockNames)]), tunnelBlockNames)
			}
			if !slices.Equal(got, presetNames[c]) {
				t.Errorf("preset %s names = %v, want %v", c, got, presetNames[c])
			}
			for _, e := range env[len(tunnelBlockNames):] {
				if e.Value != values[e.Name] {
					t.Errorf("%s = %q, want %q", e.Name, e.Value, values[e.Name])
				}
			}
		})
	}
}

// jvmWant is the JVM's nonProxyHosts for the fixtures, written out.
func jvmWant() string {
	parts := strings.Split(wantNoProxy, ",")
	for i, p := range parts {
		if strings.HasPrefix(p, ".") {
			parts[i] = "*" + p
		}
	}
	return strings.Join(parts, "|")
}

// TestJVMNonProxyHosts: NO_PROXY's leading-dot suffixes become "*."
// wildcards and the separator "|".
func TestJVMNonProxyHosts(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{".svc", "*.svc"},
		{"localhost", "localhost"},
		{".cluster.local,.svc,127.0.0.1,::1,localhost", "*.cluster.local|*.svc|127.0.0.1|::1|localhost"},
		{"10.0.0.5,broker.maul.svc", "10.0.0.5|broker.maul.svc"},
	} {
		if got := jvmNonProxyHosts(tt.in); got != tt.want {
			t.Errorf("jvmNonProxyHosts(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNoProxyKeepsIPLiterals pins the example §8.5 names: a broker given by
// address is in NO_PROXY (endpointHosts skips it on purpose; noProxyHosts
// must not), IPv6 without brackets; a URL without a host is an error.
func TestNoProxyKeepsIPLiterals(t *testing.T) {
	got, err := noProxyHosts([]string{"http://10.0.0.5:8080", "http://[fd00::5]:8080/x", "http://Broker.Example:1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.5", "fd00::5", "broker.example"}; !slices.Equal(got, want) {
		t.Errorf("noProxyHosts = %v, want %v", got, want)
	}
	if _, err := noProxyHosts([]string{"file:///x"}); err == nil {
		t.Error("noProxyHosts accepted a URL with no host")
	}
	p := *tunnelProfile()
	env, err := TunnelEnv(p, []string{"http://10.0.0.5:8080/anthropic", p.ProxyURL})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(envMapOf(env)["NO_PROXY"].Value, ","), "10.0.0.5") {
		t.Errorf("NO_PROXY = %q, want 10.0.0.5 in it", envMapOf(env)["NO_PROXY"].Value)
	}
}

// TestTunnelEnvRefuses: a bad proxy URL, an endpoint that is a tunnel host,
// an endpoint with no host, an unknown client or a host outside the
// grammar is an error, never an environment.
func TestTunnelEnvRefuses(t *testing.T) {
	for name, tt := range map[string]struct {
		edit      func(*NetworkProfile)
		endpoints []string
	}{
		"https proxy":         {func(p *NetworkProfile) { p.ProxyURL = "https://" + proxyHost + ":8092" }, nil},
		"endpoint is tunnel":  {func(*NetworkProfile) {}, []string{"https://pypi.org/simple"}},
		"endpoint no host":    {func(*NetworkProfile) {}, []string{"/relative"}},
		"unknown client":      {func(p *NetworkProfile) { p.Clients = []string{"gem"} }, nil},
		"host with wildcard":  {func(p *NetworkProfile) { p.Hosts = []string{"*.pypi.org"} }, nil},
		"host with uppercase": {func(p *NetworkProfile) { p.Hosts = []string{"PyPI.org"} }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			p := *tunnelProfile()
			tt.edit(&p)
			if env, err := TunnelEnv(p, append([]string{fixtureArtifactURL}, tt.endpoints...)); err == nil {
				t.Errorf("TunnelEnv = %v, want an error", env)
			}
		})
	}
}

// TestTunnelEnvOwnsItsNames: whatever set a tunnel or preset name before
// (Config.Env, the runner's Env, an image-style injectEnv blank), the
// tunnel Job carries the profile's value exactly once and non-empty; the
// plain-http and ALL_PROXY names stay blank; on a tunnel-only profile the
// go block is absent, so an operator's GOPROXY passes through untouched.
func TestTunnelEnvOwnsItsNames(t *testing.T) {
	cfg := profileConfig()
	cfg.Env["GOPROXY"] = "off"
	cfg.Env["PIP_PROXY"] = "http://evil:3128"
	claude := cfg.Runners["claude"]
	claude.Env = maps.Clone(claude.Env)
	claude.Env["JAVA_TOOL_OPTIONS"] = "-javaagent:/evil.jar"
	cfg.Runners["claude"] = claude
	for _, p := range []*NetworkProfile{tunnelProfile(), goTunnelProfile(), jvmTunnelProfile()} {
		spec := buildProfileSpec()
		spec.NetworkProfile = p
		ct := agentOf(t, buildJobForTest(t, cfg, spec).Spec.Template.Spec)
		count := map[string]int{}
		for _, e := range ct.Env {
			count[e.Name]++
		}
		for name, n := range count {
			if n != 1 {
				t.Errorf("%s: %s listed %d times", p.Name, name, n)
			}
		}
		env := envMap(ct)
		owned := slices.Clone(tunnelBlockNames)
		for _, c := range p.Clients {
			owned = append(owned, presetNames[c]...)
		}
		for _, name := range owned {
			if e := env[name]; e.Value == "" || e.ValueFrom != nil {
				t.Errorf("%s: %s = %+v, want a value", p.Name, name, e)
			}
		}
		if got := env["HTTPS_PROXY"].Value; got != p.ProxyURL {
			t.Errorf("%s: HTTPS_PROXY = %q, want %q", p.Name, got, p.ProxyURL)
		}
		for _, name := range []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
			if e, ok := env[name]; !ok || e.Value != "" {
				t.Errorf("%s: %s = %+v, want blanked", p.Name, name, e)
			}
		}
		if p.Route == netprofile.RouteTunnel {
			if got := env["GOPROXY"].Value; got != "off" {
				t.Errorf("%s: GOPROXY = %q, want the operator's off untouched", p.Name, got)
			}
			for _, name := range goNames() {
				if name != "GOPROXY" && count[name] != 0 {
					t.Errorf("%s: tunnel-only profile set %s", p.Name, name)
				}
			}
		} else if !strings.HasPrefix(env["GOPROXY"].Value, p.ProxyURL+"/go") {
			t.Errorf("%s: GOPROXY = %q, want the Go route", p.Name, env["GOPROXY"].Value)
		}
		if env[netprofile.EnvProfile].Value != p.Name {
			t.Errorf("%s: %s = %q", p.Name, netprofile.EnvProfile, env[netprofile.EnvProfile].Value)
		}
	}
}

// TestTunnelProxyEnvOnlyFromProfile: an operator's HTTPS_PROXY never
// reaches a tunnel Job (it is reserved, and the profile owns the name); the
// controller's value appears once. A runner's Env naming it is refused at
// startup as before (validateRunnerEnv's reservedEnv check).
func TestTunnelProxyEnvOnlyFromProfile(t *testing.T) {
	cfg := profileConfig()
	cfg.Env["HTTPS_PROXY"] = "http://evil:3128"
	cfg.Env["no_proxy"] = "*"
	spec := buildProfileSpec()
	spec.NetworkProfile = tunnelProfile()
	ct := agentOf(t, buildJobForTest(t, cfg, spec).Spec.Template.Spec)
	var values []string
	for _, e := range ct.Env {
		if e.Name == "HTTPS_PROXY" {
			values = append(values, e.Value)
		}
		if e.Name == "no_proxy" && e.Value == "*" {
			t.Error("the operator's no_proxy reached the tunnel Job")
		}
	}
	if !slices.Equal(values, []string{spec.NetworkProfile.ProxyURL}) {
		t.Errorf("HTTPS_PROXY values = %v, want the profile's once", values)
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		if !reservedEnv[name] || !slices.Contains(scrubEnv, name) {
			t.Errorf("%s is no longer reserved and scrubbed", name)
		}
	}
}

// TestClientPresetNamesNotReserved: no preset name is reserved or
// scrubbed (images legitimately set npm_config_*, JAVA_TOOL_OPTIONS, …);
// the routes and hosts names are per-Job names.
func TestClientPresetNamesNotReserved(t *testing.T) {
	for c, names := range presetNames {
		for _, name := range names {
			if reservedEnv[name] || slices.Contains(scrubEnv, name) {
				t.Errorf("client %s: %s is reserved or scrubbed", c, name)
			}
		}
	}
	for _, name := range []string{netprofile.EnvRoutes, netprofile.EnvHosts} {
		if !perJobEnv[name] || !reservedEnv[name] {
			t.Errorf("%s is not a reserved per-Job name", name)
		}
	}
	for _, e := range InjectedEnv(repoSearchPath) {
		if e.Name == netprofile.EnvRoutes || e.Name == netprofile.EnvHosts {
			t.Errorf("InjectedEnv sets %s", e.Name)
		}
	}
}

// TestCombinedProfileEnvCoherent: a go route with go and pypi grants, tunnel
// hosts and the cargo and jvm presets gives one coherent environment: the
// grants' GOPROXY and pip index untouched by the tunnel, every name once,
// the proxy host in NO_PROXY (so the grants' http routes never go through
// a CONNECT), and the tunnel block after the grant block.
func TestCombinedProfileEnvCoherent(t *testing.T) {
	p := goRegProfile()
	p.Registries = append(p.Registries, pipGrant())
	p.Hosts = []string{"index.crates.io", "static.crates.io", "repo.maven.apache.org"}
	p.Clients = []string{netprofile.ClientJVM, netprofile.ClientCargo}
	spec := remediateProfileSpec()
	spec.NetworkProfile = p
	ct := agentOf(t, buildJobForTest(t, profileConfig(), spec).Spec.Template.Spec)
	env := envMap(ct)
	count := map[string]int{}
	for _, e := range ct.Env {
		count[e.Name]++
	}
	for name, n := range count {
		if n != 1 {
			t.Errorf("%s listed %d times", name, n)
		}
	}
	if got := env["GOPROXY"].Value; !strings.HasPrefix(got, p.ProxyURL+"/r/art-go,"+p.ProxyURL+"/go") {
		t.Errorf("GOPROXY = %q, want the grant then the route", got)
	}
	if got := env["PIP_INDEX_URL"].Value; got != p.ProxyURL+"/r/ca-pypi/simple/" {
		t.Errorf("PIP_INDEX_URL = %q, want the grant's route", got)
	}
	if !slices.Contains(strings.Split(env["NO_PROXY"].Value, ","), proxyHost) {
		t.Errorf("NO_PROXY = %q lacks the proxy host", env["NO_PROXY"].Value)
	}
	if env[netprofile.EnvEcosystems].Value == "" || env[netprofile.EnvGrants].Value == "" {
		t.Error("the grant names are missing beside the tunnel")
	}
	if env[netprofile.EnvRoutes].Value != "go,tunnel" {
		t.Errorf("routes = %q", env[netprofile.EnvRoutes].Value)
	}
	idx := func(name string) int {
		return slices.IndexFunc(ct.Env, func(e corev1.EnvVar) bool { return e.Name == name })
	}
	if idx(netprofile.EnvGrants) >= idx("HTTPS_PROXY") || idx("HTTPS_PROXY") >= idx("CARGO_HTTP_PROXY") ||
		idx("CARGO_HTTP_PROXY") >= idx("JAVA_TOOL_OPTIONS") {
		t.Error("the env is not grants, then tunnel, then presets in enum order")
	}
}

// TestTunnelProfileRefused: §4.1's profileRefusal rules for tunnels, each
// a refusal before anything is created; and the defensive rule the task
// adds: a preset never sits beside a grant of the same package manager,
// so it can never override the grant's index.
func TestTunnelProfileRefused(t *testing.T) {
	tests := map[string]func(*NetworkProfile){
		"tunnel without hosts":    func(p *NetworkProfile) { p.Hosts = nil; p.Clients = nil },
		"tunnel with baked cache": func(p *NetworkProfile) { p.BakedModCache = "/opt/go/pkg/mod" },
		"hosts without route": func(p *NetworkProfile) {
			p.Route = ""
			p.Registries = []netprofile.Grant{npmGrant()}
		},
		"wildcard host":   func(p *NetworkProfile) { p.Hosts = []string{"*.pythonhosted.org"} },
		"ip host":         func(p *NetworkProfile) { p.Hosts = []string{"1.2.3.4"} },
		"trailing dot":    func(p *NetworkProfile) { p.Hosts = []string{"pypi.org."} },
		"uppercase host":  func(p *NetworkProfile) { p.Hosts = []string{"PyPI.org"} },
		"denied host":     func(p *NetworkProfile) { p.Hosts = []string{"objects.githubusercontent.com"} },
		"model host":      func(p *NetworkProfile) { p.Hosts = []string{"api.anthropic.com"} },
		"cluster host":    func(p *NetworkProfile) { p.Hosts = []string{"kubernetes.default.svc"} },
		"duplicate host":  func(p *NetworkProfile) { p.Hosts = []string{"pypi.org", "pypi.org"} },
		"too many hosts":  func(p *NetworkProfile) { p.Hosts = manyHosts(netprofile.MaxTunnelHosts + 1) },
		"unknown client":  func(p *NetworkProfile) { p.Clients = []string{"gem"} },
		"duplicate":       func(p *NetworkProfile) { p.Clients = []string{"pip", "pip"} },
		"clients no host": func(p *NetworkProfile) { p.Hosts = nil; p.Route = netprofile.RouteGo },
		"go upstream beside go route": func(p *NetworkProfile) {
			p.Route = netprofile.RouteGo
			p.Hosts = []string{"proxy.golang.org"}
			p.Clients = nil
		},
		"grant host": func(p *NetworkProfile) {
			p.Registries = []netprofile.Grant{npmGrant()}
			p.Hosts = []string{npmGrant().Hosts[0]}
			p.Clients = nil
		},
		"pip preset beside pypi grant": func(p *NetworkProfile) {
			p.Registries = []netprofile.Grant{pipGrant()}
		},
		"npm preset beside npm grant": func(p *NetworkProfile) {
			p.Registries = []netprofile.Grant{npmGrant()}
			p.Clients = []string{netprofile.ClientNPM}
			p.Hosts = []string{"registry.npmjs.org"}
		},
		"yarn preset beside npm grant": func(p *NetworkProfile) {
			p.Registries = []netprofile.Grant{npmGrant()}
			p.Clients = []string{netprofile.ClientYarn}
		},
		"uv preset beside pypi grant": func(p *NetworkProfile) {
			p.Registries = []netprofile.Grant{pipGrant()}
			p.Clients = []string{netprofile.ClientUV}
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			spec := buildProfileSpec()
			spec.NetworkProfile = tunnelProfile()
			edit(spec.NetworkProfile)
			if err := profileRefusal(spec); !errors.Is(err, ErrNetworkProfileRefused) {
				t.Fatalf("profileRefusal = %v, want ErrNetworkProfileRefused", err)
			}
			if _, _, err := New(fake.NewClientset(), profileConfig(), nil).Create(context.Background(),
				spec); !errors.Is(err, ErrNetworkProfileRefused) {
				t.Errorf("Create = %v, want ErrNetworkProfileRefused", err)
			}
		})
	}
	// Admitted: a tunnel-only profile may list the Go route's upstreams
	// (it has no allowlist to bypass), and a pip preset beside an npm
	// grant configures a different package manager.
	for name, edit := range map[string]func(*NetworkProfile){
		"go upstream on tunnel-only": func(p *NetworkProfile) { p.Hosts = []string{"proxy.golang.org"} },
		"pip beside npm grant":       func(p *NetworkProfile) { p.Registries = []netprofile.Grant{npmGrant()} },
		"64 hosts":                   func(p *NetworkProfile) { p.Hosts = manyHosts(netprofile.MaxTunnelHosts) },
	} {
		spec := buildProfileSpec()
		spec.NetworkProfile = tunnelProfile()
		edit(spec.NetworkProfile)
		if err := profileRefusal(spec); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func manyHosts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("h%d.example.org", i)
	}
	return out
}

// TestTunnelEndpointCollisionRefused: a Job whose endpoint host is also a
// tunnel host is refused, never built with that endpoint routed through
// the tunnel.
func TestTunnelEndpointCollisionRefused(t *testing.T) {
	spec := buildProfileSpec()
	spec.NetworkProfile = tunnelProfile()
	spec.ArtifactURL = "https://pypi.org/artifacts/x.tar.gz"
	if _, err := New(fake.NewClientset(), profileConfig(), nil).buildJob("x", spec); !errors.Is(err,
		ErrNetworkProfileRefused) {
		t.Errorf("buildJob = %v, want ErrNetworkProfileRefused", err)
	}
}

// TestTunnelEnvReadsBack: the routes and hosts TunnelEnv writes are what
// agent-runner reads (netprofile.ParsePodTunnel), so the prompt describes
// the routes the pod has: the Go route only beside it, the hosts sorted.
func TestTunnelEnvReadsBack(t *testing.T) {
	for _, p := range []*NetworkProfile{tunnelProfile(), goTunnelProfile()} {
		env, err := TunnelEnv(*p, []string{"http://maul-egress-broker.maul.svc.cluster.local:8080"})
		if err != nil {
			t.Fatal(err)
		}
		values := map[string]string{}
		for _, e := range env {
			values[e.Name] = e.Value
		}
		got, err := netprofile.ParsePodTunnel(values[netprofile.EnvRoutes], values[netprofile.EnvHosts])
		if err != nil {
			t.Fatalf("profile %s: %v", p.Name, err)
		}
		if got.GoRoute != (p.Route == netprofile.RouteGo) || !slices.Equal(got.Hosts, slices.Sorted(slices.Values(p.Hosts))) {
			t.Errorf("profile %s: read back %+v from %+v", p.Name, got, p)
		}
	}
}

// TestTunnelNeedsBrokeredRunner: a tunnel profile on a runner that is not
// brokered (one that would call its model API directly, so through the
// tunnel's HTTPS_PROXY, which refuses model hosts) is refused, naming the
// runner; the same runner still carries a go profile, and a brokered
// runner carries the tunnel.
func TestTunnelNeedsBrokeredRunner(t *testing.T) {
	direct := profileConfig()
	r := direct.Runners["claude"]
	r.Brokered, r.Secret = false, ""
	direct.Runners = map[string]Runner{"claude": r}
	spec := buildProfileSpec()
	spec.NetworkProfile = tunnelProfile()
	_, err := New(fake.NewClientset(), direct, nil).buildJob("x", spec)
	if !errors.Is(err, ErrNetworkProfileRefused) || !strings.Contains(err.Error(), `runner "claude" is not brokered`) {
		t.Errorf("tunnel on a direct runner: buildJob = %v, want a refusal naming the runner", err)
	}
	spec.NetworkProfile = goProfile()
	if _, err := New(fake.NewClientset(), direct, nil).buildJob("x", spec); err != nil {
		t.Errorf("go profile on a direct runner: %v", err)
	}
	spec.NetworkProfile = tunnelProfile()
	if _, err := New(fake.NewClientset(), profileConfig(), nil).buildJob("x", spec); err != nil {
		t.Errorf("tunnel on a brokered runner: %v", err)
	}
}

// TestPropertyNoProxyCoversEndpoints (§8.5): over brokered providers ×
// base URLs (names and IP literals, IPv4 and bracketed IPv6) × artifact
// URLs × proxy URLs × presets × DNS modes, a tunnel Job's NO_PROXY holds
// every endpoint URL's host as url.Parse reads it (the oracle never uses
// noProxyHosts), the proxy's host, and no tunnel host; and every name is
// set once. Seeded.
func TestPropertyNoProxyCoversEndpoints(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	for i := range 500 {
		tc := drawTunnelCase(rng)
		job, err := New(fake.NewClientset(), tc.cfg, nil).build(context.Background(), "x", tc.spec)
		if err != nil {
			t.Fatalf("iteration %d: build: %v", i, err)
		}
		if msg := tc.check(agentOf(t, job.Spec.Template.Spec)); msg != "" {
			t.Fatalf("iteration %d: %s", i, msg)
		}
	}
}

// tunnelCase is one draw of TestPropertyNoProxyCoversEndpoints: the Job's
// inputs and every URL its pod dials.
type tunnelCase struct {
	cfg       Config
	spec      Spec
	endpoints []string
}

// propertyTunnelHosts are the public hosts a tunnelCase draws from.
var propertyTunnelHosts = []string{"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "index.crates.io",
	"static.crates.io", "repo.maven.apache.org", "api.nuget.org"}

func drawTunnelCase(rng *rand.Rand) tunnelCase {
	hostOf := func() string {
		switch rng.Intn(4) {
		case 0:
			return fmt.Sprintf("10.%d.%d.%d", rng.Intn(256), rng.Intn(256), rng.Intn(256))
		case 1:
			return fmt.Sprintf("[fd00::%x]", rng.Intn(0xffff)+1)
		case 2:
			return fmt.Sprintf("svc-%d.maul.svc.cluster.local", rng.Intn(1000))
		default:
			return fmt.Sprintf("Broker-%d.Internal.Example", rng.Intn(1000))
		}
	}
	tc := tunnelCase{cfg: profileConfig(), spec: buildProfileSpec()}
	claude := tc.cfg.Runners["claude"]
	claude.Env = maps.Clone(claude.Env)
	for _, name := range provider.BaseURLEnvNames {
		delete(claude.Env, name)
		if name == "ANTHROPIC_BASE_URL" || rng.Intn(2) == 0 {
			u := fmt.Sprintf("http://%s:%d/%s", hostOf(), 1024+rng.Intn(60000), strings.ToLower(name))
			claude.Env[name] = u
			tc.endpoints = append(tc.endpoints, u)
		}
	}
	tc.cfg.Runners["claude"] = claude
	if rng.Intn(2) == 0 {
		tc.spec = remediateProfileSpec()
	}
	tc.spec.ArtifactURL = fmt.Sprintf("http://%s:9790/artifacts/x.tar.gz", hostOf())
	p := []func() *NetworkProfile{tunnelProfile, goTunnelProfile}[rng.Intn(2)]()
	p.ProxyURL = fmt.Sprintf("http://%s:%d", proxyHost, 8090+rng.Intn(8))
	if rng.Intn(2) == 0 {
		p.ProxyURL = fmt.Sprintf("http://10.100.50.%d:%d", rng.Intn(256), 8090+rng.Intn(8))
	}
	tc.endpoints = append(tc.endpoints, tc.spec.ArtifactURL, p.ProxyURL)
	hosts := slices.Clone(propertyTunnelHosts)
	rng.Shuffle(len(hosts), func(a, b int) { hosts[a], hosts[b] = hosts[b], hosts[a] })
	p.Hosts = hosts[:1+rng.Intn(len(hosts))]
	p.Clients = slices.DeleteFunc(slices.Clone(netprofile.Clients), func(string) bool { return rng.Intn(3) != 0 })
	tc.spec.NetworkProfile = p
	if rng.Intn(2) == 0 {
		// No resolver in the pod: every named endpoint resolves for the
		// controller and is pinned; NO_PROXY must not depend on the mode.
		r := &fakeResolver{answers: map[string][]string{}}
		for _, raw := range tc.endpoints {
			u, _ := url.Parse(raw)
			r.answers[strings.ToLower(u.Hostname())] = []string{"10.100.0.1"}
		}
		tc.cfg.DNS, tc.cfg.Resolver = DNSNone, r
	}
	return tc
}

// check judges the agent container against the oracle.
func (tc tunnelCase) check(ct corev1.Container) string {
	env := envMap(ct)
	noProxy := strings.Split(env["NO_PROXY"].Value, ",")
	if env["no_proxy"].Value != env["NO_PROXY"].Value {
		return fmt.Sprintf("no_proxy %q != NO_PROXY %q", env["no_proxy"].Value, env["NO_PROXY"].Value)
	}
	for _, raw := range tc.endpoints {
		u, err := url.Parse(raw)
		if err != nil {
			return err.Error()
		}
		if h := strings.ToLower(u.Hostname()); !slices.Contains(noProxy, h) {
			return fmt.Sprintf("NO_PROXY %v lacks endpoint host %s (%s)", noProxy, h, raw)
		}
	}
	for _, h := range tc.spec.NetworkProfile.Hosts {
		if slices.Contains(noProxy, h) {
			return "NO_PROXY holds tunnel host " + h
		}
	}
	count := map[string]int{}
	for _, e := range ct.Env {
		if count[e.Name]++; count[e.Name] > 1 {
			return e.Name + " listed twice"
		}
	}
	return ""
}
