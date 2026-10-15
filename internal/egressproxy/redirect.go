// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"net/url"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// MaxRedirects is the most redirect hops a registry request follows.
const MaxRedirects = 3

// isRedirect reports whether status is a 3xx the proxy may follow.
func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// redirectVerdict is the judgement of one Location.
type redirectVerdict struct {
	// next is the URL to fetch, presigned query kept; nil when refused.
	next *url.URL
	// host is the target's host, for the audit line ("" when the Location
	// does not parse).
	host string
	// outcome and reason refuse the hop when outcome is set.
	outcome, reason string
}

// judgeRedirect decides whether a 3xx's Location may be followed (§2.6 of
// the step-2 spec): resolved against the URL that answered, it must be
// https with no userinfo, on the default port, its host listed in the
// registry's redirectHosts (exactly, or strictly below a *.suffix pattern),
// and its host must pass the hard deny table again. A hop is always
// fetched afresh, without the credential, on the public dial policy.
func judgeRedirect(r netprofile.Registry, from *url.URL, location string, denyHosts []string) redirectVerdict {
	if location == "" {
		return redirectVerdict{outcome: OutcomeRedirectRefused, reason: "no_location"}
	}
	loc, err := url.Parse(location)
	if err != nil {
		return redirectVerdict{outcome: OutcomeRedirectRefused, reason: "bad_location"}
	}
	next := from.ResolveReference(loc)
	next.Fragment, next.RawFragment = "", ""
	host := strings.TrimSuffix(strings.ToLower(next.Hostname()), ".")
	v := redirectVerdict{host: host}
	switch {
	case next.Scheme != "https":
		v.outcome, v.reason = OutcomeRedirectRefused, "not_https"
	case next.User != nil:
		v.outcome, v.reason = OutcomeRedirectRefused, "userinfo"
	case next.Port() != "" && next.Port() != "443":
		v.outcome, v.reason = OutcomeRedirectRefused, "port"
	case !redirectListed(r.RedirectHosts, host):
		v.outcome, v.reason = OutcomeRedirectRefused, "unlisted_host"
	case netprofile.DeniedHost(host, netprofile.DenyContext{Hosts: denyHosts}) != nil:
		v.outcome, v.reason = OutcomeDeniedHost, "redirect_host"
	default:
		v.next = next
	}
	return v
}

// redirectListed reports whether host is one of the registry's redirect
// hosts, or strictly below a *.suffix pattern's suffix.
func redirectListed(list []string, host string) bool {
	if host == "" {
		return false
	}
	for _, h := range list {
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == h {
			return true
		}
	}
	return false
}
