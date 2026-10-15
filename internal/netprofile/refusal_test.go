// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"strings"
	"testing"
	"testing/quick"
)

// TestPolicyRefusal: the proxy's policy refusals are read whichever way a
// tool quotes them; transient and version refusals, and lines that only
// look like the proxy's, are not.
func TestPolicyRefusal(t *testing.T) {
	tests := []struct {
		name, line string
		want       Refusal
		ok         bool
	}{
		// intents#33, as the agent quoted it.
		{"tunnel host", "403 egress-proxy: download.pytorch.org is not in profile py's hosts",
			Refusal{Kind: RefusalHost, Host: "download.pytorch.org", Profile: "py"}, true},
		{"tunnel host, status line", "HTTP/1.1 403 egress-proxy: pkg.example.com is not in profile web's hosts",
			Refusal{Kind: RefusalHost, Host: "pkg.example.com", Profile: "web"}, true},
		{"tunnel host, profile not a name", "403 egress-proxy: pkg.example.com is not in profile Web Dev's hosts",
			Refusal{}, false},
		{"tunnel host, no profile", "403 egress-proxy: pkg.example.com is not in profile 's hosts", Refusal{}, false},
		{"tunnel host not a host name", "403 egress-proxy: pkg_x.example.com is not in profile web's hosts",
			Refusal{}, false},
		{"forge", "403 egress-proxy: github.com is never tunnelled (a forge, model API, control plane, internal " +
			"or publish host)", Refusal{Kind: RefusalNever}, true},
		{"ip literal", "egress-proxy: an IP address is never tunnelled; CONNECT to an allowed host name",
			Refusal{Kind: RefusalNever}, true},
		{"port", "403 egress-proxy: only port 443 may be tunnelled", Refusal{Kind: RefusalNever}, true},
		{"plain http", "400 egress-proxy: absolute-form request URIs are refused", Refusal{Kind: RefusalNever}, true},
		{"go allowlist (410)", "server response: egress-proxy: module path rsc.io/quote is not allowed by profile " +
			"go; allowed prefixes: github.com/google/uuid", Refusal{Kind: RefusalPackage}, true},
		{"pypi grant", "403 egress-proxy: project torch is not allowed by the grant of registry pypi",
			Refusal{Kind: RefusalPackage}, true},
		{"npm grant", "egress-proxy: package left-pad is not allowed by the grant of registry npm",
			Refusal{Kind: RefusalPackage}, true},
		{"not granted", "egress-proxy: profile web holds no grant for this registry route",
			Refusal{Kind: RefusalPackage}, true},
		{"registry policy", "egress-proxy: torch@2.5.1: refused by the policy of registry ca-pypi",
			Refusal{Kind: RefusalPackage}, true},
		{"private module", "egress-proxy: module path corp.example/x is private to registry ca-go and is never " +
			"looked up in the public checksum database", Refusal{Kind: RefusalPackage}, true},
		{"checksum not public", "410 egress-proxy: checksum_not_public: corp.example/x was served by registry ca-go",
			Refusal{Kind: RefusalPackage}, true},

		// Transient: a retry can succeed.
		{"request limit", "429 egress-proxy: over the request limit (rate); retry later", Refusal{}, false},
		{"tunnel limit", "egress-proxy: over the tunnel limit (concurrent)", Refusal{}, false},
		{"credential", "egress-proxy: the credential of registry ca-npm is unavailable; retry later or ask the " +
			"operator", Refusal{}, false},
		{"upstream", "502 egress-proxy: upstream unreachable (dial)", Refusal{}, false},
		{"dial", "502 egress-proxy: could not connect to pkg.example.com", Refusal{}, false},
		{"suspended", "egress-proxy: profile routes are suspended by the operator", Refusal{}, false},
		{"shutting down", "egress-proxy: shutting down; retry the CONNECT", Refusal{}, false},
		{"not found", "404 egress-proxy: torch: not found in registry pypi", Refusal{}, false},
		// Another version can avoid these.
		{"untagged", "egress-proxy: version v0.0.0-2024 of rsc.io/quote is not a tagged release; profile go admits " +
			"tagged releases only", Refusal{}, false},
		{"prerelease", "egress-proxy: 1.0.0-rc.1; the grant of registry npm admits release versions (x.y.z) only",
			Refusal{}, false},

		// Not the proxy's line at all.
		{"no marker", "download.pytorch.org is not in profile py's hosts", Refusal{}, false},
		{"no marker, package", "module path x is not allowed by profile go", Refusal{}, false},
		{"marker without space", "egress-proxy:x is never tunnelled", Refusal{}, false},
		{"empty", "", Refusal{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PolicyRefusal(tt.line)
			if got != tt.want || ok != tt.ok {
				t.Errorf("PolicyRefusal(%q) = %+v, %v; want %+v, %v", tt.line, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestPolicyRefusalProperties, seeded: a line without the proxy's marker is
// never a policy refusal, whatever it holds; a refusal's kind is always one
// of the three; and a host refusal's host and profile are always a valid
// host name and profile name that the line itself carries.
func TestPolicyRefusalProperties(t *testing.T) {
	pool := []string{"egress-proxy: ", "egress-proxy:", " is not in profile ", "'s hosts", " is never tunnelled",
		" is not allowed by profile ", "pkg.example.com", "a_b", "403 ", "x", " ", "..", "\n", "é",
		"checksum_not_public: ", " is private to registry ", "retry later", "py", "Py"}
	draw := func(picks []uint8) string {
		var b strings.Builder
		for _, p := range picks {
			b.WriteString(pool[int(p)%len(pool)])
		}
		return b.String()
	}
	prop := func(picks []uint8) bool {
		line := draw(picks)
		r, ok := PolicyRefusal(line)
		if !strings.Contains(line, RefusalMarker) {
			return !ok && r == Refusal{}
		}
		if !ok {
			return r == Refusal{}
		}
		switch r.Kind {
		case RefusalHost:
			return ValidHost(r.Host) && strings.Contains(line, r.Host) && ValidName(r.Profile) == nil &&
				strings.Contains(line, " "+r.Profile+"'s hosts")
		case RefusalPackage, RefusalNever:
			return r.Host == "" && r.Profile == ""
		}
		return false
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
}
