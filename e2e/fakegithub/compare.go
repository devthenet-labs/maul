// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"io"
	"maps"
	"net/http"
	"strings"
)

// SetComparePatch supplies the patch returned for base...head when requested
// with GitHub's patch media type. Tests can show the exact diff the revise
// round is allowed to see without giving its pod a second source tree.
func (s *Server) SetComparePatch(base, head, patch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patches[base+"..."+head] = patch
}

// SetDefaultComparePatch supplies the patch returned for every base...head
// no SetComparePatch names, for a flow whose heads a test cannot know in
// advance (a round pushed by maul). Unset, such a compare is a 404.
func (s *Server) SetDefaultComparePatch(patch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultPatch = patch
}

// SetParents records commit ancestry for the compare endpoint: each commit
// mapped to its parent. A linear history is all the flows need; commits it
// does not name are unrelated to every other.
func (s *Server) SetParents(parents map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maps.Copy(s.parents, parents)
}

// SetBranch points a branch at a commit, the way a push (or a force-push)
// does, in every repository with no branch of that name of its own; the
// compare endpoint resolves branch names through it.
func (s *Server) SetBranch(name, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.git.shared["heads/"+name] = sha
}

// Compares reports how many compare calls the fake has answered.
func (s *Server) Compares() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compares
}

// compare mimics GET /repos/{owner}/{repo}/compare/{base}...{head}: how head
// relates to base — "identical", "behind" (head is an ancestor of base),
// "ahead" (head descends from base), or "diverged".
func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	base, head, ok := strings.Cut(r.PathValue("spec"), "...")
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compares++
	if strings.Contains(r.Header.Get("Accept"), "patch") {
		patch, ok := s.patches[base+"..."+head]
		if !ok && s.defaultPatch != "" {
			patch, ok = s.defaultPatch, true
		}
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, patch)
		return
	}
	repo := repoKey(r.PathValue("owner"), r.PathValue("repo"))
	base, head = s.resolve(repo, base), s.resolve(repo, head)
	status, ahead, behind := "diverged", 0, 0
	switch {
	case base == head:
		status = "identical"
	case s.steps(base, head) > 0:
		status, behind = "behind", s.steps(base, head)
	case s.steps(head, base) > 0:
		status, ahead = "ahead", s.steps(head, base)
	}
	out := map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind}
	if mb := s.mergeBase(atBase(s, base), atBase(s, head)); mb != "" {
		out["merge_base_commit"] = map[string]any{"sha": mb}
	}
	writeJSON(w, out)
}

// atBase is rev, or the fixed base for a branch never pushed, where every
// such branch sits (getRef). Callers hold s.mu.
func atBase(s *Server, rev string) string {
	if s.knownCommit(rev) {
		return rev
	}
	return BaseSHA
}

// parentOf is c's first parent: the ancestry SetParents recorded, else the
// pushed commit's own; "" for none. Callers hold s.mu.
func (s *Server) parentOf(c string) string {
	if p, ok := s.parents[c]; ok {
		return p
	}
	if rec, ok := s.git.commits[c]; ok && len(rec.Parents) > 0 {
		return rec.Parents[0]
	}
	return ""
}

// mergeBase is the newest commit in both a's and b's first-parent history,
// each included, as GitHub's merge_base_commit; "" when they share none.
// Callers hold s.mu.
func (s *Server) mergeBase(a, b string) string {
	seen := map[string]bool{}
	for c := a; c != "" && !seen[c]; c = s.parentOf(c) {
		seen[c] = true
	}
	walked := map[string]bool{}
	for c := b; c != "" && !walked[c]; c = s.parentOf(c) {
		if seen[c] {
			return c
		}
		walked[c] = true
	}
	return ""
}

// applyCommits writes into tree every pushed commit's changes on the way
// from the base to sha, oldest first: a blob replaces a path, a deletion
// removes it. Callers hold s.mu.
func (s *Server) applyCommits(tree map[string]string, sha string) {
	var chain []string
	seen := map[string]bool{}
	for c := sha; c != "" && !seen[c]; c = s.parentOf(c) {
		seen[c] = true
		if _, ok := s.git.commits[c]; ok {
			chain = append(chain, c)
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for path, blob := range s.git.trees[s.git.commits[chain[i]].Tree] {
			if blob == "" {
				delete(tree, path)
				continue
			}
			tree[path] = string(s.git.blobs[blob])
		}
	}
}

// steps is how many parent hops lead from descendant back to ancestor, or 0
// when ancestor is not in descendant's history. Callers hold s.mu.
func (s *Server) steps(descendant, ancestor string) int {
	n := 0
	for c := s.parents[descendant]; c != ""; c = s.parents[c] {
		n++
		if c == ancestor {
			return n
		}
	}
	return 0
}

// resolve maps a branch name to its head commit in the repository repo (a
// repoKey); anything else is taken to be a commit already. Callers hold
// s.mu.
func (s *Server) resolve(repo, rev string) string {
	if sha, ok := s.git.refIn(repo, "heads/"+rev); ok {
		return sha
	}
	return rev
}
