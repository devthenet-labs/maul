// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package forge

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
)

// Sentinel errors callers branch on (they become Finding condition reasons).
var (
	// ErrNoMatch: no Forge's filters cover the repository.
	ErrNoMatch = errors.New("no forge matches repository")
	// ErrAmbiguous: two or more Forges match with equal specificity.
	ErrAmbiguous = errors.New("ambiguous forge match")
)

// Resolved is a repository bound to the Forge allowed to act on it.
type Resolved struct {
	// Forge whose credentials cover the repository.
	Forge *v1alpha1.Forge
	// Repo is the parsed owner/name.
	Repo ghclient.Repo
	// Host the repository lives on (lowercase, no port).
	Host string
}

// ParseRepoURL splits a repository URL into host and owner/name. It accepts
// https URLs with or without a .git suffix or trailing path segments and
// normalizes case (hosts and owners are case-insensitive on every supported
// forge; the repo name's case is preserved for display but matching is
// case-insensitive).
func ParseRepoURL(raw string) (host string, repo ghclient.Repo, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", ghclient.Repo{}, fmt.Errorf("parse repository url %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", ghclient.Repo{}, fmt.Errorf("repository url %q: unsupported scheme %q", raw, u.Scheme)
	}
	if u.Hostname() == "" {
		return "", ghclient.Repo{}, fmt.Errorf("repository url %q: no host", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", ghclient.Repo{}, fmt.Errorf("repository url %q: path is not owner/repo", raw)
	}
	name := strings.TrimSuffix(parts[1], ".git")
	if name == "" {
		return "", ghclient.Repo{}, fmt.Errorf("repository url %q: empty repository name", raw)
	}
	return strings.ToLower(u.Hostname()), ghclient.Repo{Owner: parts[0], Name: name}, nil
}

// Host returns the host a Forge covers: spec.baseURL's host when set (a bare
// host or a URL are both accepted), else the provider's public host.
func Host(f *v1alpha1.Forge) string {
	base := strings.TrimSpace(f.Spec.BaseURL)
	if base == "" {
		return "github.com"
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(base)
}

// WebHosts are the hosts a Forge serves people and clients on: Host, and,
// when that host is an API host (it starts with "api."), the web host it
// sits under too: GitHub Enterprise Cloud with data residency is configured
// as https://api.<subdomain>.ghe.com, and its web, git and download hosts
// are <subdomain>.ghe.com and the names below it. A GHES base URL
// (https://ghe/api/v3) is already its web host. A host judged "equal to or
// below" these covers the api., codeload. and uploads. subdomains alike.
// For github.com the egress proxy's hard-deny table already covers every
// host; the list says so all the same.
func WebHosts(f *v1alpha1.Forge) []string {
	h := Host(f)
	if h == "" {
		return nil
	}
	if web, ok := strings.CutPrefix(h, "api."); ok && strings.Contains(web, ".") {
		return []string{h, web}
	}
	return []string{h}
}

// specificity ranks a matching Forge: a spec constrained by orgs beats one
// without, then one constrained by repositories beats one without. Equal
// specificity among matches is ErrAmbiguous.
func specificity(f *v1alpha1.Forge) int {
	s := 0
	if len(f.Spec.Orgs) > 0 {
		s += 2
	}
	if len(f.Spec.Repositories) > 0 {
		s++
	}
	return s
}

// matches reports whether the Forge's filters cover the repository. Invalid
// repository regexes never match (the Forge reconciler surfaces them on the
// Forge's Ready condition).
func matches(f *v1alpha1.Forge, host string, repo ghclient.Repo) bool {
	if f.Spec.Suspend || Host(f) != host {
		return false
	}
	if len(f.Spec.Orgs) > 0 {
		ok := slices.ContainsFunc(f.Spec.Orgs, func(o string) bool {
			return strings.EqualFold(o, repo.Owner)
		})
		if !ok {
			return false
		}
	}
	if len(f.Spec.Repositories) > 0 {
		full := repo.Owner + "/" + repo.Name
		ok := slices.ContainsFunc(f.Spec.Repositories, func(pattern string) bool {
			re, err := regexp.Compile(pattern)
			return err == nil && re.MatchString(full)
		})
		if !ok {
			return false
		}
	}
	return true
}

// Resolve picks the Forge covering repoURL from the given Forges. It is pure
// — callers list the Forges (informer cache) and pass them in.
func Resolve(forges []v1alpha1.Forge, repoURL string) (*Resolved, error) {
	host, repo, err := ParseRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	best := -1
	won := make([]*v1alpha1.Forge, 0, 2)
	for i := range forges {
		f := &forges[i]
		if !matches(f, host, repo) {
			continue
		}
		switch s := specificity(f); {
		case s > best:
			best = s
			won = append(won[:0], f)
		case s == best:
			won = append(won, f)
		}
	}
	switch len(won) {
	case 0:
		return nil, fmt.Errorf("%w: %s", ErrNoMatch, repoURL)
	case 1:
		return &Resolved{Forge: won[0], Repo: repo, Host: host}, nil
	default:
		names := make([]string, len(won))
		for i, f := range won {
			names[i] = f.Name
		}
		slices.Sort(names)
		return nil, fmt.Errorf("%w: %s matched by %s", ErrAmbiguous, repoURL, strings.Join(names, ", "))
	}
}
