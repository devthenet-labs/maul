// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package auth

import (
	"fmt"
	"strconv"
)

// MapClaims derives the identity from verified ID-token claims using the
// configured claim names — the one claims-to-identity mapping shared by the
// status server's session flow and the evaluation API's bearer flow, so the
// two can never disagree about who a token names.
func MapClaims(claims map[string]any, cfg ClaimsConfig) (*Identity, error) {
	return mapClaims(claims, cfg)
}

// mapClaims derives the identity from verified ID-token claims using the
// configured claim names. The username claim is required — an identity that
// cannot be named cannot be access-reviewed; groups and display name are
// best-effort. With RequireVerifiedEmail a token whose email_verified claim
// is not true names no one. The configured prefixes are applied exactly
// once, here, to the username and to every group: this is the only place an
// identity is made from claims, so no path reaches an access review
// unprefixed. The display name stays the provider's: the UI shows it, RBAC
// never sees it.
func mapClaims(claims map[string]any, cfg ClaimsConfig) (*Identity, error) {
	username, _ := claims[cfg.Username].(string)
	if username == "" {
		return nil, fmt.Errorf("token has no usable %q claim", cfg.Username)
	}
	if cfg.RequireVerifiedEmail && !emailVerified(claims["email_verified"]) {
		return nil, fmt.Errorf("token's email_verified claim is not true")
	}
	id := &Identity{Username: cfg.UsernamePrefix + username, Session: true}
	if name, _ := claims[cfg.DisplayName].(string); name != "" {
		id.DisplayName = name
	}
	switch groups := claims[cfg.Groups].(type) {
	case []any:
		for _, g := range groups {
			if s, ok := g.(string); ok && s != "" {
				id.Groups = append(id.Groups, cfg.GroupsPrefix+s)
			}
		}
	case string:
		if groups != "" {
			id.Groups = []string{cfg.GroupsPrefix + groups}
		}
	}
	id.GitHubID = githubID(claims["federated_claims"], cfg.GitHubConnector)
	if login, _ := claims["preferred_username"].(string); id.GitHubID != 0 && login != "" &&
		login == strconv.FormatInt(id.GitHubID, 10) {
		// Dex's GitHub connector with useLoginAsID puts the login in
		// user_id, and GitHub allows all-digit logins: a login equal to
		// the "id" would name whoever holds that numeric id instead.
		// Without useLoginAsID, user_id is the id and preferred_username
		// the login, and only an account whose login is its own id could
		// match, which is refused too rather than guessed at.
		id.GitHubID = 0
	}
	return id, nil
}

// githubID reads Dex's federated_claims ({"connector_id", "user_id"}, sent
// when the federated:id scope is requested): the upstream user id when the
// connector is the configured GitHub one and the id is a positive decimal
// int64 without a sign or leading zeros, else 0. Any other connector (LDAP,
// another OIDC provider) names someone patchy cannot tie to a GitHub
// account, and an empty connector setting matches nothing. The connector
// must not set useLoginAsID: user_id is then the login, which MapClaims
// refuses when it reads as an id (it equals preferred_username).
func githubID(v any, connector string) int64 {
	fc, ok := v.(map[string]any)
	if !ok || connector == "" {
		return 0
	}
	if c, _ := fc["connector_id"].(string); c != connector {
		return 0
	}
	raw, _ := fc["user_id"].(string)
	if raw == "" || raw[0] < '1' || raw[0] > '9' || len(raw) > 19 {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

// emailVerified reads an email_verified claim. OIDC Core makes it a JSON
// boolean; a few providers send the string "true", which is accepted too.
// Anything else, an absent claim included, is unverified.
func emailVerified(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}
