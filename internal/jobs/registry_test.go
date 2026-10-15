// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The registry grants the fixtures hand a Job, as a controller copies them
// out of the operator's profile set.
func npmGrant() netprofile.Grant {
	return netprofile.Grant{Name: "ca-npm", Ecosystem: netprofile.EcosystemNPM,
		Hosts:         []string{"patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com"},
		PackagePolicy: netprofile.PackagePolicyRegistry}
}

func pipGrant() netprofile.Grant {
	return netprofile.Grant{Name: "ca-pypi", Ecosystem: netprofile.EcosystemPyPI,
		Hosts:    []string{"patchy-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com"},
		Packages: []string{"six", "requests", "urllib3", "idna", "certifi", "charset-normalizer"}}
}

func goGrant() netprofile.Grant {
	return netprofile.Grant{Name: "art-go", Ecosystem: netprofile.EcosystemGo,
		Hosts: []string{"acme.jfrog.io"}, Packages: []string{"acme.example/"},
		PrivateModules: []string{"acme.example/", "corp.example/x/"}}
}

// webProfile is a registry-only profile granting npm and pypi.
func webProfile() *NetworkProfile {
	return &NetworkProfile{Name: "web", ProxyURL: "http://" + proxyHost + ":8091",
		Registries: []netprofile.Grant{npmGrant(), pipGrant()}}
}

// goRegProfile is the go profile with a go registry grant beside its
// public route.
func goRegProfile() *NetworkProfile {
	p := goProfile()
	p.Registries = []netprofile.Grant{goGrant()}
	return p
}

// The §5.2 and §5.3 environments for webProfile, verbatim and in order.
var (
	wantNPMEnv = []corev1.EnvVar{
		{Name: "NPM_CONFIG_REGISTRY", Value: "http://" + proxyHost + ":8091/r/ca-npm/"},
		{Name: "NPM_CONFIG_REPLACE_REGISTRY_HOST", Value: "always"},
		{Name: "NPM_CONFIG_AUDIT", Value: "false"},
		{Name: "NPM_CONFIG_FUND", Value: "false"},
		{Name: "NPM_CONFIG_UPDATE_NOTIFIER", Value: "false"},
		{Name: "NPM_CONFIG_IGNORE_SCRIPTS", Value: "true"},
		{Name: "NPM_CONFIG_CACHE", Value: "/tmp/npm-cache"},
		{Name: "NPM_CONFIG_STRICT_SSL", Value: "true"},
	}
	wantPipEnv = []corev1.EnvVar{
		{Name: "PIP_CONFIG_FILE", Value: "/dev/null"},
		{Name: "PIP_INDEX_URL", Value: "http://" + proxyHost + ":8091/r/ca-pypi/simple/"},
		{Name: "PIP_EXTRA_INDEX_URL", Value: " "},
		{Name: "PIP_FIND_LINKS", Value: " "},
		{Name: "PIP_TRUSTED_HOST", Value: proxyHost + ":8091"},
		{Name: "PIP_ONLY_BINARY", Value: ":all:"},
		{Name: "PIP_NO_INPUT", Value: "1"},
		{Name: "PIP_DISABLE_PIP_VERSION_CHECK", Value: "1"},
		{Name: "PIP_CACHE_DIR", Value: "/tmp/pip-cache"},
	}
)

// npmNames and pipNames are the names each ecosystem's grant owns.
var (
	npmNames = envNames(wantNPMEnv)
	pipNames = envNames(wantPipEnv)
)

func envNames(env []corev1.EnvVar) []string {
	out := make([]string, len(env))
	for i, e := range env {
		out[i] = e.Name
	}
	return out
}

// goNames are the go names a profile owns, its name aside.
func goNames() []string {
	return slices.DeleteFunc(slices.Clone(goProfileNames), func(n string) bool { return n == netprofile.EnvProfile })
}

// TestGoldenRegistryProfileJobs pins the two registry-profile Jobs whole: a
// build Job granted npm and pypi, and a remediate Job with the public go
// route and a go registry. As with step 1's goldens, clearing the label and
// the profile's env gives back the profile-less Job byte for byte.
func TestGoldenRegistryProfileJobs(t *testing.T) {
	build := buildProfileSpec()
	build.NetworkProfile = webProfile()
	rem := remediateProfileSpec()
	rem.NetworkProfile = goRegProfile()
	for _, tt := range []struct {
		golden string
		spec   Spec
	}{
		{"job_build_network_profile_npm_pip", build},
		{"job_remediate_network_profile_goreg", rem},
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
			ct := &job.Spec.Template.Spec.Containers[0]
			ct.Env = ct.Env[:len(ct.Env)-len(profileEnv(tt.spec.NetworkProfile))]
			if got, w := marshal(t, job), marshal(t, want); got != w {
				t.Errorf("the profile changed more than its label and env:\n--- want\n%s\n--- got\n%s", w, got)
			}
		})
	}
}

// TestRegistryEnvOwnsItsNames: whatever set an npm, pip or go name before
// (Config.Env, the runner's Env, an image ENV the Job overrides), a
// registry-profile Job carries the profile's value for it exactly once, no
// value empty; pip's list variables are a single space; GOPROXY is the
// registry, then the public route, then the baked cache, never direct or
// off; GONOSUMDB is the grant's private prefixes; and the proxy variables
// stay blanked.
func TestRegistryEnvOwnsItsNames(t *testing.T) {
	cfg := profileConfig()
	for _, n := range []string{"NPM_CONFIG_REGISTRY", "NPM_CONFIG_IGNORE_SCRIPTS", "PIP_INDEX_URL",
		"PIP_EXTRA_INDEX_URL", "PIP_CONFIG_FILE", "GOPROXY", "GONOSUMDB"} {
		cfg.Env[n] = ""
	}
	cfg.Env["PIP_EXTRA_INDEX_URL"] = "https://evil.invalid/simple/"
	cfg.Env["NPM_CONFIG_REGISTRY"] = "https://evil.invalid/"
	claude := cfg.Runners["claude"]
	claude.Env = maps.Clone(claude.Env)
	claude.Env["PIP_FIND_LINKS"] = "https://evil.invalid/"
	claude.Env["NPM_CONFIG_STRICT_SSL"] = "false"
	cfg.Runners["claude"] = claude

	web := buildProfileSpec()
	web.NetworkProfile = webProfile()
	goreg := remediateProfileSpec()
	goreg.NetworkProfile = goRegProfile()
	for _, spec := range []Spec{web, goreg} {
		checkOwnsOnce(t, agentOf(t, buildJobForTest(t, cfg, spec).Spec.Template.Spec), spec.NetworkProfile)
	}
	checkWebEnv(t, cfg, web)
	checkGoRegEnv(t, cfg, goreg)
}

// checkOwnsOnce: every name the profile owns is on ct once, with the
// profile's non-empty value, and the proxy variables stay blanked.
func checkOwnsOnce(t *testing.T, ct corev1.Container, p *NetworkProfile) {
	t.Helper()
	count := map[string]int{}
	for _, e := range ct.Env {
		count[e.Name]++
	}
	env := envMap(ct)
	for _, want := range profileEnv(p) {
		if count[want.Name] != 1 {
			t.Errorf("%s: %s listed %d times, want once", p.Name, want.Name, count[want.Name])
		}
		if got := env[want.Name]; got.Value != want.Value || got.ValueFrom != nil || got.Value == "" {
			t.Errorf("%s: %s = %+v, want %q", p.Name, want.Name, got, want.Value)
		}
	}
	for _, name := range proxyEnv {
		if e, ok := env[name]; !ok || e.Value != "" {
			t.Errorf("%s = %+v, want blanked", name, e)
		}
	}
}

// checkWebEnv: the npm and pip environments verbatim, and no go name owned.
func checkWebEnv(t *testing.T, cfg Config, web Spec) {
	t.Helper()
	env := envMap(agentOf(t, buildJobForTest(t, cfg, web).Spec.Template.Spec))
	for _, want := range append(slices.Clone(wantNPMEnv), wantPipEnv...) {
		if got := env[want.Name].Value; got != want.Value {
			t.Errorf("web: %s = %q, want %q", want.Name, got, want.Value)
		}
	}
	for _, name := range []string{"PIP_EXTRA_INDEX_URL", "PIP_FIND_LINKS"} {
		if env[name].Value != " " {
			t.Errorf("%s = %q, want a single space", name, env[name].Value)
		}
	}
	// A registry-only profile without a go grant owns no go name: an
	// operator's GOPROXY stays as it was.
	if got := env["GOPROXY"].Value; got != "" {
		t.Errorf("web: GOPROXY = %q, want the operator's (blank) value left alone", got)
	}
	if got := env[netprofile.EnvEcosystems].Value; got != "npm,pypi" {
		t.Errorf("web: %s = %q, want npm,pypi", netprofile.EnvEcosystems, got)
	}
}

// checkGoRegEnv: GOPROXY's order, GONOSUMDB, and npm's and pip's names
// left alone without their grants.
func checkGoRegEnv(t *testing.T, cfg Config, goreg Spec) {
	t.Helper()
	env := envMap(agentOf(t, buildJobForTest(t, cfg, goreg).Spec.Template.Spec))
	wantProxy := []string{"http://" + proxyHost + ":8090/r/art-go", "http://" + proxyHost + ":8090/go",
		"file:///opt/go/pkg/mod/cache/download"}
	if got := strings.Split(env["GOPROXY"].Value, ","); !slices.Equal(got, wantProxy) {
		t.Errorf("goreg: GOPROXY = %q, want %q", got, wantProxy)
	}
	if got := env["GONOSUMDB"].Value; got != "acme.example/,corp.example/x/" {
		t.Errorf("goreg: GONOSUMDB = %q, want the grant's private prefixes", got)
	}
	for _, name := range []string{"GOPRIVATE", "GONOPROXY"} {
		if got := env[name].Value; got != patternNone {
			t.Errorf("goreg: %s = %q, want %q (a real pattern means direct)", name, got, patternNone)
		}
	}
	// Without their grants, npm's and pip's names are not the profile's:
	// whatever the operator set stays as it is on the profile-less Job.
	plain := goreg
	plain.NetworkProfile = nil
	base := envMap(agentOf(t, buildJobForTest(t, cfg, plain).Spec.Template.Spec))
	for _, name := range append(slices.Clone(npmNames), pipNames...) {
		if env[name] != base[name] {
			t.Errorf("goreg: %s = %+v without a grant of its ecosystem, want %+v", name, env[name], base[name])
		}
	}
	if got := env[netprofile.EnvEcosystems].Value; got != "go" {
		t.Errorf("goreg: %s = %q, want go", netprofile.EnvEcosystems, got)
	}
}

// TestRegistryGrantSwitches: scripts and sdists turn their defaults off, a
// go registry without private modules keeps GONOSUMDB unmatched, and a
// registry-only go profile lists the registry alone before the cache.
func TestRegistryGrantSwitches(t *testing.T) {
	p := webProfile()
	p.Registries[0].NPMScripts = true
	p.Registries[1].PipSdists = true
	env := map[string]string{}
	for _, e := range profileEnv(p) {
		env[e.Name] = e.Value
	}
	if env["NPM_CONFIG_IGNORE_SCRIPTS"] != "false" || env["PIP_ONLY_BINARY"] != ":none:" {
		t.Errorf("scripts and sdists on: ignore-scripts %q, only-binary %q", env["NPM_CONFIG_IGNORE_SCRIPTS"],
			env["PIP_ONLY_BINARY"])
	}

	g := goGrant()
	g.PrivateModules = nil
	only := &NetworkProfile{Name: "goreg", ProxyURL: "http://10.0.0.9:8092", BakedModCache: "/opt/go/pkg/mod",
		Registries: []netprofile.Grant{g}}
	env = map[string]string{}
	for _, e := range profileEnv(only) {
		env[e.Name] = e.Value
	}
	if want := "http://10.0.0.9:8092/r/art-go,file:///opt/go/pkg/mod/cache/download"; env["GOPROXY"] != want {
		t.Errorf("GOPROXY = %q, want %q", env["GOPROXY"], want)
	}
	if env["GONOSUMDB"] != patternNone {
		t.Errorf("GONOSUMDB = %q, want %q", env["GONOSUMDB"], patternNone)
	}
	if env[netprofile.EnvEcosystems] != "go" {
		t.Errorf("%s = %q, want go", netprofile.EnvEcosystems, env[netprofile.EnvEcosystems])
	}
}

// TestRegistryGrantsReachTheAgent: the grants variable decodes to what the
// agent is told about each grant, in ecosystem order, and agrees with the
// ecosystems variable.
func TestRegistryGrantsReachTheAgent(t *testing.T) {
	p := webProfile()
	p.Route = netprofile.RouteGo
	p.Registries = []netprofile.Grant{pipGrant(), goGrant(), npmGrant()} // any order in, fixed order out
	env := map[string]string{}
	for _, e := range profileEnv(p) {
		env[e.Name] = e.Value
	}
	grants, err := netprofile.DecodePodGrants(env[netprofile.EnvGrants])
	if err != nil {
		t.Fatal(err)
	}
	eco, err := netprofile.ParseEcosystems(env[netprofile.EnvEcosystems])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(eco, netprofile.Ecosystems) {
		t.Errorf("ecosystems = %v, want %v", eco, netprofile.Ecosystems)
	}
	if err := netprofile.CheckPodRoutes(eco, grants); err != nil {
		t.Error(err)
	}
	want := []netprofile.PodGrant{netprofile.PodGrantOf(goGrant()), netprofile.PodGrantOf(npmGrant()),
		netprofile.PodGrantOf(pipGrant())}
	if fmt.Sprint(grants) != fmt.Sprint(want) {
		t.Errorf("grants = %+v, want %+v", grants, want)
	}
	for _, v := range env {
		if strings.Contains(v, "codeartifact") || strings.Contains(v, "jfrog") {
			t.Errorf("a registry's host reached the pod: %q", v)
		}
	}
}

// TestRegistryProfileRefused: a profile with no route and no grant, two
// grants of one ecosystem, or a grant that does not hold together is
// refused before anything is created.
func TestRegistryProfileRefused(t *testing.T) {
	tests := []struct {
		name string
		edit func(*NetworkProfile)
	}{
		{"no route and no grant", func(p *NetworkProfile) { p.Registries = nil }},
		{"two npm grants", func(p *NetworkProfile) {
			g := npmGrant()
			g.Name = "art-npm"
			p.Registries = append(p.Registries, g)
		}},
		{"bad registry name", func(p *NetworkProfile) { p.Registries[0].Name = "Ca-npm" }},
		{"unknown ecosystem", func(p *NetworkProfile) { p.Registries[0].Ecosystem = "maven" }},
		{"no package policy", func(p *NetworkProfile) { p.Registries[0].PackagePolicy = "" }},
		{"private modules on npm", func(p *NetworkProfile) { p.Registries[0].PrivateModules = []string{"a.example/"} }},
		{"bad private module", func(p *NetworkProfile) {
			g := goGrant()
			g.PrivateModules = []string{"acme.example/,direct"}
			p.Registries = append(p.Registries, g)
		}},
		{"sdists on npm", func(p *NetworkProfile) { p.Registries[0].PipSdists = true }},
		{"route npm", func(p *NetworkProfile) { p.Route = "npm" }},
		{"bad package", func(p *NetworkProfile) { p.Registries[1].Packages = []string{"six\nrequests"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := buildProfileSpec()
			spec.NetworkProfile = webProfile()
			tt.edit(spec.NetworkProfile)
			if _, _, err := New(fake.NewClientset(), profileConfig(), nil).Create(context.Background(),
				spec); !errors.Is(err, ErrNetworkProfileRefused) {
				t.Fatalf("Create = %v, want ErrNetworkProfileRefused", err)
			}
		})
	}
}

// TestRegistryEnvNamesNotReserved: npm's and pip's names are owned only on
// a profile Job, as go's are, so an image's ENV may set them; the
// ecosystems and grants variables are per-Job names, reserved.
func TestRegistryEnvNamesNotReserved(t *testing.T) {
	reserved := ReservedEnvNames()
	for _, name := range append(slices.Clone(npmNames), pipNames...) {
		if slices.Contains(reserved, name) || slices.Contains(scrubEnv, name) {
			t.Errorf("%s is reserved or scrubbed; an image's ENV may set it", name)
		}
	}
	for _, name := range []string{netprofile.EnvEcosystems, netprofile.EnvGrants} {
		if !slices.Contains(reserved, name) || !slices.Contains(PerJobEnvNames(), name) {
			t.Errorf("%s is not a reserved per-Job name", name)
		}
	}
}

// TestPropertyRegistryEnvMatchesGrants: for generated grant sets (any
// subset of go, npm and pypi, with or without the public route, each
// switch and policy drawn), a profile Job carries an ecosystem's names iff
// it is granted (go's also with the public route), each once and
// non-empty, the ecosystems variable lists exactly what is present, and the
// proxy variables stay blanked. Seeded so the gate is deterministic.
func TestPropertyRegistryEnvMatchesGrants(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	coin := func() bool { return rng.Intn(2) == 0 }
	for i := range 500 {
		p := &NetworkProfile{Name: "p", ProxyURL: "http://" + proxyHost + ":8091"}
		if coin() {
			p.Route = netprofile.RouteGo
			if coin() {
				p.BakedModCache = "/opt/go/pkg/mod"
			}
		}
		for _, g := range []netprofile.Grant{goGrant(), npmGrant(), pipGrant()} {
			if !coin() {
				continue
			}
			if coin() {
				g.Packages, g.PackagePolicy = nil, netprofile.PackagePolicyRegistry
			} else if g.PackagePolicy != "" {
				g.Packages, g.PackagePolicy = []string{"left-pad", "@acme/*"}, ""
			}
			switch g.Ecosystem {
			case netprofile.EcosystemNPM:
				g.NPMScripts, g.AllowPrerelease = coin(), coin()
			case netprofile.EcosystemPyPI:
				g.PipSdists = coin()
			case netprofile.EcosystemGo:
				g.AllowPseudoVersions = coin()
				if coin() {
					g.PrivateModules = nil
				}
			}
			p.Registries = append(p.Registries, g)
		}
		if p.Route == "" && len(p.Registries) == 0 {
			continue
		}
		rng.Shuffle(len(p.Registries), func(a, b int) { p.Registries[a], p.Registries[b] = p.Registries[b], p.Registries[a] })
		spec := buildProfileSpec()
		spec.NetworkProfile = p
		job, err := New(fake.NewClientset(), profileConfig(), nil).build(context.Background(),
			NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
		if err != nil {
			t.Fatalf("iteration %d: %+v: %v", i, p, err)
		}
		if msg := checkRegistryEnv(agentOf(t, job.Spec.Template.Spec), p); msg != "" {
			t.Fatalf("iteration %d: %+v: %s", i, p, msg)
		}
	}
}

// checkRegistryEnv judges a registry-profile agent's env against the
// profile, returning what is wrong.
func checkRegistryEnv(agent corev1.Container, p *NetworkProfile) string {
	count := map[string]int{}
	for _, e := range agent.Env {
		count[e.Name]++
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return "a SecretKeyRef on " + e.Name
		}
	}
	env := envMap(agent)
	present := presentEcosystems(p)
	if msg := checkEcosystemNames(count, env, present); msg != "" {
		return msg
	}
	if msg := checkRegistryVars(count, env, p, present); msg != "" {
		return msg
	}
	if present[netprofile.EcosystemGo] {
		for entry := range strings.SplitSeq(env["GOPROXY"].Value, ",") {
			if entry == "direct" || entry == "off" || entry == "" {
				return "GOPROXY holds " + entry
			}
		}
	}
	for _, name := range proxyEnv {
		if e, ok := env[name]; !ok || e.Value != "" {
			return name + " is not blanked"
		}
	}
	return ""
}

// presentEcosystems is the oracle: an ecosystem is present iff granted, go
// also with the public route.
func presentEcosystems(p *NetworkProfile) map[string]bool {
	present := map[string]bool{netprofile.EcosystemGo: p.Route == netprofile.RouteGo}
	for _, g := range p.Registries {
		present[g.Ecosystem] = true
	}
	return present
}

// checkEcosystemNames: each present ecosystem's names once and non-empty,
// an absent one's not at all.
func checkEcosystemNames(count map[string]int, env map[string]corev1.EnvVar, present map[string]bool) string {
	for eco, names := range map[string][]string{
		netprofile.EcosystemGo: goNames(), netprofile.EcosystemNPM: npmNames, netprofile.EcosystemPyPI: pipNames,
	} {
		for _, n := range names {
			if want := map[bool]int{true: 1}[present[eco]]; count[n] != want {
				return fmt.Sprintf("%s listed %d times, want %d", n, count[n], want)
			}
			if present[eco] && env[n].Value == "" {
				return n + " is empty"
			}
		}
	}
	return ""
}

// checkRegistryVars: the profile's name once, and the ecosystems and
// grants variables exactly on a profile with grants.
func checkRegistryVars(count map[string]int, env map[string]corev1.EnvVar, p *NetworkProfile,
	present map[string]bool) string {
	if count[netprofile.EnvProfile] != 1 || env[netprofile.EnvProfile].Value != p.Name {
		return "the profile's name is not listed once"
	}
	if len(p.Registries) == 0 {
		if count[netprofile.EnvEcosystems] != 0 || count[netprofile.EnvGrants] != 0 {
			return "a step-1 profile carries the registry variables"
		}
		return ""
	}
	var listed []string
	for _, eco := range netprofile.Ecosystems {
		if present[eco] {
			listed = append(listed, eco)
		}
	}
	if got := env[netprofile.EnvEcosystems].Value; got != strings.Join(listed, ",") {
		return fmt.Sprintf("%s = %q, want %q", netprofile.EnvEcosystems, got, strings.Join(listed, ","))
	}
	if _, err := netprofile.DecodePodGrants(env[netprofile.EnvGrants].Value); err != nil {
		return err.Error()
	}
	return ""
}
