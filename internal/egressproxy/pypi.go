// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/egressproxy/pyindex"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// pyOrigins are the hosts a registry's simple pages may link files to:
// origin 0 is the registry's own, its files under the index URL's path
// less its final simple/ (P); 1 to n are its fileHosts, never sent the
// credential.
func pyOrigins(rt *registryRoute) []pyindex.Origin {
	host := strings.TrimSuffix(strings.ToLower(rt.base.Host), ":443")
	prefix := strings.TrimSuffix(strings.TrimSuffix(rt.base.Path, "/"), "/simple")
	out := make([]pyindex.Origin, 0, 1+len(rt.reg.FileHosts))
	out = append(out, pyindex.Origin{Host: host, Prefix: prefix + "/"})
	for _, h := range rt.reg.FileHosts {
		out = append(out, pyindex.Origin{Host: h})
	}
	return out
}

// admitPyPI judges a pip request below /r/<registry> (sub is the escaped
// remainder, leading slash included): a simple page /simple/<normalized
// project>/, rewritten on the way back, or a file /f/<origin>/<path>
// judged statelessly by the same rules a page's links pass.
func (s *Server) admitPyPI(g netprofile.Grant, rt *registryRoute, w http.ResponseWriter, r *http.Request,
	sub string, e *auditEntry,
) (regReq, bool) {
	bad := func(reason, msg string) (regReq, bool) {
		refuseFor(w, e, colPyPI, http.StatusForbidden, OutcomeBadPath, reason, "egress-proxy: "+msg)
		return regReq{}, false
	}
	if len(r.URL.EscapedPath()) > MaxPathLen {
		return bad("too_long", "path too long")
	}
	origins := pyOrigins(rt)
	if rest, ok := strings.CutPrefix(sub, "/simple/"); ok {
		project, ok := strings.CutSuffix(rest, "/")
		if !ok || !netprofile.PyNormalized(project) {
			return bad("grammar", "not a simple page this route serves: /simple/<normalized project>/")
		}
		e.kind, e.reg.pkg = kindPage, project
		if !g.Allows(project) {
			return s.pyNotAllowed(w, g, project, e)
		}
		target, err := joinBase(rt.base, "/"+project+"/")
		if err != nil {
			return bad("escaping", "path is not in a form this route forwards")
		}
		var allow func(string) bool
		if g.PackagePolicy != netprofile.PackagePolicyRegistry {
			allow = func(p string) bool { return p == project }
		}
		route := registryPrefix + g.Name
		return regReq{target: target, origin0: true, accept: "text/html", limit: maxPyPageBytes, scan: true,
			what: project,
			page: func(body []byte, final *url.URL, e *auditEntry) ([]byte, error) {
				out, st, err := pyindex.Rewrite(body, final, origins, route, project, allow)
				e.reg.rewritten, e.reg.dropped = st.Rewritten, st.Dropped
				return out, err
			}}, true
	}
	k, raw, err := pyindex.ParseFileRoute(sub)
	if err != nil {
		return bad("grammar", "not a simple page or file path this route serves")
	}
	file, target, err := pyindex.Judge(k, raw, origins)
	if err != nil {
		return bad("grammar", "not a distribution file path this route serves: "+clip(err.Error()))
	}
	e.kind, e.reg.pkg, e.version = kindFile, file.Dist.Project, file.Dist.Version
	if file.Dist.Metadata {
		e.kind = kindMetadata
	}
	if !g.Allows(file.Dist.Project) {
		return s.pyNotAllowed(w, g, file.Dist.Project, e)
	}
	req := regReq{target: target, origin0: k == 0, what: file.Dist.Project}
	if file.Dist.Metadata {
		req.limit, req.scan = maxPyMetadataBytes, true
	} else {
		req.limit = rt.maxArtifact
	}
	return req, true
}

// pyNotAllowed is the allowlist refusal.
func (s *Server) pyNotAllowed(w http.ResponseWriter, g netprofile.Grant, project string, e *auditEntry) (regReq, bool) {
	refuseFor(w, e, colPyPI, http.StatusForbidden, OutcomeNotAllowed, "package",
		fmt.Sprintf("egress-proxy: project %s is not allowed by the grant of registry %s", clip(project), g.Name))
	return regReq{}, false
}
