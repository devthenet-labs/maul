// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// activityRec is one entry of a repository's activity, as GET
// /repos/{o}/{r}/activity lists it: a branch created or moved, when the
// fake accepted it.
type activityRec struct {
	repo   string // a repoKey
	ID     int64  `json:"id"`
	Before string `json:"before"`
	After  string `json:"after"`
	Ref    string `json:"ref"`
	Time   string `json:"timestamp"`
	Type   string `json:"activity_type"`
}

// recordActivity records ref (a "heads/<branch>") in repo moving from
// before to after as activity of kind at now. Callers hold s.mu.
func (s *Server) recordActivity(repo, ref, before, after, kind string) {
	s.git.activity = append(s.git.activity, activityRec{repo: repo, ID: int64(len(s.git.activity) + 1),
		Before: before, After: after, Ref: "refs/" + ref, Time: s.now().Format(time.RFC3339), Type: kind})
}

// listActivity answers GET /repos/{o}/{r}/activity: the repository's
// activity, newest first, filtered by ref (refs/heads/<branch> or the bare
// branch) and activity_type, at most per_page (default 30, max 100).
func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref := q.Get("ref")
	if ref != "" && !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	limit := 30
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 {
		limit = min(n, 100)
	}
	repo := repoKey(r.PathValue("owner"), r.PathValue("repo"))
	s.mu.Lock()
	out := []activityRec{}
	for _, a := range slices.Backward(s.git.activity) {
		if len(out) == limit {
			break
		}
		if a.repo != repo || (ref != "" && a.Ref != ref) ||
			(q.Get("activity_type") != "" && a.Type != q.Get("activity_type")) {
			continue
		}
		out = append(out, a)
	}
	s.mu.Unlock()
	if q.Get("direction") == "asc" {
		slices.Reverse(out)
	}
	writeJSON(w, out)
}
