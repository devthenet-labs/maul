// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/web/auth"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// The intents views come up only under an auth posture that can serve them:
// oidc with both prefixes, or mode none behind the explicit development flag
// on a loopback address. Every other posture refuses to start.
func TestIntentsAuthGate(t *testing.T) {
	oidc := func(cl auth.ClaimsConfig) *auth.Config {
		return &auth.Config{Mode: auth.ModeOIDC, OIDC: &auth.OIDCConfig{Claims: cl}}
	}
	dex := auth.ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:"}
	cases := []struct {
		name     string
		cfg      *auth.Config
		dev      bool
		listen   string
		wantErr  string
		wantFull bool
	}{
		{"unconfigured", nil, false, ":8080", "none is configured", false},
		{"mode none", &auth.Config{Mode: auth.ModeNone}, false, "127.0.0.1:8080", "refuses auth mode none", false},
		{"mode none with the dev flag on every interface", &auth.Config{Mode: auth.ModeNone}, true, ":8080",
			"loopback", false},
		{"mode none with the dev flag on a public address", &auth.Config{Mode: auth.ModeNone}, true,
			"10.0.0.5:8080", "loopback", false},
		{"mode none with the dev flag on loopback", &auth.Config{Mode: auth.ModeNone}, true, "127.0.0.1:8080", "", true},
		{"mode none with the dev flag on localhost", &auth.Config{Mode: auth.ModeNone}, true, "localhost:8080", "", true},
		{"mode anonymous", &auth.Config{Mode: auth.ModeAnonymous}, true, "127.0.0.1:8080", "anonymous", false},
		{"oidc without prefixes", oidc(auth.ClaimsConfig{Username: "preferred_username"}), false, ":8080",
			"usernamePrefix", false},
		{"oidc with prefixes", oidc(dex), false, ":8080", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			granter, err := intentsAuthGate(tc.cfg, tc.dev, tc.listen)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("intentsAuthGate = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("intentsAuthGate: %v", err)
			}
			_, full := granter.(authz.FullProjects)
			if full != tc.wantFull {
				t.Errorf("granter = %T, want FullProjects=%v (oidc gets the access-review granter)", granter, tc.wantFull)
			}
		})
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "127.1.2.3:80": true, "[::1]:8080": true, "localhost:8080": true,
		":8080": false, "0.0.0.0:8080": false, "10.0.0.5:8080": false, "[::]:8080": false, "bogus": false,
	} {
		if got := loopback(addr); got != want {
			t.Errorf("loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

// The usage views come up only under oidc with both prefixes and Dex's
// federated:id scope; off, any posture starts.
func TestUsageAuthGate(t *testing.T) {
	dex := auth.ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:",
		GitHubConnector: "github"}
	oidc := func(scopes ...string) *auth.Config {
		return &auth.Config{Mode: auth.ModeOIDC, OIDC: &auth.OIDCConfig{Claims: dex, Scopes: scopes}}
	}
	cases := []struct {
		name    string
		on      bool
		cfg     *auth.Config
		wantErr string
	}{
		{"off and unconfigured", false, nil, ""},
		{"off in mode none", false, &auth.Config{Mode: auth.ModeNone}, ""},
		{"unconfigured", true, nil, "none is configured"},
		{"mode none", true, &auth.Config{Mode: auth.ModeNone}, `not "none"`},
		{"mode anonymous", true, &auth.Config{Mode: auth.ModeAnonymous}, `not "anonymous"`},
		{"oidc without federated:id", true, oidc("openid", "profile"), "federated:id"},
		{"dex with federated:id", true, oidc("openid", "federated:id"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uw, err := newUsageWiring(tc.on, tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("newUsageWiring = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("newUsageWiring = %v, want an error containing %q", err, tc.wantErr)
			case tc.wantErr == "" && uw.on != tc.on:
				t.Errorf("on = %v, want %v", uw.on, tc.on)
			}
		})
	}
}

// TestAccountProviders: --account-providers names one or more of the
// providers a credential may be stored for, each once, in the page's order.
func TestAccountProviders(t *testing.T) {
	dex := auth.ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:",
		GitHubConnector: "github"}
	cfg := &auth.Config{Mode: auth.ModeOIDC, OIDC: &auth.OIDCConfig{Claims: dex, Scopes: []string{"federated:id"}}}
	for _, tc := range []struct {
		in      []string
		want    []v1alpha1.ModelCredentialProvider
		wantErr string
	}{
		{[]string{"anthropic"}, []v1alpha1.ModelCredentialProvider{"anthropic"}, ""},
		{[]string{"openai"}, []v1alpha1.ModelCredentialProvider{"openai"}, ""},
		{[]string{"anthropic", "openai"}, []v1alpha1.ModelCredentialProvider{"anthropic", "openai"}, ""},
		{nil, nil, "at least one"},
		{[]string{"bedrock"}, nil, `"bedrock" is not one of`},
		{[]string{"openai", "openai"}, nil, "listed twice"},
	} {
		aw, err := newAccountWiring(true, "maul-credentials", tc.in, cfg)
		switch {
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%v: err = %v, want %q", tc.in, err, tc.wantErr)
		case tc.wantErr == "" && (err != nil || !slices.Equal(aw.providers, tc.want)):
			t.Errorf("%v: providers = %v, %v, want %v", tc.in, aw.providers, err, tc.want)
		}
	}
	if aw, err := newAccountWiring(false, "maul-credentials", []string{"bedrock"}, cfg); err != nil || aw.on {
		t.Errorf("off with a bad provider list = %+v, %v; want off and no error", aw, err)
	}
}

// The Account page comes up only under the usage views' posture (oidc,
// both prefixes, federated:id) and with a valid credentials namespace; off,
// any posture starts.
func TestAccountAuthGate(t *testing.T) {
	dex := auth.ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:",
		GitHubConnector: "github"}
	oidc := func(scopes ...string) *auth.Config {
		return &auth.Config{Mode: auth.ModeOIDC, OIDC: &auth.OIDCConfig{Claims: dex, Scopes: scopes}}
	}
	cases := []struct {
		name    string
		on      bool
		ns      string
		cfg     *auth.Config
		wantErr string
	}{
		{"off and unconfigured", false, "maul-credentials", nil, ""},
		{"mode none", true, "maul-credentials", &auth.Config{Mode: auth.ModeNone}, `not "none"`},
		{"oidc without federated:id", true, "maul-credentials", oidc("openid"), "federated:id"},
		{"a namespace outside the grammar", true, "Maul_Creds", oidc("openid", "federated:id"),
			"--credentials-namespace"},
		{"dex with federated:id", true, "maul-credentials", oidc("openid", "federated:id"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aw, err := newAccountWiring(tc.on, tc.ns, []string{"anthropic"}, tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("newAccountWiring = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("newAccountWiring = %v, want an error containing %q", err, tc.wantErr)
			case tc.wantErr == "" && aw.on != tc.on:
				t.Errorf("on = %v, want %v", aw.on, tc.on)
			}
		})
	}
}
