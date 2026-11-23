// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/projectsettings"
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

// The Project settings page comes up only beside the intents views, under
// the usage views' posture (oidc, both prefixes, federated:id): a session
// is matched to an approver by its GitHub id. Under the intents views'
// development posture (mode none) it is served read-only, never with a
// write path. Its policy is parsed strictly and its Project list held to
// the name grammar; off, nothing is checked.
func TestProjectSettingsAuthGate(t *testing.T) {
	dex := auth.ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:",
		GitHubConnector: "github"}
	oidc := func(scopes ...string) *auth.Config {
		return &auth.Config{Mode: auth.ModeOIDC, OIDC: &auth.OIDCConfig{Claims: dex, Scopes: scopes}}
	}
	on := intentsWiring{on: true}
	cases := []struct {
		name         string
		enabled      bool
		intents      intentsWiring
		cfg          *auth.Config
		policy       string
		projects     []string
		wantErr      string
		wantReadOnly bool
	}{
		{name: "off", cfg: nil, policy: "{nonsense", projects: []string{"Bad_Name"}},
		{name: "without the intents views", enabled: true, cfg: oidc("openid", "federated:id"),
			wantErr: "--intents-enabled"},
		{name: "oidc without federated:id", enabled: true, intents: on, cfg: oidc("openid"),
			wantErr: "--project-settings-enabled: "},
		{name: "mode anonymous", enabled: true, intents: on, cfg: &auth.Config{Mode: auth.ModeAnonymous},
			wantErr: "--project-settings-enabled: "},
		{name: "unconfigured", enabled: true, intents: on, cfg: nil, wantErr: "--project-settings-enabled: "},
		{name: "mode none behind the development flag", enabled: true, intents: on,
			cfg: &auth.Config{Mode: auth.ModeNone}, wantReadOnly: true},
		{name: "dex with federated:id", enabled: true, intents: on, cfg: oidc("openid", "federated:id")},
		{name: "a policy with an unknown switch", enabled: true, intents: on, cfg: oidc("federated:id"),
			policy: `{"allow":{"everything":true}}`, wantErr: "--project-settings-policy: "},
		{name: "a policy past a field's range", enabled: true, intents: on, cfg: oidc("federated:id"),
			policy:  `{"ceilings":{"maxCostMicroUSD":1,"maxVerifications":1,"maxActiveIntents":1,"maxRevisions":99}}`,
			wantErr: "--project-settings-policy: "},
		{name: "a policy", enabled: true, intents: on, cfg: oidc("federated:id"),
			policy: `{"allow":{"approvers":true},"locked":["models"],"ceilings":{"maxCostMicroUSD":5000000,` +
				`"maxVerifications":10,"maxActiveIntents":2}}`},
		{name: "a project outside the name grammar", enabled: true, intents: on, cfg: oidc("federated:id"),
			projects: []string{"alpha", "Bad_Name"}, wantErr: "--project-settings-projects"},
		{name: "a project listed twice", enabled: true, intents: on, cfg: oidc("federated:id"),
			projects: []string{"alpha", "alpha"}, wantErr: "listed twice"},
		{name: "projects", enabled: true, intents: on, cfg: oidc("federated:id"), projects: []string{"alpha", "beta"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw, err := newSettingsWiring(tc.enabled, tc.intents, tc.cfg, tc.policy, tc.projects)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("newSettingsWiring = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newSettingsWiring: %v", err)
			}
			if sw.on != tc.enabled || sw.readOnly != tc.wantReadOnly {
				t.Errorf("on %v readOnly %v, want %v %v", sw.on, sw.readOnly, tc.enabled, tc.wantReadOnly)
			}
			if tc.enabled && !slices.Equal(sw.projects, tc.projects) {
				t.Errorf("projects = %v, want %v", sw.projects, tc.projects)
			}
		})
	}

	sw, err := newSettingsWiring(true, on, oidc("federated:id"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.policy, projectsettings.DefaultPolicy()) {
		t.Errorf("no policy = %+v, want the default (the chart's)", sw.policy)
	}
	sw, err = newSettingsWiring(true, on, oidc("federated:id"), `{"allow":{"approvers":true},"ceilings":`+
		`{"maxCostMicroUSD":5000000,"maxVerifications":10,"maxActiveIntents":2}}`, nil)
	if err != nil || !sw.policy.Allow.Approvers || sw.policy.Ceilings.MaxCostMicroUSD != 5_000_000 {
		t.Errorf("policy = %+v, %v", sw.policy, err)
	}
}
