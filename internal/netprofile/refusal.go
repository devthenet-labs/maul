// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import "strings"

// RefusalKind says what an egress-proxy policy refusal needs before the same
// request could succeed.
type RefusalKind int

// The policy refusals the job controllers act on.
const (
	// RefusalHost: the tunnel refused a host the profile does not list. The
	// operator adds it to the profile's hosts.
	RefusalHost RefusalKind = iota + 1
	// RefusalPackage: a dependency route refused a module or package by its
	// allowlist or grant (or the registry refused it by its own policy).
	// The operator allows it in the profile's modules or registry grant.
	RefusalPackage
	// RefusalNever: the proxy never opens it, whatever the profile says (a
	// forge, model API or control-plane host, an IP address, a port other
	// than 443, plain http). No operator change admits it; the request needs
	// another source.
	RefusalNever
)

// Refusal is a policy refusal read from the egress proxy's line.
type Refusal struct {
	Kind RefusalKind
	// Host is the refused host, for RefusalHost: a valid host name
	// (ValidHost), never anything else.
	Host string
}

// refusalMarker opens every line the egress proxy refuses a request with.
const refusalMarker = "egress-proxy: "

// policyPhrases are the egress proxy's own refusal phrases that depend only
// on the profile, its grants and the proxy's fixed deny table: the same
// request is refused again the same way until an operator changes the
// profile. They are the proxy's fixed wording (internal/egressproxy), pinned
// to it by a test there. The transient refusals (over a limit, shutting
// down, suspended, a credential or upstream unavailable, a dial error) are
// deliberately absent, and so are the version refusals (not a tagged or
// canonical version, a prerelease), which another version of the same
// dependency can avoid.
var policyPhrases = []struct {
	phrase string
	kind   RefusalKind
}{
	// The tunnel (step 3).
	{" is never tunnelled", RefusalNever},
	{"only port 443 may be tunnelled", RefusalNever},
	// A plain-http request through the proxy.
	{"absolute-form request URIs are refused", RefusalNever},
	// The public Go route (step 1).
	{" is not allowed by profile ", RefusalPackage},
	// The registry routes (step 2).
	{" is not allowed by the grant of registry ", RefusalPackage},
	{" holds no grant for this registry route", RefusalPackage},
	{": refused by the policy of registry ", RefusalPackage},
	{" is private to registry ", RefusalPackage},
	{"checksum_not_public: ", RefusalPackage},
}

// PolicyRefusal reads a line quoting the egress proxy's refusal ("403
// egress-proxy: pkg.example.com is not in profile py's hosts", as a package
// manager, curl or the agent repeats it) and reports whether it is a policy
// refusal: one no retry under the same profile can get past (policyPhrases).
// Only the text after the proxy's "egress-proxy: " marker is read, so a
// line without it is never one. A tunnel refusal names its host, which
// must be a valid host name for the line to count.
func PolicyRefusal(line string) (Refusal, bool) {
	_, msg, ok := strings.Cut(line, refusalMarker)
	if !ok {
		return Refusal{}, false
	}
	if host, rest, ok := strings.Cut(msg, " is not in profile "); ok {
		host = strings.TrimSpace(host)
		if ValidHost(host) && strings.Contains(rest, "'s hosts") {
			return Refusal{Kind: RefusalHost, Host: host}, true
		}
		return Refusal{}, false
	}
	for _, p := range policyPhrases {
		if strings.Contains(msg, p.phrase) {
			return Refusal{Kind: p.kind}, true
		}
	}
	return Refusal{}, false
}
