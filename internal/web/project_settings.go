// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/web/auth"
	"github.com/devthenet-labs/maul/internal/web/authz"
)

// The Project settings page (statusServer.projectSettings.enabled): an
// approver of a Project changes the settings intent-controller reads from
// it (limits, models, the evaluator's switches, the approver list where the
// operator allows it) without kubectl. This file is its read side: who may
// see a Project's settings is who may see its intents (the intents views'
// tier), and what the page shows, bounds and lets the viewer save is the
// pure core's (internal/projectsettings.View) of the Project read uncached,
// since the cache strips the managedFields the page names other managers
// from. Whether the viewer may save is never a Kubernetes grant: it is the
// core's Gate, an approver GitHub gives write access, matched by the
// numeric GitHub id the session carries.

// SettingsReadOnly is the view's reason when the page is served with no
// write path (ProjectSettingsOptions.ReadOnly), in place of the gate's.
const SettingsReadOnly = "read-only"

// ProjectSettingsOptions turn the Project settings page on. It needs the
// intents views (WithIntents), whose visibility it shares; without them its
// routes are not mounted. Turning it on hardens the browser envelope, as
// the intents views do.
type ProjectSettingsOptions struct {
	// Policy is the operator's: switches, locks, ceilings and floors
	// (statusServer.projectSettings, projectsettings.ParsePolicy).
	Policy projectsettings.Policy
	// Projects are the Projects the page is for, the status server's patch
	// grant's resourceNames (statusServer.projectSettings.projects); empty
	// is every Project. Any other has no page.
	Projects []string
	// ReadOnly serves the page with no write path: nobody may save
	// (--intents-dev-insecure, auth mode none, where no session carries a
	// GitHub id that could be trusted).
	ReadOnly bool
}

// projectSettingsState is the server's settings side; nil while the page is
// off.
type projectSettingsState struct {
	policy   projectsettings.Policy
	projects []string
	readOnly bool
}

// WithProjectSettings enables the Project settings page.
func (s *Server) WithProjectSettings(o ProjectSettingsOptions) *Server {
	s.settings = &projectSettingsState{policy: o.Policy, projects: slices.Clone(o.Projects), readOnly: o.ReadOnly}
	return s
}

// registerProjectSettings mounts the page's routes.
func (s *Server) registerProjectSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{name}/settings", s.handleProjectSettings)
}

// settingsFor reports whether the page is served for the named Project.
func (s *Server) settingsFor(project string) bool {
	st := s.settings
	return st != nil && s.intents != nil && (len(st.projects) == 0 || slices.Contains(st.projects, project))
}

// visibleProject loads the named Project uncached, for a caller who may see
// its intents and for a Project the page is for. A Project that does not
// exist, one the caller may not see and one the page is not for all return
// errNotVisible. Existence is asked of the cache first, as every intents
// route does, so a name the caller made up costs no access review; the
// uncached read comes only once the caller's tier admits it.
func (s *Server) visibleProject(r *http.Request, id auth.Identity, name string) (*v1alpha1.Project, error) {
	if len(validation.IsDNS1123Subdomain(name)) > 0 || !s.settingsFor(name) {
		return nil, errNotVisible
	}
	ctx := r.Context()
	key := types.NamespacedName{Namespace: s.namespace, Name: name}
	if err := s.client.Get(ctx, key, &v1alpha1.Project{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotVisible
		}
		return nil, err
	}
	tiers, err := s.intents.projects.Tiers(ctx, id, []string{name})
	if err != nil {
		return nil, err
	}
	if tiers[name] == authz.TierNone {
		return nil, errNotVisible
	}
	var p v1alpha1.Project
	if err := s.reader.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotVisible
		}
		return nil, err
	}
	return &p, nil
}

// handleProjectSettings serves one Project's settings view.
func (s *Server) handleProjectSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	p, err := s.visibleProject(r, id, name)
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return
	case err != nil:
		s.log.LogAttrs(r.Context(), slog.LevelError, "load project settings", slog.String("project", name),
			slog.Any("error", err))
		http.Error(w, "failed to load the project's settings", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.settingsView(p, id))
}

// settingsView is the core's view of p for id, with what the server adds:
// the read-only posture, the conditions in fixed wording, and the
// operator's own words made fit to show.
func (s *Server) settingsView(p *v1alpha1.Project, id auth.Identity) projectsettings.ProjectSettingsView {
	v := projectsettings.View(p, s.settings.policy, id.GitHubID, s.now())
	if s.settings.readOnly {
		v.CanEdit, v.Reason = false, SettingsReadOnly
	}
	v.Conditions = settingsConditions(p)
	fitSettingsView(&v)
	return v
}

// settingsConditions are the Project's conditions the page shows, in
// intentview's order and fixed wording: never a condition's message, which
// names repositories, errors and flags.
func settingsConditions(p *v1alpha1.Project) []projectsettings.ProjectSettingsCondition {
	out := []projectsettings.ProjectSettingsCondition{}
	for _, typ := range intentview.ProjectConditions {
		c := meta.FindStatusCondition(p.Status.Conditions, typ)
		if c == nil {
			continue
		}
		out = append(out, projectsettings.ProjectSettingsCondition{Type: typ, Status: string(c.Status),
			Reason: intentview.Text(c.Reason, maxSettingsName), Text: intentview.ProjectConditionText(typ, c.Reason)})
	}
	return out
}

// Bounds of the operator's words in a settings view.
const (
	maxSettingsName = 128 // a label, a manager's name, a reason
	maxSettingsRef  = 512 // a URL, an image repository, a path
)

// fitSettingsView makes the operator's own words in v fit to show:
// characters that render as nothing shown as their code point, and a bound.
// The settings document, the approver logins and the repository names are
// left exactly as stored (the CRD holds the last two to a grammar): the
// page posts the document back, and a changed byte would be a change.
func fitSettingsView(v *projectsettings.ProjectSettingsView) {
	f := &v.Facts
	f.IntentRepository = intentview.Text(f.IntentRepository, maxSettingsRef)
	f.TriggerLabel = intentview.Text(f.TriggerLabel, maxSettingsName)
	f.ApproveLabel = intentview.Text(f.ApproveLabel, maxSettingsName)
	for i := range f.Repositories {
		repo := &f.Repositories[i]
		repo.URL = intentview.Text(repo.URL, maxSettingsRef)
		repo.PreviewPath = intentview.Text(repo.PreviewPath, maxSettingsRef)
		repo.PreviewImage = intentview.Text(repo.PreviewImage, maxSettingsRef)
		repo.PreviewReadinessPath = intentview.Text(repo.PreviewReadinessPath, maxSettingsRef)
		fitManagers(repo.ClassManagers)
	}
	for i := range v.Fields {
		fitManagers(v.Fields[i].Managers)
	}
	for i := range v.Approvers {
		fitManagers(v.Approvers[i].Managers)
	}
	if e := v.LastEdit; e != nil {
		e.Login = intentview.Text(e.Login, maxSettingsName)
	}
}

func fitManagers(ms []projectsettings.ProjectSettingsManager) {
	for i := range ms {
		ms[i].Name = intentview.Text(ms[i].Name, maxSettingsName)
	}
}
