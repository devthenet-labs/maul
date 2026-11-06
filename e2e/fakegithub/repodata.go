// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

// HeadSHA is the commit every un-pushed branch resolves to — the SHA
// source-controller pins Repositories to (gitdata.go's fixed base).
const HeadSHA = BaseSHA

// tarballRedirect mimics GitHub's archive API: a 302 to the signed download
// URL (here: the fake's own /_tarball path).
func (s *Server) tarballRedirect(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	ref := r.PathValue("ref")
	loc := fmt.Sprintf("%s/api/v3/_tarball/%s/%s/%s", s.externalURL, owner, repo, ref)
	http.Redirect(w, r, loc, http.StatusFound)
}

// SetRepoFile adds a file to owner/repo's tree, served in its tarball beside
// the fixed ones (a repository's .maul/agent.yaml, say); setting a path
// again replaces its content.
func (s *Server) SetRepoFile(owner, repo, path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, repo)
	if s.repoFiles[key] == nil {
		s.repoFiles[key] = map[string]string{}
	}
	s.repoFiles[key][path] = content
}

// ServeCommitTrees makes the tarball of a commit pushed to the fake its own
// tree: the base's (the fixed files and SetRepoFile's) with every commit on
// its way from the base applied, oldest first, under a top-level directory
// naming the commit, as GitHub's archive of that commit is. Off (the
// default), every ref's tarball is the base's tree.
func (s *Server) ServeCommitTrees(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitTrees = on
}

// tarball serves the archive itself: a deterministic tree-only tar.gz with
// GitHub's top-level "<owner>-<repo>-<sha7>/" directory, holding the fixed
// files and then those SetRepoFile added, by path; with commit trees served,
// a pushed commit's own changes applied.
func (s *Server) tarball(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	s.mu.Lock()
	tree := map[string]string{
		"README.md": "# " + owner + "/" + repo + "\n",
		"app.js":    "vulnerable();\n",
	}
	maps.Copy(tree, s.repoFiles[repoKey(owner, repo)])
	sha := HeadSHA
	if s.commitTrees {
		if ref := s.resolve(repoKey(owner, repo), r.PathValue("ref")); s.knownCommit(ref) {
			sha = ref
			s.applyCommits(tree, ref)
		}
	}
	s.mu.Unlock()
	top := fmt.Sprintf("%s-%s-%s/", owner, repo, sha[:7])

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// The fixed files first, then the rest by path, as before.
	paths := []string{"README.md", "app.js"}
	for _, path := range slices.Sorted(maps.Keys(tree)) {
		if path != "README.md" && path != "app.js" {
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		body, ok := tree[path]
		if !ok {
			continue
		}
		_ = tw.WriteHeader(&tar.Header{
			Name: top + path, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0),
		})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()

	w.Header().Set("Content-Type", "application/gzip")
	_, _ = w.Write(buf.Bytes())
}
