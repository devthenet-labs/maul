// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestConfigReadsNetworkProfile: the profile the Job carries reaches the
// stage configuration through the injected getenv, and only through config:
// agentrun.FromEnv neither reads nor lists it, so a repository-image Job's
// blanks of agentrun's keys never grow.
func TestConfigReadsNetworkProfile(t *testing.T) {
	env := map[string]string{
		"MAUL_REPO": "acme/api", "MAUL_FINDING": "finding-1", "MAUL_PHASE": "remediate",
		netprofile.EnvProfile: "go",
	}
	getenv := func(k string) string { return env[k] }
	cfg, err := config(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkProfile != "go" {
		t.Errorf("NetworkProfile = %q, want go", cfg.NetworkProfile)
	}
	plain, err := agentrun.FromEnv(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if plain.NetworkProfile != "" {
		t.Errorf("FromEnv read the profile (%q); cmd/agent-runner owns it", plain.NetworkProfile)
	}
	if slices.Contains(agentrun.ConfigEnvKeys(), netprofile.EnvProfile) {
		t.Errorf("ConfigEnvKeys lists %s", netprofile.EnvProfile)
	}

	delete(env, netprofile.EnvProfile)
	if cfg, err = config(getenv); err != nil || cfg.NetworkProfile != "" {
		t.Errorf("without the variable: NetworkProfile = %q, %v; want empty", cfg.NetworkProfile, err)
	}
}

// TestConfigReadsNetworkRoutes: a profile with registry grants reaches the
// stage configuration with its ecosystems and grants, again only through
// config; values the controller could not have written are a configuration
// error, never a prompt.
func TestConfigReadsNetworkRoutes(t *testing.T) {
	npm := netprofile.PodGrant{Registry: "ca-npm", Ecosystem: netprofile.EcosystemNPM, AnyPackage: true}
	grants, err := netprofile.EncodePodGrants([]netprofile.PodGrant{npm})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"MAUL_REPO": "acme/api", "MAUL_FINDING": "finding-1", "MAUL_PHASE": "remediate",
		netprofile.EnvProfile: "web", netprofile.EnvEcosystems: "go,npm", netprofile.EnvGrants: grants,
	}
	getenv := func(k string) string { return env[k] }
	cfg, err := config(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.NetworkRoutes; !r.PublicGo || len(r.Grants) != 1 || !reflect.DeepEqual(r.Grants[0], npm) {
		t.Errorf("NetworkRoutes = %+v, want the public go route and the npm grant", cfg.NetworkRoutes)
	}
	for _, k := range []string{netprofile.EnvEcosystems, netprofile.EnvGrants} {
		if slices.Contains(agentrun.ConfigEnvKeys(), k) {
			t.Errorf("ConfigEnvKeys lists %s", k)
		}
	}

	for name, edit := range map[string]func(map[string]string){
		"unknown ecosystem":       func(e map[string]string) { e[netprofile.EnvEcosystems] = "go,maven" },
		"grant not listed":        func(e map[string]string) { e[netprofile.EnvEcosystems] = "go" },
		"ecosystem without grant": func(e map[string]string) { e[netprofile.EnvEcosystems] = "npm,pypi" },
		"grants not JSON":         func(e map[string]string) { e[netprofile.EnvGrants] = "npm" },
		"grants without profile":  func(e map[string]string) { delete(e, netprofile.EnvProfile) },
		"ecosystems alone":        func(e map[string]string) { delete(e, netprofile.EnvGrants) },
	} {
		bad := maps.Clone(env)
		edit(bad)
		if _, err := config(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("%s: config accepted it", name)
		}
	}
}

// TestConfigReadsTunnel: a profile with tunnel hosts reaches the stage
// configuration with its hosts, and the Go route only where the routes
// list it, so a tunnel-only Job's prompt never describes a Go route (step
// 3); again only through config, and values the controller could not have
// written are a configuration error.
func TestConfigReadsTunnel(t *testing.T) {
	npm := netprofile.PodGrant{Registry: "ca-npm", Ecosystem: netprofile.EcosystemNPM, AnyPackage: true}
	grants, err := netprofile.EncodePodGrants([]netprofile.PodGrant{npm})
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"MAUL_REPO": "acme/api", "MAUL_FINDING": "finding-1", "MAUL_PHASE": "remediate"}
	with := func(kv ...string) map[string]string {
		env := maps.Clone(base)
		for i := 0; i < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return env
	}
	hosts := []string{"files.pythonhosted.org", "pypi.org"}
	for _, tt := range []struct {
		name string
		env  map[string]string
		want templates.NetworkRoutes
	}{
		{"tunnel only", with(netprofile.EnvProfile, "py", netprofile.EnvRoutes, "tunnel",
			netprofile.EnvHosts, "files.pythonhosted.org,pypi.org"),
			templates.NetworkRoutes{TunnelHosts: hosts}},
		{"go and tunnel", with(netprofile.EnvProfile, "go", netprofile.EnvRoutes, "go,tunnel",
			netprofile.EnvHosts, "files.pythonhosted.org,pypi.org"),
			templates.NetworkRoutes{PublicGo: true, TunnelHosts: hosts}},
		{"go, grants and tunnel", with(netprofile.EnvProfile, "web", netprofile.EnvEcosystems, "go,npm",
			netprofile.EnvGrants, grants, netprofile.EnvRoutes, "go,tunnel", netprofile.EnvHosts, "pypi.org"),
			templates.NetworkRoutes{PublicGo: true, Grants: []netprofile.PodGrant{npm},
				TunnelHosts: []string{"pypi.org"}}},
		{"grants and tunnel, no go", with(netprofile.EnvProfile, "web", netprofile.EnvEcosystems, "npm",
			netprofile.EnvGrants, grants, netprofile.EnvRoutes, "tunnel", netprofile.EnvHosts, "pypi.org"),
			templates.NetworkRoutes{Grants: []netprofile.PodGrant{npm}, TunnelHosts: []string{"pypi.org"}}},
		{"go alone (step 1)", with(netprofile.EnvProfile, "go"), templates.NetworkRoutes{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config(func(k string) string { return tt.env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.NetworkRoutes, tt.want) {
				t.Errorf("NetworkRoutes = %+v, want %+v", cfg.NetworkRoutes, tt.want)
			}
		})
	}
	for _, k := range []string{netprofile.EnvRoutes, netprofile.EnvHosts} {
		if slices.Contains(agentrun.ConfigEnvKeys(), k) {
			t.Errorf("ConfigEnvKeys lists %s", k)
		}
	}

	for name, env := range map[string]map[string]string{
		"hosts without profile": with(netprofile.EnvRoutes, "tunnel", netprofile.EnvHosts, "pypi.org"),
		"routes alone":          with(netprofile.EnvProfile, "py", netprofile.EnvRoutes, "tunnel"),
		"hosts alone":           with(netprofile.EnvProfile, "py", netprofile.EnvHosts, "pypi.org"),
		"unknown route": with(netprofile.EnvProfile, "py", netprofile.EnvRoutes, "npm",
			netprofile.EnvHosts, "pypi.org"),
		"bad host": with(netprofile.EnvProfile, "py", netprofile.EnvRoutes, "tunnel",
			netprofile.EnvHosts, "pypi.org,`evil`"),
		"go route the ecosystems lack": with(netprofile.EnvProfile, "web", netprofile.EnvEcosystems, "npm",
			netprofile.EnvGrants, grants, netprofile.EnvRoutes, "go,tunnel", netprofile.EnvHosts, "pypi.org"),
		"go ecosystem the routes lack": with(netprofile.EnvProfile, "web", netprofile.EnvEcosystems, "go,npm",
			netprofile.EnvGrants, grants, netprofile.EnvRoutes, "tunnel", netprofile.EnvHosts, "pypi.org"),
	} {
		if _, err := config(func(k string) string { return env[k] }); err == nil {
			t.Errorf("%s: config accepted it", name)
		}
	}
}
