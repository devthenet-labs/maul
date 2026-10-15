// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"

	"github.com/google/go-github/v90/github"
)

// FileContent returns the bytes of the file at path in repo at ref (a
// commit SHA or a branch). A path that is absent at ref is GitHub's 404
// (IsNotFound); a directory, a symlink or a file too large for the contents
// API to inline (over 1 MiB) is an error.
func (c *Client) FileContent(ctx context.Context, repo Repo, path, ref string) ([]byte, error) {
	file, _, _, err := c.gh.Repositories.GetContents(ctx, repo.Owner, repo.Name, path,
		&github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("ghclient: read %s at %s in %s: %w", path, ref, repo, err)
	}
	if file == nil || file.GetType() != "file" {
		return nil, fmt.Errorf("ghclient: read %s at %s in %s: not a file", path, ref, repo)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("ghclient: read %s at %s in %s: %w", path, ref, repo, err)
	}
	return []byte(content), nil
}
