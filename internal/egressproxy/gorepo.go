// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/mod/module"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// admitGoRegistry judges a Go request below /r/<registry>: step 1's
// GOPROXY and checksum-database grammars on the same canonical-escaping
// rule, the grant's module allowlist and version gate, and for a checksum
// lookup the order of §2.5 of the step-2 spec.
func (s *Server) admitGoRegistry(p netprofile.Profile, g netprofile.Grant, rt *registryRoute, w http.ResponseWriter,
	r *http.Request, e *auditEntry,
) (regReq, bool) {
	if r.URL.RawPath != "" {
		refuse(w, e, http.StatusGone, OutcomeBadPath, "path is not in canonical escaping",
			"egress-proxy: path is not in canonical escaping")
		return regReq{}, false
	}
	if len(r.URL.Path) > MaxPathLen {
		refuse(w, e, http.StatusGone, OutcomeBadPath, "path too long", "egress-proxy: path too long")
		return regReq{}, false
	}
	rest := strings.TrimPrefix(r.URL.Path, registryPrefix+g.Name+"/")
	var (
		req Request
		err error
	)
	if db, ok := strings.CutPrefix(rest, sumdbPrefix); ok {
		req, err = ParseSumDB(db)
	} else {
		req, err = ParseGoProxy(rest)
	}
	if err != nil {
		var pe *PathError
		status, reason := http.StatusGone, err.Error()
		if errors.As(err, &pe) && pe.NotFound {
			status, reason = http.StatusNotFound, ReasonNotFound
		}
		refuse(w, e, status, OutcomeBadPath, reason, "egress-proxy: "+err.Error())
		return regReq{}, false
	}
	e.kind, e.module, e.version = string(req.Kind), req.Module, req.Version
	if req.Route == RouteSumDB {
		return s.judgeLookup(p, g, rt, w, req, e)
	}
	if !g.Allows(req.Module) {
		refuse(w, e, http.StatusGone, OutcomeNotAllowed, ReasonPrefix,
			fmt.Sprintf("egress-proxy: module path %s is not allowed by the grant of registry %s",
				clip(req.Module), g.Name))
		return regReq{}, false
	}
	if req.VersionGated() && !VersionAllowed(req.Version, g.AllowPseudoVersions) {
		refuse(w, e, http.StatusGone, OutcomeNotAllowed, ReasonVersion,
			fmt.Sprintf("egress-proxy: version %s of %s is not a tagged release the grant of registry %s admits",
				clip(req.Version), clip(req.Module), g.Name))
		return regReq{}, false
	}
	// The module-escaped path leaves as step 1's does: net/url's canonical
	// escaping, which is the go command's.
	target := outboundURL(rt.base, req.Upstream)
	limit := s.cfg.Limits.bodyCap(req)
	if req.Kind == KindZip {
		limit = min(limit, rt.maxArtifact)
	}
	return regReq{target: target, origin0: true, limit: limit, scan: req.Kind != KindZip,
		what: req.Module}, true
}

// judgeLookup answers a checksum-database request on a go registry route.
// supported is answered locally; latest and tile go to the public
// database; a lookup is refused for a private module, forwarded when the
// profile's public modules admit it (step 1's rule), or when the registry
// allows public lookups of what it serves (the grant's allowlist, or under
// packagePolicy registry a credentialed HEAD of the .mod answering 200),
// and otherwise refused checksum_not_public.
func (s *Server) judgeLookup(p netprofile.Profile, g netprofile.Grant, rt *registryRoute, w http.ResponseWriter,
	req Request, e *auditEntry,
) (regReq, bool) {
	sumdb := regReq{target: outboundURL(s.cfg.SumDB, req.Upstream), sumdb: true,
		limit: s.cfg.Limits.bodyCap(req), scan: true, what: req.Module}
	switch req.Kind {
	case KindSupported:
		return regReq{local: true}, true
	case KindLatest, KindTile:
		sumdb.what = SumDBName
		return sumdb, true
	}
	m, v := req.Module, req.Version
	if g.PrivateModulePrefixes().Allows(m) {
		refuse(w, e, http.StatusGone, OutcomePrivateModule, ReasonPrivateModule,
			fmt.Sprintf("egress-proxy: module path %s is private to registry %s and is never looked up in the "+
				"public checksum database", clip(m), g.Name))
		return regReq{}, false
	}
	if p.Route == netprofile.RouteGo && p.Modules.Allows(m) && VersionAllowed(v, p.AllowPseudoVersions) {
		return sumdb, true
	}
	if rt.reg.PublicChecksumLookups != nil && *rt.reg.PublicChecksumLookups && VersionAllowed(v, g.AllowPseudoVersions) {
		if g.PackagePolicy != netprofile.PackagePolicyRegistry {
			if g.Allows(m) {
				return sumdb, true
			}
		} else if probe, ok := modProbe(rt, m, v); ok {
			sumdb.probe = probe
			return sumdb, true
		}
	}
	refuse(w, e, http.StatusGone, OutcomeChecksumNotPublic, "", checksumNotPublicMsg(m, g.Name))
	return regReq{}, false
}

// modProbe is the registry's .mod URL for m@v.
func modProbe(rt *registryRoute, m, v string) (*url.URL, bool) {
	em, err := module.EscapePath(m)
	if err != nil {
		return nil, false
	}
	ev, err := module.EscapeVersion(v)
	if err != nil {
		return nil, false
	}
	return outboundURL(rt.base, em+"/@v/"+ev+".mod"), true
}

// checksumNotPublicMsg is the 410 body of a refused lookup.
func checksumNotPublicMsg(m, registry string) string {
	return fmt.Sprintf("egress-proxy: checksum_not_public: %s was served by registry %s and the operator has not "+
		"said it may be checked against the public checksum database (add it to the registry's privateModules if "+
		"it is in-house, or set publicChecksumLookups)", clip(m), registry)
}
