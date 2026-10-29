// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/google/go-github/v90/github"
)

// Blocker is one issue GitHub records as blocking another (issue
// dependencies): its repository, number and state as the listing shows it.
type Blocker struct {
	// Repo is the blocker's repository, from its repository_url; zero when
	// GitHub sent a URL of another shape.
	Repo   Repo
	Number int
	// State is "open" or "closed"; StateReason GitHub's state_reason
	// ("completed", "not_planned", "duplicate", "reopened", "" on an issue
	// closed before reasons existed, or any later value).
	State       string
	StateReason string
	HTMLURL     string
}

// BlockerList is one conditional blocked-by listing.
type BlockerList struct {
	// Blockers are the blocking issues the token may see, in GitHub's
	// order. Nil when NotModified.
	Blockers []Blocker
	// ETag is the listing's entity tag, to pass back as the next call's
	// etag; on NotModified the tag the caller already holds.
	ETag string
	// NotModified reports GitHub answered 304 to the etag: the caller's
	// previous result stands.
	NotModified bool
}

// ListBlockedBy lists the issues blocking issue number in repo
// (GET /repos/{o}/{r}/issues/{n}/dependencies/blocked_by), every page, as a
// conditional request on the first: a non-empty etag is sent as
// If-None-Match. A GitHub without issue dependencies answers 404 (or 410),
// and a token that may not read them 403; the error predicates (IsNotFound,
// IsGone, IsForbidden) tell them apart.
func (c *Client) ListBlockedBy(ctx context.Context, repo Repo, number int64, etag string) (*BlockerList, error) {
	w := pageWalk{
		path:  repoPath(repo) + "/issues/" + strconv.FormatInt(number, 10) + "/dependencies/blocked_by",
		query: url.Values{},
		etag:  etag,
	}
	var out []Blocker
	tag, notModified, err := walk(ctx, c, w, func(page []*github.Issue) {
		for _, is := range page {
			out = append(out, Blocker{
				Repo: repoFromURL(is.GetRepositoryURL()), Number: is.GetNumber(), State: is.GetState(),
				StateReason: is.GetStateReason(), HTMLURL: is.GetHTMLURL(),
			})
		}
	})
	if err != nil {
		return nil, fmt.Errorf("ghclient: list the issues blocking %s#%d: %w", repo, number, err)
	}
	if notModified {
		return &BlockerList{ETag: tag, NotModified: true}, nil
	}
	return &BlockerList{Blockers: out, ETag: tag}, nil
}

// dependencySummary maps go-github's summary onto DependencySummary, nil
// when GitHub sent none.
func dependencySummary(s *github.IssueDependenciesSummary) *DependencySummary {
	if s == nil {
		return nil
	}
	return &DependencySummary{BlockedBy: s.GetBlockedBy(), TotalBlockedBy: s.GetTotalBlockedBy()}
}
