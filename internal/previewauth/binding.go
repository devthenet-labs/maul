// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"errors"
	"strconv"
	"strings"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// SubresourcePreviews is the virtual subresource of projects an access review
// for viewing a Project's previews names; it matches web/authz's.
const SubresourcePreviews = "previews"

var (
	// ErrNoPreview means no live Preview holds the address, or it holds
	// another slot: a 404 at authorize, invalid_grant at the token endpoint.
	ErrNoPreview = errors.New("previewauth: no live preview at this address")
	// ErrNotBound means a token was issued for another client, slot, Preview
	// or host label.
	ErrNotBound = errors.New("previewauth: token is not bound to this preview")
	// ErrNoProject means the Preview names no Project, so no access review
	// can be made for it and the viewer is refused.
	ErrNoProject = errors.New("previewauth: preview names no project")
)

// View is what the relay needs to know about the Preview at a host label.
type View struct {
	UID     string
	Label   string
	Project string
	Slot    int
	// Live: not being deleted, in a phase that holds or awaits its slot's
	// host (Queued, Deploying, Ready), with a slot assigned.
	Live bool
	// Routes is what the Preview serves, from status.serving: set only on a
	// live View whose snapshot was written for this very Preview (its UID),
	// never from spec. The gateway proxies only to these.
	Routes []Route
}

// Route is one served component: requests whose path falls under Path go to
// Service's Port in the Preview's slot namespace.
type Route struct {
	Path    string
	Service string
	Port    int32
}

// Upstream is the route's in-cluster address in slot's namespace: by DNS
// name, so the gateway needs no Service or Endpoint read.
func (r Route) Upstream(slot int) string {
	return r.Service + "." + v1alpha1.PreviewSlotNamespace(int32(slot)) + ".svc:" + strconv.Itoa(int(r.Port))
}

// Pick is the route a request path goes to (the longest matching prefix,
// v1alpha1.PreviewRoute), or false when none matches.
func (v View) Pick(path string) (Route, bool) {
	prefixes := make([]string, len(v.Routes))
	for i, r := range v.Routes {
		prefixes[i] = r.Path
	}
	i := v1alpha1.PreviewRoute(prefixes, path)
	if i < 0 {
		return Route{}, false
	}
	return v.Routes[i], true
}

// routesOf reads the serving snapshot of p, refusing the whole snapshot when
// it was written for another UID or any component is not a well-formed
// Service name, path and port.
func routesOf(p *v1alpha1.Preview) []Route {
	s := p.Status.Serving
	if s == nil || s.UID == "" || s.UID != p.UID || len(s.Components) == 0 ||
		len(s.Components) > v1alpha1.MaxPreviewComponents {
		return nil
	}
	out := make([]Route, 0, len(s.Components))
	for _, c := range s.Components {
		if !validServiceName(c.Service) || c.Port < 1 || c.Port > 65535 || !strings.HasPrefix(c.Path, "/") ||
			len(c.Path) > 128 {
			return nil
		}
		out = append(out, Route{Path: c.Path, Service: c.Service, Port: c.Port})
	}
	return out
}

// validServiceName is a DNS-1035 label, which a Service name is.
func validServiceName(n string) bool {
	return n != "" && len(n) <= maxLabel && n[0] >= 'a' && n[0] <= 'z' && n[len(n)-1] != '-' &&
		strings.Trim(n, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}

// ViewOf projects p. A Preview whose name is not its host label, or that has
// no slot, is never Live.
func ViewOf(p *v1alpha1.Preview) View {
	v := View{UID: string(p.UID), Label: p.Spec.HostLabel, Project: p.Spec.Project, Slot: -1}
	if p.Status.Slot != nil {
		v.Slot = int(*p.Status.Slot)
	}
	switch p.Status.Phase {
	case v1alpha1.PreviewQueued, v1alpha1.PreviewDeploying, v1alpha1.PreviewReady:
		v.Live = p.DeletionTimestamp == nil && p.Status.Slot != nil && p.Name == p.Spec.HostLabel &&
			v.UID != "" && v.Slot >= 0 && v.Slot < MaxSlots
	}
	if v.Live {
		v.Routes = routesOf(p)
	}
	return v
}

// Admit is the authorize-time check: a live Preview holds label in slot. A
// Preview in another slot is the same answer as none, so a slot's ALB can
// never be used to sign in to another slot's host.
func Admit(label string, slot int, v View) error {
	if !v.Live || v.UID == "" || v.Label != label || v.Slot != slot {
		return ErrNoPreview
	}
	return nil
}

// Matches is the check every token presented back to the relay passes: the
// Preview it was issued for is still live at the same label and in the same
// slot, with the same UID. A new Preview at a reused label or slot has a new
// UID, so no token from its predecessor works for it.
func (b Bound) Matches(v View) error {
	if b.validBound() != nil {
		return ErrNotBound
	}
	if !v.Live {
		return ErrNoPreview
	}
	if v.UID != b.UID || v.Label != b.Label || v.Slot != b.Slot {
		return ErrNotBound
	}
	return nil
}

// IssuedTo checks that the token was issued to the authenticated client.
func (b Bound) IssuedTo(clientID string) error {
	if b.Client != clientID || b.Client != ClientID(b.Slot) {
		return ErrNotBound
	}
	return nil
}

// AccessReview is the input of the authorisation decision: may this viewer
// get the previews subresource of this Project.
type AccessReview struct {
	Username    string
	Groups      []string
	Project     string
	Subresource string
}

// ReviewFor builds the access review for id viewing v's previews. A Preview
// that names no Project (one written before Previews recorded it) is
// ErrNoProject: the viewer is refused rather than reviewed against nothing.
func ReviewFor(id Identity, v View) (AccessReview, error) {
	if v.Project == "" {
		return AccessReview{}, ErrNoProject
	}
	if err := id.Validate(); err != nil {
		return AccessReview{}, err
	}
	groups := append([]string(nil), id.Groups...)
	return AccessReview{Username: id.Username, Groups: groups, Project: v.Project, Subresource: SubresourcePreviews}, nil
}
