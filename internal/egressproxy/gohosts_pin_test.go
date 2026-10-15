// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"net/url"
	"slices"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// TestGoRouteHostsMatchDefaults pins netprofile.GoRouteHosts, the hosts a
// Go-route profile may not tunnel to, to the Go route's default upstreams
// and the one checksum database it serves: moving an upstream without the
// refusal would let a tunnel bypass the module allowlist again.
func TestGoRouteHostsMatchDefaults(t *testing.T) {
	want := make([]string, 0, 3)
	for _, raw := range []string{DefaultGoProxy, DefaultSumDB} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, u.Hostname())
	}
	want = append(want, SumDBName)
	for _, h := range want {
		if !slices.Contains(netprofile.GoRouteHosts, h) {
			t.Errorf("netprofile.GoRouteHosts %v lacks the Go route upstream %s", netprofile.GoRouteHosts, h)
		}
	}
	for _, h := range netprofile.GoRouteHosts {
		if !slices.Contains(want, h) {
			t.Errorf("netprofile.GoRouteHosts holds %s, which is no Go route upstream", h)
		}
	}
}
