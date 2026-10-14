// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
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
