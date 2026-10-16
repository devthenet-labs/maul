// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

// dexClaims are the claims Dex's GitHub connector issues with the
// federated:id scope.
func dexClaims(connector string, userID any) map[string]any {
	return map[string]any{
		"preferred_username": "octocat",
		"federated_claims":   map[string]any{"connector_id": connector, "user_id": userID},
	}
}

func TestMapClaimsGitHubID(t *testing.T) {
	cfg := ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GitHubConnector: "github"}
	cases := []struct {
		name   string
		claims map[string]any
		cfg    ClaimsConfig
		want   int64
	}{
		{"github connector", dexClaims("github", "583231"), cfg, 583231},
		{"largest int64", dexClaims("github", "9223372036854775807"), cfg, 9223372036854775807},
		{"another connector", dexClaims("ldap", "583231"), cfg, 0},
		{"a renamed github connector", dexClaims("gh", "583231"), ClaimsConfig{
			Username: "preferred_username", GitHubConnector: "gh"}, 583231},
		{"no connector configured", dexClaims("github", "583231"), ClaimsConfig{Username: "preferred_username"}, 0},
		{"no federated claims", map[string]any{"preferred_username": "octocat"}, cfg, 0},
		{"federated claims not an object", map[string]any{
			"preferred_username": "octocat", "federated_claims": "github:583231"}, cfg, 0},
		{"numeric user id is not a string", dexClaims("github", 583231.0), cfg, 0},
		{"login, not an id", dexClaims("github", "octocat"), cfg, 0},
		{"zero", dexClaims("github", "0"), cfg, 0},
		{"leading zero", dexClaims("github", "0583231"), cfg, 0},
		{"signed", dexClaims("github", "+583231"), cfg, 0},
		{"negative", dexClaims("github", "-583231"), cfg, 0},
		{"overflow", dexClaims("github", "9223372036854775808"), cfg, 0},
		{"too long", dexClaims("github", "12345678901234567890"), cfg, 0},
		{"empty", dexClaims("github", ""), cfg, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := MapClaims(tc.claims, tc.cfg)
			if err != nil {
				t.Fatalf("MapClaims: %v", err)
			}
			if id.GitHubID != tc.want {
				t.Errorf("GitHubID = %d, want %d", id.GitHubID, tc.want)
			}
		})
	}
}

// The GitHub id round-trips for every positive int64 under the GitHub
// connector, and no user_id string that is not one's canonical decimal form
// yields an id.
func TestGitHubIDProperty(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261016)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			id := r.Int63()
			if id == 0 {
				id = 1
			}
			alphabet := "0123456789+- x"
			var b strings.Builder
			for range r.Intn(21) {
				b.WriteByte(alphabet[r.Intn(len(alphabet))])
			}
			args[0] = reflect.ValueOf(id)
			args[1] = reflect.ValueOf(b.String())
		},
	}
	prop := func(id int64, junk string) bool {
		if githubID(map[string]any{"connector_id": "github", "user_id": strconv.FormatInt(id, 10)},
			"github") != id {
			return false
		}
		got := githubID(map[string]any{"connector_id": "github", "user_id": junk}, "github")
		if got == 0 {
			return true
		}
		return strconv.FormatInt(got, 10) == junk
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// The usage views need everything the intents views do and the
// federated:id scope; LoadConfig defaults the connector to github.
func TestRequireForUsage(t *testing.T) {
	const base = "mode: oidc\noidc:\n  issuerURL: https://sso.acme.test\n  clientID: c\n  clientSecret: s\n"
	const claims = "  claims:\n    username: preferred_username\n" +
		"    usernamePrefix: \"github:\"\n    groupsPrefix: \"github:\"\n"
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unconfigured", "", "none is configured"},
		{"mode none", "mode: none\n", `not "none"`},
		{"mode anonymous", "mode: anonymous\nanonymous:\n  username: viewer\n", `not "anonymous"`},
		{"oidc without prefixes", base + "  scopes: [openid, federated:id]\n", "usernamePrefix"},
		{"default scopes", base + claims, "federated:id"},
		{"scopes without federated:id", base + "  scopes: [openid, profile]\n" + claims, "federated:id"},
		{"dex github", base + "  scopes: [openid, profile, groups, federated:id]\n" + claims, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *Config
			if tc.yaml != "" {
				var err error
				if cfg, err = LoadConfig(writeConfig(t, tc.yaml)); err != nil {
					t.Fatalf("LoadConfig: %v", err)
				}
			}
			err := cfg.RequireForUsage()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("RequireForUsage = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("RequireForUsage = %v, want an error containing %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && cfg.OIDC.Claims.GitHubConnector != DefaultGitHubConnector {
				t.Errorf("GitHubConnector = %q, want the default %q", cfg.OIDC.Claims.GitHubConnector,
					DefaultGitHubConnector)
			}
		})
	}
}
