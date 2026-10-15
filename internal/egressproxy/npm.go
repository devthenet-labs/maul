// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// npmCorgi is the Accept npm sends for a packument (the abbreviated
// "corgi" document first, the full one after).
const npmCorgi = "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*"

// npmAccept is the Accept table of §2.3: npm's own corgi value or
// application/json pass; anything else becomes application/json.
func npmAccept(in string) string {
	if in == npmCorgi || in == "application/json" {
		return in
	}
	return "application/json"
}

// admitNPM judges an npm request below /r/<registry> (sub is the escaped
// remainder, leading slash included): the grammar of netprofile.NPMPath on
// the escaped path, the base-path-prefixed tarball shape npm's
// replace-registry-host=always produces, the version gate, and the grant's
// package policy.
func (s *Server) admitNPM(g netprofile.Grant, rt *registryRoute, w http.ResponseWriter, r *http.Request, sub string,
	e *auditEntry,
) (regReq, bool) {
	if len(r.URL.EscapedPath()) > MaxPathLen {
		refuseFor(w, e, colNPM, http.StatusForbidden, OutcomeBadPath, "too_long", "egress-proxy: path too long")
		return regReq{}, false
	}
	nr, err := parseNPM(rt.base.EscapedPath(), sub, g.AllowPrerelease)
	if err != nil {
		if errors.Is(err, netprofile.ErrPrerelease) {
			if pre, perr := parseNPM(rt.base.EscapedPath(), sub, true); perr == nil {
				e.kind, e.reg.pkg, e.version = string(pre.Kind), pre.FullName(), pre.Version
			}
			refuseFor(w, e, colNPM, http.StatusForbidden, OutcomePrerelease, ReasonVersion,
				fmt.Sprintf("egress-proxy: %s; the grant of registry %s admits release versions (x.y.z) only",
					clip(err.Error()), g.Name))
			return regReq{}, false
		}
		refuseFor(w, e, colNPM, http.StatusForbidden, OutcomeBadPath, "grammar",
			"egress-proxy: not an npm packument or tarball path this route serves: "+clip(err.Error()))
		return regReq{}, false
	}
	full := nr.FullName()
	e.kind, e.reg.pkg, e.version = string(nr.Kind), full, nr.Version
	if !g.Allows(full) {
		refuseFor(w, e, colNPM, http.StatusForbidden, OutcomeNotAllowed, "package",
			fmt.Sprintf("egress-proxy: package %s is not allowed by the grant of registry %s", clip(full), g.Name))
		return regReq{}, false
	}
	target, err := joinBase(rt.base, nr.Escaped)
	if err != nil {
		refuseFor(w, e, colNPM, http.StatusForbidden, OutcomeBadPath, "escaping",
			"egress-proxy: path is not in a form this route forwards")
		return regReq{}, false
	}
	req := regReq{target: target, origin0: true, what: full}
	if nr.Kind == netprofile.NPMPackument {
		req.accept, req.limit, req.scan = npmAccept(r.Header.Get("Accept")), maxPackumentBytes, true
	} else {
		req.accept, req.limit = "*/*", rt.maxArtifact
	}
	return req, true
}

// parseNPM parses sub, first as the base-path-prefixed tarball shape
// (B/<tarball>, stripped once) and then as it stands. basePath is the
// registry URL's escaped path.
func parseNPM(basePath, sub string, allowPrerelease bool) (netprofile.NPMRequest, error) {
	if b := strings.TrimSuffix(basePath, "/"); b != "" {
		if stripped, ok := strings.CutPrefix(sub, b+"/"); ok {
			nr, err := netprofile.NPMPath("/"+stripped, allowPrerelease)
			switch {
			case err == nil && nr.Kind == netprofile.NPMTarball:
				return nr, nil
			case errors.Is(err, netprofile.ErrPrerelease):
				return netprofile.NPMRequest{}, err
			}
		}
	}
	return netprofile.NPMPath(sub, allowPrerelease)
}
