// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/go-github/v90/github"
)

// ErrArchiveNotFound marks a Tarball the forge answered 404 for: the ref does
// not exist, or the credential cannot see the repository (GitHub answers a
// private repository's missing access the same way).
var ErrArchiveNotFound = errors.New("archive not found")

// Tarball streams the repository's tarball archive at ref (a SHA, branch, or
// tag). The caller must Close the reader. The archive is the tree only — no
// .git — which is exactly what the artifact server serves; agents synthesize
// their local git state from it.
func (c *Client) Tarball(ctx context.Context, repo Repo, ref string) (io.ReadCloser, error) {
	opts := &github.RepositoryContentGetOptions{Ref: ref}
	u, linkResp, err := c.gh.Repositories.GetArchiveLink(ctx, repo.Owner, repo.Name, github.Tarball, opts, 3)
	if err != nil {
		// go-github reports a non-redirect answer as a bare "unexpected
		// status code" error, so the 404 is read from the response too.
		if IsNotFound(err) || (linkResp != nil && linkResp.StatusCode == http.StatusNotFound) {
			err = fmt.Errorf("%w: %w", ErrArchiveNotFound, err)
		}
		return nil, fmt.Errorf("ghclient: archive link for %s@%s: %w", repo, ref, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("ghclient: archive request: %w", err)
	}
	// The client's transport injects auth — required on GHES; harmless on the
	// signed codeload URLs github.com hands out.
	resp, err := c.gh.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ghclient: download archive for %s@%s: %w", repo, ref, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("ghclient: download archive for %s@%s: status %d: %w",
				repo, ref, resp.StatusCode, ErrArchiveNotFound)
		}
		return nil, fmt.Errorf("ghclient: download archive for %s@%s: status %d", repo, ref, resp.StatusCode)
	}
	return resp.Body, nil
}
