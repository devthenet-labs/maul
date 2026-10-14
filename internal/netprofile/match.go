// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"fmt"
	"net/url"
	"strings"
)

// Reason says why For returned no profile.
type Reason string

// The reasons For returns; "" when exactly one profile matched.
const (
	// ReasonNoMatch: no profile's host and patterns match the repository.
	ReasonNoMatch Reason = "no-match"
	// ReasonAmbiguous: more than one profile matches, so none is applied
	// (fail closed).
	ReasonAmbiguous Reason = "ambiguous"
	// ReasonBadURL: the repository URL is not an http(s) owner/name URL.
	ReasonBadURL Reason = "unparseable-url"
)

// For returns the one profile repoURL gets. The URL is parsed as the forge
// package parses a repository URL; a profile matches when its Host equals
// the URL's host (case-insensitively) and one of its patterns matches the
// lowercased owner/name in full. No match, a bad URL, or more than one
// matching profile returns the zero Profile and the reason.
func (s Set) For(repoURL string) (Profile, Reason) {
	host, owner, name, err := parseRepoURL(repoURL)
	if err != nil {
		return Profile{}, ReasonBadURL
	}
	subject := strings.ToLower(owner + "/" + name)
	var found *Profile
	for i := range s.profiles {
		p := &s.profiles[i]
		if !p.matches(host, subject) {
			continue
		}
		if found != nil {
			return Profile{}, ReasonAmbiguous
		}
		found = p
	}
	if found == nil {
		return Profile{}, ReasonNoMatch
	}
	return found.clone(), ""
}

// matches reports whether the profile covers the lowercased host and
// owner/name.
func (p *Profile) matches(host, subject string) bool {
	if !strings.EqualFold(host, p.Host) {
		return false
	}
	for _, re := range p.patterns {
		if re.MatchString(subject) {
			return true
		}
	}
	return false
}

// parseRepoURL splits an http(s) repository URL into its lowercased host and
// its owner and name, dropping a .git suffix and any path past the second
// element. It is internal/forge.ParseRepoURL without the forge types (a test
// pins the two to agree), since this package imports only the standard
// library.
func parseRepoURL(raw string) (host, owner, name string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", "", fmt.Errorf("parse repository url %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", "", fmt.Errorf("repository url %q: unsupported scheme %q", raw, u.Scheme)
	}
	if u.Hostname() == "" {
		return "", "", "", fmt.Errorf("repository url %q: no host", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("repository url %q: path is not owner/repo", raw)
	}
	name = strings.TrimSuffix(parts[1], ".git")
	if name == "" {
		return "", "", "", fmt.Errorf("repository url %q: empty repository name", raw)
	}
	return strings.ToLower(u.Hostname()), parts[0], name, nil
}
