// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v90/github"
)

// pushActivities is how many of a branch's latest activities PushedAt
// reads: one page, newest first. A commit pushed more than this many
// branch moves ago is read as not found.
const pushActivities = 100

// PushedAt is when GitHub recorded the latest push (or force push) that
// moved branch to sha: the repository activity API's timestamp, written by
// GitHub as it accepts the push. It is GitHub's clock, the one a comment's
// created_at is on, and no author or committer date a pusher can set. ok
// is false when none of the branch's latest activities moved it to sha. It
// needs contents read.
func (c *Client) PushedAt(ctx context.Context, repo Repo, branch, sha string) (at time.Time, ok bool,
	err error) {
	acts, _, err := c.gh.Repositories.ListRepositoryActivities(ctx, repo.Owner, repo.Name,
		&github.ListRepositoryActivityOptions{Ref: "refs/heads/" + branch, Direction: "desc",
			PerPage: pushActivities})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("ghclient: list activity of %s in %s: %w", branch, repo, err)
	}
	for _, a := range acts {
		if a == nil || a.After != sha || a.Timestamp == nil {
			continue
		}
		switch a.ActivityType {
		case "push", "force_push":
			return a.Timestamp.Time, true, nil
		}
	}
	return time.Time{}, false, nil
}
