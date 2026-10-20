// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"strconv"
	"strings"
)

// The preview routing helpers. preview-controller names each component's
// Service and renders the slot Ingress's Prefix paths with them, and records
// the same names and paths in status.serving; the preview gateway routes a
// request from that snapshot with PreviewRoute. One definition of each, so
// the Ingress a legacy slot renders and the gateway cannot route a request
// differently (a test in the preview-controller pins that they agree).

// PreviewServicePort is the port every component's Service exposes, whatever
// the component's container port: the Service maps it to the container's
// named port "http".
const PreviewServicePort int32 = 80

// PreviewSlotNamespace is slot n's fixed namespace, maul-preview-<n>: where
// preview-controller renders the slot's objects and where the gateway
// addresses its Services.
func PreviewSlotNamespace(n int32) string { return "maul-preview-" + strconv.Itoa(int(n)) }

// PreviewComponentServiceName names component i's Service (and Deployment)
// in the Preview's slot. The first component keeps preview-<preview>, the
// name every single-component Preview has always rendered; each further one
// is preview-<preview>-<component>.
func PreviewComponentServiceName(p *Preview, i int) string {
	name := "preview-" + p.Name
	if i == 0 {
		return name
	}
	return name + "-" + p.Spec.Components[i].Name
}

// PreviewPathMatches reports whether a request path falls under a component's
// path prefix with Ingress pathType Prefix semantics: the prefix and the path
// are compared element by element, split on "/", so /api matches /api,
// /api/ and /api/v1 but never /apix; a trailing slash on either is ignored;
// and "/" matches every path. A request path that does not start with "/"
// matches nothing, and an empty one is "/".
func PreviewPathMatches(prefix, path string) bool {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") || !strings.HasPrefix(prefix, "/") {
		return false
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// PreviewRoute returns the index of the path prefix a request path routes to:
// the longest of prefixes that matches it (PreviewPathMatches), as an Ingress
// picks among its Prefix paths, or -1 when none does. Equal-length matches
// cannot both match one path unless they are the same prefix, which a
// Preview's components never repeat; the first is taken.
func PreviewRoute(prefixes []string, path string) int {
	best, bestLen := -1, -1
	for i, prefix := range prefixes {
		n := len(strings.TrimRight(prefix, "/"))
		if n > bestLen && PreviewPathMatches(prefix, path) {
			best, bestLen = i, n
		}
	}
	return best
}
