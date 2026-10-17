// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
)

// The relay's claim prefixes: a viewer binding's subjects must carry them,
// since the relay's access reviews run for exactly the prefixed user and
// groups.
const (
	keyAuthUsernamePrefix = "PATCHY_PREVIEW_AUTH_USERNAME_PREFIX"
	keyAuthGroupsPrefix   = "PATCHY_PREVIEW_AUTH_GROUPS_PREFIX"
)

// viewerRoleSuffix names the chart's ClusterRole granting get on
// projects/previews: <fullname>-preview-viewer.
const viewerRoleSuffix = "-preview-viewer"

// viewers checks that someone may view previews: a RoleBinding in the
// release namespace, or a ClusterRoleBinding, binds the chart's
// preview-viewer ClusterRole to at least one subject the relay can sign in
// (a User or Group carrying the relay's claim prefix). The relay's access
// review is for someone else, so no SelfSubjectAccessReview can stand in:
// the check reads the bindings. With none, every sign-in is refused 403
// unless another role grants get on projects/previews, which this cannot
// see, so that is a WARN rather than a FAIL.
func (c *Checker) viewers(ctx context.Context, s Settings) {
	switch {
	case s.Auth.Err != nil:
		c.add(CheckPreviewViewers, checkreport.Skip, "cannot read the controllers' settings: %v", s.Auth.Err)
		return
	case !s.Auth.Found():
		c.add(CheckPreviewViewers, checkreport.Skip, "preview sign-in is off, so nobody views previews through it")
		return
	}
	base, ok := strings.CutSuffix(s.Auth.Name, relayConfigSuffix)
	if !ok {
		c.add(CheckPreviewViewers, checkreport.Skip, "the relay's ConfigMap %q is not named <release>%s, so the "+
			"viewer ClusterRole cannot be found", s.Auth.Name, relayConfigSuffix)
		return
	}
	role := base + viewerRoleSuffix
	userPrefix, groupPrefix := s.Auth.Data[keyAuthUsernamePrefix], s.Auth.Data[keyAuthGroupsPrefix]
	var granting []string
	var all []rbacv1.Subject
	judge := func(kind, name string, ref rbacv1.RoleRef, subjects []rbacv1.Subject) {
		if ref.Kind != "ClusterRole" || ref.Name != role {
			return
		}
		all = append(all, subjects...)
		if v := judgeViewers(subjects, userPrefix, groupPrefix); v.teams+v.users > 0 {
			granting = append(granting, kind+" "+name)
		}
	}
	var rbs rbacv1.RoleBindingList
	rbErr := c.cfg.Reader.List(ctx, &rbs, client.InNamespace(c.cfg.Namespace))
	for _, rb := range rbs.Items {
		judge("RoleBinding", rb.Name, rb.RoleRef, rb.Subjects)
	}
	var crbs rbacv1.ClusterRoleBindingList
	crbErr := c.cfg.Reader.List(ctx, &crbs)
	for _, crb := range crbs.Items {
		judge("ClusterRoleBinding", crb.Name, crb.RoleRef, crb.Subjects)
	}
	v := judgeViewers(all, userPrefix, groupPrefix)
	switch {
	case v.teams+v.users > 0:
		c.add(CheckPreviewViewers, checkreport.Pass, "%s bind%s ClusterRole %s to %d team%s and %d user%s the "+
			"relay can sign in%s", strings.Join(granting, ", "), plural(len(granting), "s", ""), role, v.teams,
			plural(v.teams, "", "s"), v.users, plural(v.users, "", "s"), v.ignoredNote())
	case rbErr != nil || crbErr != nil:
		c.add(CheckPreviewViewers, checkreport.Skip, "no readable binding grants ClusterRole %s to a viewer, "+
			"and not every binding could be listed (%v)%s", role, errors.Join(rbErr, crbErr), v.ignoredNote())
	default:
		c.add(CheckPreviewViewers, checkreport.Warn, "nothing binds ClusterRole %s to a viewer the relay can "+
			"sign in, so unless another role grants get on projects/previews in namespace %s, every sign-in is "+
			"refused 403: set previewAuth.viewers.teams or users%s", role, c.cfg.Namespace, v.ignoredNote())
	}
}

// viewerCount is what a binding's subjects come to: the teams and users the
// relay can sign in, and the subjects it never can (a ServiceAccount, or a
// name without the claim prefix the relay maps an ID token through).
type viewerCount struct {
	teams, users int
	ignored      []string
}

// judgeViewers counts subjects, each distinct one once. An empty prefix
// admits any name, as the relay then maps the claim as it is.
func judgeViewers(subjects []rbacv1.Subject, userPrefix, groupPrefix string) viewerCount {
	var v viewerCount
	seen := map[rbacv1.Subject]bool{}
	for _, s := range subjects {
		key := rbacv1.Subject{Kind: s.Kind, Name: s.Name, Namespace: s.Namespace}
		if seen[key] {
			continue
		}
		seen[key] = true
		switch {
		case s.Kind == rbacv1.GroupKind && strings.HasPrefix(s.Name, groupPrefix) && s.Name != groupPrefix:
			v.teams++
		case s.Kind == rbacv1.UserKind && strings.HasPrefix(s.Name, userPrefix) && s.Name != userPrefix:
			v.users++
		default:
			v.ignored = append(v.ignored, s.Kind+" "+s.Name)
		}
	}
	slices.Sort(v.ignored)
	return v
}

// ignoredNote names the subjects the relay can never sign in, for a reason.
func (v viewerCount) ignoredNote() string {
	if len(v.ignored) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s never sign%s in: a ServiceAccount, or a name without the relay's claim prefix)",
		strings.Join(v.ignored, ", "), plural(len(v.ignored), "s", ""))
}

// plural is one when n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
