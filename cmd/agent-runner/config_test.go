// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/agentrun"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// TestConfigReadsNetworkProfile: the profile the Job carries reaches the
// stage configuration through the injected getenv, and only through config:
// agentrun.FromEnv neither reads nor lists it, so a repository-image Job's
// blanks of agentrun's keys never grow.
func TestConfigReadsNetworkProfile(t *testing.T) {
	env := map[string]string{
		"PATCHY_REPO": "acme/api", "PATCHY_FINDING": "finding-1", "PATCHY_PHASE": "remediate",
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
		"PATCHY_REPO": "acme/api", "PATCHY_FINDING": "finding-1", "PATCHY_PHASE": "remediate",
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
