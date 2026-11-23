// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// ErrNoSuchUser reports GitHub answered 404 for a login's permission on a
// repository. A nonexistent login answers so, and so does a repository the
// credential cannot see, so it means "no access", never a transient failure.
var ErrNoSuchUser = errors.New("no such user")

// Repository permission levels, as the collaborator-permission endpoint's
// permission field reports them. That field is GitHub's legacy form: a
// maintainer reads as write and a triager as read.
const (
	PermissionAdmin    = "admin"
	PermissionMaintain = "maintain"
	PermissionWrite    = "write"
	PermissionRead     = "read"
	PermissionNone     = "none"
)

// Collaborator is what GitHub's collaborator-permission endpoint answers
// for a login on a repository: the account the login resolved to (its
// canonical login and numeric id, both empty when the answer names no
// account) and its permission there.
type Collaborator struct {
	Login      string
	ID         int64
	Permission string
}

// CollaboratorPermission returns login's permission on repo (GET
// /repos/{owner}/{repo}/collaborators/{login}/permission, which works with
// an installation token's metadata read): PermissionAdmin, PermissionWrite,
// PermissionRead or PermissionNone. On a public repository every GitHub
// account reads as "read"; an App's own bot reads as "none". A 404 is
// ErrNoSuchUser. Pass the result to CanWrite; never treat "any permission"
// as authority.
func (c *Client) CollaboratorPermission(ctx context.Context, repo Repo, login string) (string, error) {
	got, err := c.CollaboratorAccess(ctx, repo, login)
	return got.Permission, err
}

// CollaboratorAccess is CollaboratorPermission with the account GitHub
// resolved login to: the same one request, whose answer carries the user
// for every existing account, collaborator or not, under its canonical
// login (the lookup is case-insensitive). An answer without a user, or with
// a negative id, has ID zero, which is no account. A 404 is ErrNoSuchUser.
func (c *Client) CollaboratorAccess(ctx context.Context, repo Repo, login string) (Collaborator, error) {
	if login == "" {
		return Collaborator{}, fmt.Errorf("ghclient: permission on %s: empty login: %w", repo, ErrNoSuchUser)
	}
	// go-github formats the login into the path unescaped; escaping it
	// keeps a "/" (or a bot's brackets) from reshaping the request.
	lvl, _, err := c.gh.Repositories.GetPermissionLevel(ctx, repo.Owner, repo.Name, url.PathEscape(login))
	if err != nil {
		if IsNotFound(err) {
			return Collaborator{}, fmt.Errorf("ghclient: permission of %s on %s: %w: %w", login, repo, ErrNoSuchUser, err)
		}
		return Collaborator{}, fmt.Errorf("ghclient: permission of %s on %s: %w", login, repo, err)
	}
	u := lvl.GetUser()
	return Collaborator{Login: u.GetLogin(), ID: max(u.GetID(), 0), Permission: lvl.GetPermission()}, nil
}

// CanWrite reports whether permission grants write access or more: admin,
// maintain or write. Read is never enough — a public repository grants it
// to every GitHub account — and anything unrecognised is refused.
func CanWrite(permission string) bool {
	switch permission {
	case PermissionAdmin, PermissionMaintain, PermissionWrite:
		return true
	}
	return false
}
