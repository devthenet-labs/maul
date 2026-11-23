// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigUnconfigured(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "absent.yaml")} {
		cfg, err := LoadConfig(path)
		if err != nil || cfg != nil {
			t.Errorf("LoadConfig(%q) = %+v, %v; want nil, nil", path, cfg, err)
		}
	}
}

func TestLoadConfigModes(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name: "none",
			yaml: "mode: none\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.SessionDuration.Duration != DefaultSessionDuration {
					t.Errorf("sessionDuration default = %v", cfg.SessionDuration.Duration)
				}
			},
		},
		{
			name: "anonymous",
			yaml: "mode: anonymous\nanonymous:\n  username: viewer\n  groups: [maul-viewers]\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.Anonymous.Username != "viewer" || len(cfg.Anonymous.Groups) != 1 {
					t.Errorf("anonymous = %+v", cfg.Anonymous)
				}
			},
		},
		{
			name: "oidc with defaults",
			yaml: "mode: oidc\nsessionDuration: 12h\noidc:\n  issuerURL: https://sso.acme.test\n" +
				"  clientID: maul\n  clientSecret: s3cret\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.SessionDuration.Duration != 12*time.Hour {
					t.Errorf("sessionDuration = %v", cfg.SessionDuration.Duration)
				}
				if len(cfg.OIDC.Scopes) == 0 || cfg.OIDC.Scopes[0] != "openid" {
					t.Errorf("scopes default = %v", cfg.OIDC.Scopes)
				}
				cl := cfg.OIDC.Claims
				if cl.Username != "email" || cl.Groups != "groups" || cl.DisplayName != "name" {
					t.Errorf("claims defaults = %+v", cl)
				}
			},
		},
		{name: "missing mode", yaml: "sessionDuration: 1h\n", wantErr: true},
		{name: "unknown mode", yaml: "mode: basic\n", wantErr: true},
		{name: "anonymous without username", yaml: "mode: anonymous\n", wantErr: true},
		{name: "oidc without issuer", yaml: "mode: oidc\noidc:\n  clientID: x\n  clientSecret: y\n", wantErr: true},
		{
			name:    "oidc without secret",
			yaml:    "mode: oidc\noidc:\n  issuerURL: https://sso\n  clientID: x\n",
			wantErr: true,
		},
		{
			name: "oidc with both secret forms",
			yaml: "mode: oidc\noidc:\n  issuerURL: https://sso\n  clientID: x\n" +
				"  clientSecret: a\n  clientSecretFile: /b\n",
			wantErr: true,
		},
		{name: "unknown field", yaml: "mode: none\nbogus: true\n", wantErr: true},
		{name: "bad duration", yaml: "mode: none\nsessionDuration: soon\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tc.yaml))
			if tc.wantErr {
				if err == nil {
					t.Fatal("LoadConfig accepted an invalid config")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

func TestClientSecretFile(t *testing.T) {
	path := writeConfig(t, "s3cret\n")
	o := &OIDCConfig{ClientSecretFile: path}
	got, err := o.clientSecret()
	if err != nil || got != "s3cret" {
		t.Errorf("clientSecret = %q, %v", got, err)
	}
	if _, err := (&OIDCConfig{ClientSecretFile: writeConfig(t, "\n")}).clientSecret(); err == nil {
		t.Error("empty secret file accepted")
	}
}

func TestNewModes(t *testing.T) {
	// Unconfigured: no identity, ever, and no routes.
	a, err := New(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	rec := httptest.NewRecorder()
	id, err := a.Identify(rec, httptest.NewRequest(http.MethodGet, "/api/findings", nil))
	if id != nil || err != nil {
		t.Errorf("unconfigured Identify = %+v, %v", id, err)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("unconfigured posture set cookies")
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth2/authorize", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Errorf("unconfigured /oauth2/authorize = %d, want 404", res.Code)
	}

	// None: a fixed dev identity without a session.
	a, err = New(t.Context(), &Config{Mode: ModeNone}, nil)
	if err != nil {
		t.Fatalf("New(none): %v", err)
	}
	id, _ = a.Identify(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if id == nil || id.Session {
		t.Errorf("none Identify = %+v, want session-less identity", id)
	}

	// Anonymous: the configured identity.
	a, err = New(t.Context(), &Config{
		Mode:      ModeAnonymous,
		Anonymous: &AnonymousConfig{Username: "viewer", Groups: []string{"g"}},
	}, nil)
	if err != nil {
		t.Fatalf("New(anonymous): %v", err)
	}
	id, _ = a.Identify(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if id == nil || id.Username != "viewer" || len(id.Groups) != 1 || id.Session {
		t.Errorf("anonymous Identify = %+v", id)
	}

	// Unknown mode is rejected (validation normally catches it earlier).
	if _, err := New(t.Context(), &Config{Mode: "basic"}, nil); err == nil {
		t.Error("New accepted an unknown mode")
	}
}

// Editing Project settings ties a session to an approver by the person's
// numeric GitHub id, so its gate admits only oidc with both claim prefixes
// and the federated:id scope. Mode none is refused here whatever flag the
// binary was started with: --intents-dev-insecure admits it for the read-only
// views, and this package offers no development bypass for a write. The
// insecure switch is another thing (cookies without Secure over plain HTTP),
// and does not change who a session is, so oidc with it on is admitted.
func TestRequireForProjectSettings(t *testing.T) {
	const base = "mode: oidc\noidc:\n  issuerURL: https://sso.acme.test\n  clientID: c\n  clientSecret: s\n"
	const scopes = "  scopes: [openid, profile, groups, federated:id]\n"
	const claims = "  claims:\n    username: preferred_username\n" +
		"    usernamePrefix: \"github:\"\n    groupsPrefix: \"github:\"\n"
	cases := []struct {
		name    string
		yaml    string // "" means no auth config at all
		wantErr string
	}{
		{"unconfigured", "", "none is configured"},
		{"mode none, the dev-insecure posture", "mode: none\n", `not "none"`},
		{"mode none with insecure cookies", "mode: none\ninsecure: true\n", `not "none"`},
		{"mode anonymous", "mode: anonymous\nanonymous:\n  username: viewer\n", `not "anonymous"`},
		{"oidc without prefixes", base + scopes + "  claims:\n    username: preferred_username\n",
			"usernamePrefix"},
		{"oidc with only the username prefix", base + scopes +
			"  claims:\n    username: preferred_username\n    usernamePrefix: \"github:\"\n", "groupsPrefix"},
		{"oidc with only the groups prefix", base + scopes +
			"  claims:\n    username: preferred_username\n    groupsPrefix: \"github:\"\n", "usernamePrefix"},
		{"oidc email username unverified", base + scopes +
			"  claims:\n    usernamePrefix: \"oidc:\"\n    groupsPrefix: \"oidc:\"\n", "requireVerifiedEmail"},
		{"default scopes", base + claims, FederatedIDScope},
		{"scopes without federated:id", base + "  scopes: [openid, profile]\n" + claims, FederatedIDScope},
		{"dex github", base + scopes + claims, ""},
		{"dex github, verified email username", base + scopes + "  claims:\n    usernamePrefix: \"oidc:\"\n" +
			"    groupsPrefix: \"oidc:\"\n    requireVerifiedEmail: true\n", ""},
		{"dex github over plain http", "insecure: true\n" + base + scopes + claims, ""},
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
			err := cfg.RequireForProjectSettings()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("RequireForProjectSettings = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("RequireForProjectSettings = %v, want an error containing %q", err, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), "Project settings"):
				t.Errorf("RequireForProjectSettings = %v, want it to name Project settings", err)
			}
			// The page keys a person as the usage views do; the two gates
			// must not drift apart.
			if usage := cfg.RequireForUsage(); (usage == nil) != (err == nil) {
				t.Errorf("RequireForProjectSettings = %v but RequireForUsage = %v, want them to agree", err, usage)
			}
		})
	}
}

// A configuration built by hand, never through LoadConfig, has no connector
// default: with no GitHub connector named no token could carry a GitHub id.
func TestRequireForProjectSettingsNeedsTheConnector(t *testing.T) {
	cfg := &Config{Mode: ModeOIDC, OIDC: &OIDCConfig{
		Scopes: []string{"openid", FederatedIDScope},
		Claims: ClaimsConfig{Username: "preferred_username", UsernamePrefix: "github:", GroupsPrefix: "github:"},
	}}
	err := cfg.RequireForProjectSettings()
	if err == nil || !strings.Contains(err.Error(), "githubConnector") {
		t.Errorf("RequireForProjectSettings = %v, want a githubConnector error", err)
	}
	cfg.OIDC.Claims.GitHubConnector = DefaultGitHubConnector
	if err := cfg.RequireForProjectSettings(); err != nil {
		t.Errorf("RequireForProjectSettings with the connector = %v, want nil", err)
	}
}
