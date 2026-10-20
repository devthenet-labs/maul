// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/usageview"
	"github.com/devthenet-labs/maul/internal/web/auth"
)

// The usage views (statusServer.usage.enabled; design L2, Dashboard): a
// signed-in person's own usage, and everyone's for whoever holds get on the
// virtual subresource usage/all. A person is their numeric GitHub id, which
// only a Dex sign-in through the GitHub connector carries
// (auth.Identity.GitHubID); any other identity is refused, so nobody is ever
// shown usage by a login that could have changed hands. Read only: every
// route is a GET over the UsageRollups the run controllers write.

// UsageAllResource and UsageAllSubresource name the admin view's grant: get
// on usage/all in the release namespace. Virtual: nothing on the API server
// serves it, so RBAC granting it is read only by the status server's access
// reviews.
const (
	UsageAllResource    = "usage"
	UsageAllSubresource = "all"
)

// UsageAllGranter answers whether an identity may see everyone's usage;
// authz.ResourceReviewer (NewSubresourceReviewer on usage/all) is the
// SubjectAccessReview implementation.
type UsageAllGranter interface {
	Allowed(ctx context.Context, id auth.Identity, verb string) (bool, error)
}

// UsageOptions turn on the usage views. Turning them on also hardens the
// browser envelope of the whole server, as the intents views do.
type UsageOptions struct {
	// All gates the admin view. Required.
	All UsageAllGranter
}

// usageState is the server's usage side; nil while the views are off.
type usageState struct {
	all UsageAllGranter
}

// WithUsage enables the usage views.
func (s *Server) WithUsage(o UsageOptions) *Server {
	s.usage = &usageState{all: o.All}
	return s
}

// UsageUserIndex indexes UsageRollups by spec.scope.user, so a person's own
// report reads only their scopes from the cache.
const UsageUserIndex = "maul.web/usage-user"

// RegisterUsageIndexes installs the usage views' index on the manager's
// cache; call it before the manager starts, and only with the views on (the
// UsageRollup informer it implies needs the usage RBAC).
func RegisterUsageIndexes(ctx context.Context, idx client.FieldIndexer) error {
	if err := idx.IndexField(ctx, &v1alpha1.UsageRollup{}, UsageUserIndex, UsageUserIndexer); err != nil {
		return fmt.Errorf("index usage rollups by user: %w", err)
	}
	return nil
}

// UsageUserIndexer extracts a rollup's user key.
func UsageUserIndexer(obj client.Object) []string {
	if r, ok := obj.(*v1alpha1.UsageRollup); ok && r.Spec.Scope.User != "" {
		return []string{r.Spec.Scope.User}
	}
	return nil
}

// UsageAccess is what the signed-in caller may see of the usage views, so
// the page can say why rather than fail. Mirrors ui/src/types.ts.
type UsageAccess struct {
	// Name is the caller's display name.
	Name string `json:"name"`
	// Self is whether the caller has an identity their own usage is kept
	// under (a GitHub sign-in); Reason says why not.
	Self bool `json:"self"`
	// All is whether the caller holds get on usage/all.
	All bool `json:"all"`
	// Reason explains a refused Self, "" otherwise.
	Reason string `json:"reason"`
}

// notGitHubReason is the one explanation for a caller without a GitHub id.
const notGitHubReason = "Usage is kept per GitHub account. Sign in with GitHub (through the GitHub connector) " +
	"to see it; if you already did, sign out and in again so your session carries your GitHub id."

// registerUsage mounts the usage routes.
func (s *Server) registerUsage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/usage/access", s.handleUsageAccess)
	mux.HandleFunc("GET /api/usage", s.handleUsageSelf)
	mux.HandleFunc("GET /api/usage/all", s.handleUsageAll)
}

// usageCaller identifies the caller and holds them to the GitHub identity,
// answering the request itself on failure.
func (s *Server) usageCaller(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	id, ok := s.identify(w, r)
	if !ok {
		return id, false
	}
	if id.GitHubID < 1 {
		http.Error(w, notGitHubReason, http.StatusForbidden)
		return id, false
	}
	return id, true
}

// handleUsageAccess serves what the caller may see.
func (s *Server) handleUsageAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	out := UsageAccess{Name: id.Display(), Self: id.GitHubID > 0}
	if !out.Self {
		out.Reason = notGitHubReason
		writeJSON(w, out)
		return
	}
	all, err := s.usage.all.Allowed(r.Context(), id, "get")
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "usage access review", slog.Any("error", err))
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return
	}
	out.All = all
	writeJSON(w, out)
}

// handleUsageSelf serves the caller's own report.
func (s *Server) handleUsageSelf(w http.ResponseWriter, r *http.Request) {
	id, ok := s.usageCaller(w, r)
	if !ok {
		return
	}
	days, err := usageDays(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Has("user") || r.URL.Query().Has("project") {
		// Only the admin view filters; refusing beats silently showing
		// the caller's own figures under someone else's name.
		http.Error(w, "the own-usage view takes no user or project; use /api/usage/all", http.StatusBadRequest)
		return
	}
	user := usageview.UserKey(id.GitHubID)
	var list v1alpha1.UsageRollupList
	if err := s.client.List(r.Context(), &list, client.InNamespace(s.namespace),
		client.MatchingFields{UsageUserIndex: user}, client.UnsafeDisableDeepCopy); err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "list usage rollups", slog.Any("error", err))
		http.Error(w, "failed to load usage", http.StatusInternalServerError)
		return
	}
	rep := usageview.Build(list.Items, usageview.Filter{User: user, Self: true}, s.now(), days)
	if rep.Login == "" {
		rep.Login = id.Display()
	}
	writeJSONGzip(w, r, rep)
}

// handleUsageAll serves everyone's report, or one person's or Project's,
// to a holder of get on usage/all.
func (s *Server) handleUsageAll(w http.ResponseWriter, r *http.Request) {
	id, ok := s.usageCaller(w, r)
	if !ok {
		return
	}
	allowed, err := s.usage.all.Allowed(r.Context(), id, "get")
	if err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "usage access review", slog.Any("error", err))
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Error(w, fmt.Sprintf("Permission denied. User %q may not get %s/%s in namespace %q.",
			id.Display(), UsageAllResource, UsageAllSubresource, s.namespace), http.StatusForbidden)
		return
	}
	days, err := usageDays(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	project := strings.TrimSpace(q.Get("project"))
	if len(project) > 253 {
		http.Error(w, "project: too long", http.StatusBadRequest)
		return
	}
	var list v1alpha1.UsageRollupList
	if err := s.client.List(r.Context(), &list, client.InNamespace(s.namespace),
		client.UnsafeDisableDeepCopy); err != nil {
		s.log.LogAttrs(r.Context(), slog.LevelError, "list usage rollups", slog.Any("error", err))
		http.Error(w, "failed to load usage", http.StatusInternalServerError)
		return
	}
	f := usageview.Filter{Project: project}
	if u := strings.TrimSpace(q.Get("user")); u != "" {
		if len(u) > 64 {
			http.Error(w, "user: too long", http.StatusBadRequest)
			return
		}
		key, err := usageview.ResolveUser(list.Items, u)
		if errors.Is(err, usageview.ErrUnknownUser) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.User = key
	}
	writeJSONGzip(w, r, usageview.Build(list.Items, f, s.now(), days))
}

// usageDays reads the days query parameter: absent is the default window,
// anything else must be a whole number of days within retention.
func usageDays(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("days")
	if raw == "" {
		return usageview.DefaultDays, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > usageview.MaxDays {
		return 0, fmt.Errorf("days: want a whole number from 1 to %d", usageview.MaxDays)
	}
	return n, nil
}
