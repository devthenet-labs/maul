// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The bounds of a profile's upload caps, in bytes; the chart schema
// carries the same numbers.
const (
	MinUploadBytes = 64 << 10
	MaxUploadBytes = 1 << 30
)

// Upload overrides the egress proxy's upload caps for one profile's
// tunnels: the client-to-upstream bytes of one tunnel, and of every tunnel
// of one source on this profile. Zero leaves the proxy's default; at least
// one is set.
type Upload struct {
	PerTunnel int64 `json:"perTunnel,omitempty"`
	PerSource int64 `json:"perSource,omitempty"`
}

// GoRouteHosts are the hosts behind the public Go route's default
// upstreams (the module proxy and the checksum database). A profile with
// the Go route may not tunnel to them: GOPROXY pointed at the tunnel would
// skip the route's module allowlist, its pseudo-version gate and its
// toolchain refusal. egressproxy's tests pin this to its defaults.
var GoRouteHosts = []string{"proxy.golang.org", "sum.golang.org"}

// HasGoRoute reports whether the profile serves the public Go route.
func (p Profile) HasGoRoute() bool { return p.Route == RouteGo }

// HasTunnel reports whether the profile serves a CONNECT tunnel: it lists
// hosts.
func (p Profile) HasTunnel() bool { return len(p.Hosts) > 0 }

// validateTunnel is the step-3 half of Validate: the hosts (grammar, no
// duplicate, the tunnel deny table with the profile's own forge host, and
// none a host of one of the profile's registry grants or, beside the Go
// route, one of GoRouteHosts), then the clients
// and the upload caps, both only beside hosts. validateRoute has already
// held hosts to a route.
func (p Profile) validateTunnel() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("profile %s: %s", p.Name, fmt.Sprintf(format, args...))
	}
	if len(p.Hosts) > MaxTunnelHosts {
		return fail("hosts lists %d names; at most %d", len(p.Hosts), MaxTunnelHosts)
	}
	seen := map[string]bool{}
	for _, h := range p.Hosts {
		if !ValidHost(h) {
			return fail("host %q is not an exact lowercase host name (no wildcard, IP address or trailing dot)", h)
		}
		if seen[h] {
			return fail("host %s is listed twice", h)
		}
		seen[h] = true
		if err := DeniedTunnelHost(h, DenyContext{Hosts: []string{p.Host}}); err != nil {
			return fail("hosts: %v", err)
		}
		if g, gh, ok := p.grantHost(h); ok {
			return fail("host %s is %s, a host of its %s grant: the tunnel would bypass the grant's package "+
				"policy", h, gh, g)
		}
		if p.HasGoRoute() && slices.Contains(GoRouteHosts, h) {
			return fail("host %s is an upstream of the profile's Go route: the tunnel would bypass the route's "+
				"module allowlist", h)
		}
	}
	if err := p.validateClients(); err != nil {
		return fail("%v", err)
	}
	if err := p.Upload.validate(len(p.Hosts) > 0); err != nil {
		return fail("%v", err)
	}
	return nil
}

// grantHost reports the grant and the grant host that h is, or that a
// grant's *.suffix pattern matches.
func (p Profile) grantHost(h string) (grant, host string, ok bool) {
	for _, g := range p.Registries {
		for _, gh := range g.Hosts {
			if suffix, isPattern := strings.CutPrefix(gh, "*."); isPattern {
				if strings.HasSuffix(h, "."+suffix) {
					return g.Name, gh, true
				}
			} else if h == gh {
				return g.Name, gh, true
			}
		}
	}
	return "", "", false
}

// validateClients holds the client presets to the enum, without
// duplicates, only beside hosts, and never beside a grant that sets the
// same package manager's registry (clientEcosystem).
func (p Profile) validateClients() error {
	if len(p.Clients) == 0 {
		return nil
	}
	if len(p.Hosts) == 0 {
		return errors.New("clients configure the tunnel; set hosts or drop them")
	}
	if len(p.Clients) > len(Clients) {
		return fmt.Errorf("clients lists %d entries; at most %d", len(p.Clients), len(Clients))
	}
	seen := map[string]bool{}
	for _, c := range p.Clients {
		if !ValidClient(c) {
			return fmt.Errorf("client %q is not one of %s", c, strings.Join(Clients, ", "))
		}
		if seen[c] {
			return fmt.Errorf("client %s is listed twice", c)
		}
		seen[c] = true
		if eco, ok := clientEcosystem[c]; ok {
			if g, ok := p.GrantFor(eco); ok {
				return fmt.Errorf("client %s and the %s grant %s both configure the same package manager; "+
					"drop one", c, eco, g.Name)
			}
		}
	}
	return nil
}

// validate checks the caps: only with hosts, at least one set, each in
// [MinUploadBytes, MaxUploadBytes], and perTunnel at most perSource when
// both are set.
func (u *Upload) validate(hasHosts bool) error {
	if u == nil {
		return nil
	}
	if !hasHosts {
		return errors.New("upload caps the tunnel; set hosts or drop it")
	}
	if u.PerTunnel == 0 && u.PerSource == 0 {
		return errors.New("upload sets neither perTunnel nor perSource")
	}
	for _, c := range []struct {
		name string
		v    int64
	}{{"perTunnel", u.PerTunnel}, {"perSource", u.PerSource}} {
		if c.v != 0 && (c.v < MinUploadBytes || c.v > MaxUploadBytes) {
			return fmt.Errorf("upload.%s %d is outside %d-%d bytes", c.name, c.v, MinUploadBytes, MaxUploadBytes)
		}
	}
	if u.PerTunnel != 0 && u.PerSource != 0 && u.PerTunnel > u.PerSource {
		return fmt.Errorf("upload.perTunnel %d is above upload.perSource %d", u.PerTunnel, u.PerSource)
	}
	return nil
}

// cloneTunnel copies the tunnel fields a caller could mutate, keeping nil
// nil (a profile without a tunnel reads back exactly as it was given).
func cloneTunnel(p Profile) (Hosts, []string, *Upload) {
	var u *Upload
	if p.Upload != nil {
		c := *p.Upload
		u = &c
	}
	return slices.Clone(p.Hosts), slices.Clone(p.Clients), u
}
