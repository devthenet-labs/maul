// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// TestPolicyRefusalsPinnedToTheWire: the job controllers end an intent build
// without another attempt when its agent reports one of the proxy's policy
// refusals (netprofile.PolicyRefusal), so the phrases that function reads
// are pinned here to what this proxy actually writes: each policy refusal
// on the wire is read as one, of its kind, and the transient ones are not.
func TestPolicyRefusalsPinnedToTheWire(t *testing.T) {
	tunnel := []struct {
		name, line string
		kind       netprofile.RefusalKind
		host       string
	}{
		{"not in the profile's hosts", "CONNECT other.registry.dev:443 HTTP/1.1", netprofile.RefusalHost,
			"other.registry.dev"},
		{"forge", "CONNECT github.com:443 HTTP/1.1", netprofile.RefusalNever, ""},
		{"ip literal", "CONNECT 1.2.3.4:443 HTTP/1.1", netprofile.RefusalNever, ""},
		{"port", "CONNECT pkg.registry.dev:80 HTTP/1.1", netprofile.RefusalNever, ""},
		{"HTTP/1.0 (not a policy refusal)", "CONNECT pkg.registry.dev:443 HTTP/1.0", 0, ""},
	}
	for _, tt := range tunnel {
		t.Run(tt.name, func(t *testing.T) {
			h := newTunnelHarness(t, Config{})
			c, br := h.connect(t, "web", tt.line)
			r := readRaw(t, br)
			_ = c.Close()
			got, ok := netprofile.PolicyRefusal(r.statusLine)
			if ok != (tt.kind != 0) || got.Kind != tt.kind || got.Host != tt.host {
				t.Errorf("PolicyRefusal(%q) = %+v, %v; want kind %d host %q", r.statusLine, got, ok, tt.kind, tt.host)
			}
		})
	}

	t.Run("go route allowlist", func(t *testing.T) {
		h := newHarness(t, Config{}, failingUpstream(t), failingUpstream(t))
		r := h.get(t, "/go/rsc.io/quote/@v/v1.5.2.info")
		line := "410 " + strings.TrimSpace(r.body)
		if got, ok := netprofile.PolicyRefusal(line); r.status != http.StatusGone || !ok ||
			got.Kind != netprofile.RefusalPackage {
			t.Errorf("%d %q read as %+v, %v; want a package refusal", r.status, line, got, ok)
		}
	})

	t.Run("registry grant allowlist", func(t *testing.T) {
		h, _ := npmRig(t, okBody("x"), nil)
		r := h.web(t, "/r/art-npm/express", nil)
		line := "403 " + jsonError(t, r.body)
		if got, ok := netprofile.PolicyRefusal(line); r.status != http.StatusForbidden || !ok ||
			got.Kind != netprofile.RefusalPackage {
			t.Errorf("%d %q read as %+v, %v; want a package refusal", r.status, line, got, ok)
		}
	})

	for _, phrase := range []string{suspendedPhrase, shuttingDownPhrase} {
		if got, ok := netprofile.PolicyRefusal("403 " + phrase); ok {
			t.Errorf("transient %q read as a policy refusal %+v", phrase, got)
		}
	}
}
