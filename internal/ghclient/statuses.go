// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/go-github/v90/github"
)

// maxStatusDescription is GitHub's limit on a commit status description.
const maxStatusDescription = 140

// statusStates are the states GitHub accepts on a commit status.
var statusStates = []string{"error", "failure", "pending", "success"}

// CreateCommitStatus posts a commit status on sha and returns its id, which
// a caller keeps to tell its own status from a later one under the same
// context. The state, context and description are validated before any
// request: GitHub's own refusal of an overlong description is a 422 that
// names nothing. targetURL may be empty. The token needs statuses write.
func (c *Client) CreateCommitStatus(ctx context.Context, repo Repo, sha, state, statusContext, description,
	targetURL string) (int64, error) {
	switch {
	case sha == "":
		return 0, errors.New("ghclient: create status: sha is empty")
	case !slices.Contains(statusStates, state):
		return 0, fmt.Errorf("ghclient: create status on %s@%s: state %q is not one of %v", repo, sha, state, statusStates)
	case statusContext == "":
		return 0, fmt.Errorf("ghclient: create status on %s@%s: context is empty", repo, sha)
	case len(description) > maxStatusDescription:
		return 0, fmt.Errorf("ghclient: create status on %s@%s: description is %d bytes, over %d",
			repo, sha, len(description), maxStatusDescription)
	}
	in := github.RepoStatus{State: &state, Context: &statusContext}
	if description != "" {
		in.Description = &description
	}
	if targetURL != "" {
		in.TargetURL = &targetURL
	}
	out, _, err := c.gh.Repositories.CreateStatus(ctx, repo.Owner, repo.Name, sha, in)
	if err != nil {
		return 0, fmt.Errorf("ghclient: create status on %s@%s: %w", repo, sha, err)
	}
	return out.GetID(), nil
}
