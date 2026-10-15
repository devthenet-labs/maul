// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestParsePodTunnel: the two values the jobs package sets on a tunnel Job
// read back; nothing set is no tunnel; anything else is an error.
func TestParsePodTunnel(t *testing.T) {
	for _, tt := range []struct {
		name, routes, hosts string
		want                PodTunnel
	}{
		{"none", "", "", PodTunnel{}},
		{"tunnel", "tunnel", "files.pythonhosted.org,pypi.org",
			PodTunnel{Hosts: []string{"files.pythonhosted.org", "pypi.org"}}},
		{"go and tunnel", "go,tunnel", "registry.npmjs.org",
			PodTunnel{GoRoute: true, Hosts: []string{"registry.npmjs.org"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePodTunnel(tt.routes, tt.hosts)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParsePodTunnel(%q, %q) = %+v, %v; want %+v", tt.routes, tt.hosts, got, err, tt.want)
			}
		})
	}
	tooMany := make([]string, 0, MaxTunnelHosts+1)
	for i := range MaxTunnelHosts + 1 {
		tooMany = append(tooMany, "h"+strings.Repeat("a", i)+".example.org")
	}
	slices.Sort(tooMany)
	for _, tt := range []struct{ name, routes, hosts string }{
		{"routes alone", "tunnel", ""},
		{"hosts alone", "", "pypi.org"},
		{"go alone", "go", "pypi.org"},
		{"reversed routes", "tunnel,go", "pypi.org"},
		{"spaced routes", "go, tunnel", "pypi.org"},
		{"unsorted", "tunnel", "pypi.org,files.pythonhosted.org"},
		{"repeated", "tunnel", "pypi.org,pypi.org"},
		{"empty entry", "tunnel", "pypi.org,"},
		{"uppercase", "tunnel", "PyPI.org"},
		{"wildcard", "tunnel", "*.npmjs.org"},
		{"ip", "tunnel", "10.0.0.1"},
		{"markup", "tunnel", "pypi.org`,evil.org"},
		{"space", "tunnel", "pypi.org evil.org"},
		{"too many", "tunnel", strings.Join(tooMany, ",")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := ParsePodTunnel(tt.routes, tt.hosts); err == nil {
				t.Errorf("ParsePodTunnel(%q, %q) = %+v, want an error", tt.routes, tt.hosts, got)
			}
		})
	}
}

// TestPropertyPodTunnelRoundTrip: for generated sorted sets of valid
// hosts, the value the jobs package writes (sorted, comma-joined) reads
// back to the same hosts, and the routes to whether the Go route is set.
// Seeded so the gate is deterministic.
func TestPropertyPodTunnelRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20261016))
	label := func() string {
		const chars = "abcdefghijklmnopqrstuvwxyz0123456789-"
		b := make([]byte, 1+rng.Intn(10))
		for i := range b {
			b[i] = chars[rng.Intn(26)]
			if i > 0 && i < len(b)-1 {
				b[i] = chars[rng.Intn(len(chars))]
			}
		}
		return string(b)
	}
	for i := range 2000 {
		set := map[string]bool{}
		for range 1 + rng.Intn(MaxTunnelHosts) {
			h := label() + "." + label()
			if rng.Intn(2) == 0 {
				h = label() + "." + h
			}
			if ValidHost(h) {
				set[h] = true
			}
		}
		if len(set) == 0 {
			continue
		}
		hosts := make([]string, 0, len(set))
		for h := range set {
			hosts = append(hosts, h)
		}
		slices.Sort(hosts)
		goRoute := rng.Intn(2) == 0
		routes := RouteTunnel
		if goRoute {
			routes = RouteGo + "," + RouteTunnel
		}
		got, err := ParsePodTunnel(routes, strings.Join(hosts, ","))
		if err != nil || got.GoRoute != goRoute || !slices.Equal(got.Hosts, hosts) {
			t.Fatalf("iteration %d: ParsePodTunnel(%q, %v) = %+v, %v", i, routes, hosts, got, err)
		}
	}
}
