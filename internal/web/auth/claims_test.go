// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"slices"
	"testing"
)

func TestMapClaims(t *testing.T) {
	defaults := ClaimsConfig{Username: "email", Groups: "groups", DisplayName: "name"}
	cases := []struct {
		name    string
		claims  map[string]any
		cfg     ClaimsConfig
		want    *Identity
		wantErr bool
	}{
		{
			name: "defaults",
			claims: map[string]any{
				"email": "dev@acme.test", "name": "Dev",
				"groups": []any{"maul-approvers", "eng"},
			},
			cfg: defaults,
			want: &Identity{
				Username: "dev@acme.test", DisplayName: "Dev",
				Groups: []string{"maul-approvers", "eng"}, Session: true,
			},
		},
		{
			name:   "custom claim names",
			claims: map[string]any{"preferred_username": "dev", "roles": "ops"},
			cfg:    ClaimsConfig{Username: "preferred_username", Groups: "roles", DisplayName: "name"},
			want:   &Identity{Username: "dev", Groups: []string{"ops"}, Session: true},
		},
		{
			name:   "missing optional claims",
			claims: map[string]any{"email": "dev@acme.test"},
			cfg:    defaults,
			want:   &Identity{Username: "dev@acme.test", Session: true},
		},
		{
			name:   "non-string group entries skipped",
			claims: map[string]any{"email": "dev@acme.test", "groups": []any{"ok", 7, ""}},
			cfg:    defaults,
			want:   &Identity{Username: "dev@acme.test", Groups: []string{"ok"}, Session: true},
		},
		{
			name:    "missing username claim",
			claims:  map[string]any{"name": "Dev"},
			cfg:     defaults,
			wantErr: true,
		},
		{
			name:    "non-string username claim",
			claims:  map[string]any{"email": 42},
			cfg:     defaults,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mapClaims(tc.claims, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("mapClaims accepted unusable claims")
				}
				return
			}
			if err != nil {
				t.Fatalf("mapClaims: %v", err)
			}
			if got.Username != tc.want.Username || got.DisplayName != tc.want.DisplayName ||
				!slices.Equal(got.Groups, tc.want.Groups) || !got.Session {
				t.Errorf("identity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIdentityDisplay(t *testing.T) {
	if got := (Identity{Username: "u"}).Display(); got != "u" {
		t.Errorf("Display = %q, want username fallback", got)
	}
	if got := (Identity{Username: "u", DisplayName: "D"}).Display(); got != "D" {
		t.Errorf("Display = %q, want display name", got)
	}
}
