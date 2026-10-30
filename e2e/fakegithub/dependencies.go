// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Issue dependencies (GitHub's "blocked by" relationships), as pinned live on
// github.com on 2026-10-28: GET .../issues/{n}/dependencies/blocked_by lists
// the blocking issues, closed ones included with their state_reason, in a
// listing whose ETag changes when a blocker's state does; every issue
// carries issue_dependencies_summary, blocked_by counting the open blockers
// and total_blocked_by all of them.
//
// Not pinned live, and modelled conservatively: a blocker in a repository
// the installation token was not minted for is left out of the listing but
// still counted in the summary (SetCrossRepositoryBlockersVisible lists it
// all the same), and a GitHub without the feature answers the listing 404
// and sends no summary (SetDependenciesUnsupported).

// DependenciesSummary is GitHub's issue_dependencies_summary.
type DependenciesSummary struct {
	BlockedBy      int `json:"blocked_by"`
	Blocking       int `json:"blocking"`
	TotalBlockedBy int `json:"total_blocked_by"`
	TotalBlocking  int `json:"total_blocking"`
}

// dependencyMode is how the fake answers dependency reads.
type dependencyMode int

const (
	dependenciesOn dependencyMode = iota
	// dependenciesUnsupported: a GitHub without issue dependencies.
	dependenciesUnsupported
	// dependenciesFailing: every listing answers 500.
	dependenciesFailing
	// dependenciesThrottled: every listing answers a throttling 403 that
	// go-github cannot type as a rate limit (a Retry-After header, no
	// documentation_url anchor).
	dependenciesThrottled
)

// AddBlockedBy records issue number as blocked by issue blocker; both are
// the fake's global issue numbers, each in its own repository.
func (s *Server) AddBlockedBy(number, blocker int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.blockedBy[number], blocker) {
		s.blockedBy[number] = append(s.blockedBy[number], blocker)
		s.touchDependents(number)
	}
}

// RemoveBlockedBy removes that relationship.
func (s *Server) RemoveBlockedBy(number, blocker int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockedBy[number] = slices.DeleteFunc(s.blockedBy[number], func(n int) bool { return n == blocker })
	s.touchDependents(number)
}

// touchDependents moves issue number's updated_at, as a relationship change
// does on GitHub. Callers hold s.mu.
func (s *Server) touchDependents(number int) {
	if is, ok := s.issues[number]; ok {
		s.touch(is)
	}
}

// SetDependenciesUnsupported makes the fake a GitHub without issue
// dependencies: the listing answers 404 and issues carry no summary.
func (s *Server) SetDependenciesUnsupported(on bool) {
	s.setDependencyMode(on, dependenciesUnsupported)
}

// SetDependenciesFailing makes every blocked-by listing answer 500.
func (s *Server) SetDependenciesFailing(on bool) { s.setDependencyMode(on, dependenciesFailing) }

// SetDependenciesThrottled makes every blocked-by listing answer a 403 that
// only its Retry-After header marks as throttling.
func (s *Server) SetDependenciesThrottled(on bool) { s.setDependencyMode(on, dependenciesThrottled) }

func (s *Server) setDependencyMode(on bool, mode dependencyMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case on:
		s.dependencyMode = mode
	case s.dependencyMode == mode:
		s.dependencyMode = dependenciesOn
	}
}

// SetCrossRepositoryBlockersVisible lists blockers in repositories the
// token was not minted for, as GitHub may (not pinned live).
func (s *Server) SetCrossRepositoryBlockersVisible(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crossBlockersVisible = on
}

// BlockedByReads counts the blocked-by listings answered with a body or a
// 304, for assertions about what was read.
func (s *Server) BlockedByReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blockedByReads
}

// summaryOf is issue number's dependency summary, nil when the fake offers
// none. Callers hold s.mu.
func (s *Server) summaryOf(number int) *DependenciesSummary {
	if s.dependencyMode == dependenciesUnsupported {
		return nil
	}
	out := &DependenciesSummary{}
	for _, b := range s.blockedBy[number] {
		out.TotalBlockedBy++
		if is, ok := s.issues[b]; ok && is.State == "open" {
			out.BlockedBy++
		}
	}
	for n, bs := range s.blockedBy {
		if slices.Contains(bs, number) {
			out.TotalBlocking++
			if is, ok := s.issues[n]; ok && is.State == "open" {
				out.Blocking++
			}
		}
	}
	return out
}

// withSummary is a copy of is carrying its dependency summary. Callers hold
// s.mu.
func (s *Server) withSummary(is *Issue) *Issue {
	out := *is
	out.Dependencies = s.summaryOf(is.Number)
	return &out
}

// listBlockedBy answers GET /repos/{o}/{r}/issues/{number}/dependencies/
// blocked_by: the blockers the caller's token may see, conditional on the
// listing's tag. One page holds them all (GitHub caps an issue's blockers
// under the page size the client asks for); a later page is empty.
func (s *Server) listBlockedBy(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("number"))
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.dependencyMode {
	case dependenciesUnsupported:
		notFound(w)
		return
	case dependenciesFailing:
		http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		return
	case dependenciesThrottled:
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		return
	}
	if _, ok := s.issues[number]; !ok {
		notFound(w)
		return
	}
	s.blockedByReads++
	out := []*Issue{}
	if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page <= 1 {
		for _, b := range s.blockedBy[number] {
			if is, ok := s.issues[b]; ok && s.visibleTo(r, is) {
				out = append(out, s.withSummary(is))
			}
		}
	}
	writeJSONTagged(w, r, out)
}

// visibleTo reports whether r's credential may see issue is in a listing:
// a token minted for named repositories sees issues in those alone, unless
// cross-repository blockers are visible. Callers hold s.mu.
func (s *Server) visibleTo(r *http.Request, is *Issue) bool {
	scope, isToken := s.minted[bearer(r)]
	if !isToken || len(scope.Repositories) == 0 || s.crossBlockersVisible {
		return true
	}
	repo := is.RepositoryURL[strings.LastIndex(is.RepositoryURL, "/")+1:]
	return slices.ContainsFunc(scope.Repositories, func(name string) bool { return strings.EqualFold(name, repo) })
}
