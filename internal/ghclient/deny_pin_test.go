// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient_test

import (
	"net/url"
	"testing"

	"github.com/google/go-github/v90/github"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// TestForgeHostsDenied pins the egress proxy's hard-deny tables to the
// hosts patchy's GitHub client really reaches: go-github's default API and
// upload hosts, read from a fresh client so a moved default fails here,
// plus the archive (codeload) and release-asset hosts its redirects lead
// to. No registry may be any of them (DeniedHost), and no tunnel may open
// to one (DeniedTunnelHost).
func TestForgeHostsDenied(t *testing.T) {
	c, err := github.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defaults := []string{c.BaseURL(), c.UploadURL()}
	hosts := make([]string, 0, len(defaults)+2)
	hosts = append(hosts, "codeload.github.com", "objects.githubusercontent.com")
	for _, raw := range defaults {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			t.Fatalf("go-github's default URL %q has no host (%v)", raw, err)
		}
		hosts = append(hosts, u.Hostname())
	}
	for _, h := range hosts {
		if err := netprofile.DeniedHost(h, netprofile.DenyContext{}); err == nil {
			t.Errorf("netprofile.DeniedHost admits the GitHub host %s", h)
		}
		if err := netprofile.DeniedTunnelHost(h, netprofile.DenyContext{}); err == nil {
			t.Errorf("netprofile.DeniedTunnelHost admits the GitHub host %s", h)
		}
	}
}
