// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
)

// RouteGo is the Go modules route: GOPROXY and the checksum database
// through the egress proxy. It is the only route in step 1.
const RouteGo = "go"

// RouteTunnel is a profile whose only public route is the CONNECT tunnel
// to its exact hosts (step 3). A RouteGo profile may list hosts too, and
// then serves both on its one port.
const RouteTunnel = "tunnel"

// DefaultHost is the forge host a profile's repositories live on when the
// profile names none.
const DefaultHost = "github.com"

// HealthPort is the egress proxy's health port, which no profile may use.
const HealthPort = 8081

// Limits on a profile and a set; the chart schema carries the same numbers.
const (
	MaxProfiles      = 8
	MaxNameLen       = 32
	MaxRepositories  = 64
	MaxPatternLen    = 256
	MaxModules       = 256
	MinPort          = 1024
	MaxPort          = 65535
	maxHostLen       = 253
	maxBakedCacheLen = 4096
)

var (
	// namePattern is a DNS label of at most MaxNameLen characters that
	// starts with a letter.
	namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	// hostPattern is a lowercase host name.
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	// bakedPattern is an absolute path of plain elements, or "".
	bakedPattern = regexp.MustCompile(`^(/[A-Za-z0-9._+-]+)*$`)
)

// Profile is one operator network profile, as the chart renders it.
type Profile struct {
	// Name is a DNS label of at most 32 characters; it names the pod label
	// (LabelKey) and appears in the agent's prompt.
	Name string `json:"name"`
	// Route is the public dependency route the profile opens: RouteGo, or
	// "" for a profile that holds registry grants only.
	Route string `json:"route"`
	// Port is the egress-proxy listener serving this profile alone.
	Port int `json:"port"`
	// Host is the forge host the repositories live on, lowercase.
	Host string `json:"host"`
	// Repositories are RE2 patterns matched anchored against a
	// repository's lowercased owner/name.
	Repositories []string `json:"repositories"`
	// Modules are the module-path prefixes the proxy forwards on the
	// public route; required with RouteGo, absent without it.
	Modules Prefixes `json:"modules"`
	// BakedModCache is the image's GOMODCACHE, served read-only after the
	// proxy; "" means the network only.
	BakedModCache string `json:"bakedModCache"`
	// AllowPseudoVersions admits v0.0.0-<time>-<commit> versions of
	// allowed modules; off, only tagged releases are forwarded.
	AllowPseudoVersions bool `json:"allowPseudoVersions"`
	// Registries are the profile's registry grants (step 2), at most one
	// per ecosystem. The key is rendered only for a profile that has
	// grants, so a step-1 profile's JSON is unchanged.
	Registries []Grant `json:"registries,omitempty"`
	// Hosts are the exact public host names the profile's tunnel opens on
	// 443 (step 3); none means no tunnel. Rendered only when set, like the
	// two keys below.
	Hosts Hosts `json:"hosts,omitempty"`
	// Clients are the client presets (Clients) the job controllers apply
	// beside the tunnel; only with Hosts.
	Clients []string `json:"clients,omitempty"`
	// Upload overrides the proxy's upload caps for this profile's tunnels;
	// only with Hosts.
	Upload *Upload `json:"upload,omitempty"`

	// patterns are Repositories compiled, anchored; set by NewSet.
	patterns []*regexp.Regexp
}

// Validate checks one profile's own fields; NewSet adds the checks across
// profiles (unique names and ports) and compiles the patterns.
func (p Profile) Validate() error {
	if !namePattern.MatchString(p.Name) {
		return fmt.Errorf("profile name %q is not a DNS label of at most %d characters starting with a letter",
			p.Name, MaxNameLen)
	}
	if err := p.validateRoute(); err != nil {
		return err
	}
	if p.Port < MinPort || p.Port > MaxPort {
		return fmt.Errorf("profile %s: port %d is outside %d-%d", p.Name, p.Port, MinPort, MaxPort)
	}
	if p.Port == HealthPort {
		return fmt.Errorf("profile %s: port %d is the egress proxy's health port", p.Name, p.Port)
	}
	if len(p.Host) > maxHostLen || !hostPattern.MatchString(p.Host) {
		return fmt.Errorf("profile %s: host %q is not a lowercase host name", p.Name, p.Host)
	}
	if len(p.Repositories) == 0 || len(p.Repositories) > MaxRepositories {
		return fmt.Errorf("profile %s: repositories must list 1-%d patterns, not %d",
			p.Name, MaxRepositories, len(p.Repositories))
	}
	for _, r := range p.Repositories {
		if r == "" || len(r) > MaxPatternLen {
			return fmt.Errorf("profile %s: repository pattern %q must be 1-%d bytes", p.Name, r, MaxPatternLen)
		}
		if _, err := anchored(r); err != nil {
			return fmt.Errorf("profile %s: repository pattern %q: %w", p.Name, r, err)
		}
	}
	if err := validBakedModCache(p.BakedModCache); err != nil {
		return fmt.Errorf("profile %s: %w", p.Name, err)
	}
	if err := p.validateGrants(); err != nil {
		return err
	}
	return p.validateTunnel()
}

// validateGrants checks each grant and the profile-wide rules: one grant
// per registry and per ecosystem (guard 10).
func (p Profile) validateGrants() error {
	if len(p.Registries) > len(Ecosystems) {
		return fmt.Errorf("profile %s: %w", p.Name, errOneGrantPerEcosystem)
	}
	seen := map[string]bool{}
	for _, g := range p.Registries {
		if err := g.Validate(); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
		if seen[g.Ecosystem] {
			return fmt.Errorf("profile %s: two %s grants: %w", p.Name, g.Ecosystem, errOneGrantPerEcosystem)
		}
		seen[g.Ecosystem] = true
	}
	return nil
}

// Grant returns the profile's grant of the registry called name.
func (p Profile) Grant(name string) (Grant, bool) {
	for _, g := range p.Registries {
		if g.Name == name {
			return g.clone(), true
		}
	}
	return Grant{}, false
}

// GrantFor returns the profile's grant for the ecosystem.
func (p Profile) GrantFor(ecosystem string) (Grant, bool) {
	for _, g := range p.Registries {
		if g.Ecosystem == ecosystem {
			return g.clone(), true
		}
	}
	return Grant{}, false
}

// validateRoute is the public route and its modules: RouteGo with 1 to
// MaxModules module prefixes; RouteTunnel with hosts and none of the go
// route's fields; or no route, no modules and at least one grant.
func (p Profile) validateRoute() error {
	switch {
	case p.Route == "" && len(p.Registries) == 0:
		return fmt.Errorf("profile %s: route %q is not supported (only %q or %q), and a profile without a route "+
			"must hold registry grants", p.Name, p.Route, RouteGo, RouteTunnel)
	case p.Route != "" && p.Route != RouteGo && p.Route != RouteTunnel:
		return fmt.Errorf("profile %s: route %q is not supported (only %q or %q)", p.Name, p.Route, RouteGo,
			RouteTunnel)
	}
	if p.Route == RouteGo && (len(p.Modules) == 0 || len(p.Modules) > MaxModules) {
		return fmt.Errorf("profile %s: modules must list 1-%d prefixes, not %d", p.Name, MaxModules, len(p.Modules))
	}
	if p.Route == "" && len(p.Modules) > 0 {
		return fmt.Errorf("profile %s: modules are the public go route's allowlist; set route: go or drop them",
			p.Name)
	}
	if p.Route == RouteTunnel {
		switch {
		case len(p.Hosts) == 0:
			return fmt.Errorf("profile %s: route tunnel needs hosts", p.Name)
		case len(p.Modules) > 0 || p.BakedModCache != "" || p.AllowPseudoVersions:
			return fmt.Errorf("profile %s: modules, bakedModCache and allowPseudoVersions are the go route's; "+
				"route tunnel takes none of them", p.Name)
		}
	}
	if p.Route == "" && len(p.Hosts) > 0 {
		return fmt.Errorf("profile %s: hosts need route: tunnel (or route: go, for both)", p.Name)
	}
	for _, m := range p.Modules {
		if err := ValidPrefix(m); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
	}
	return nil
}

// validBakedModCache admits "" or an absolute, path.Clean-stable path of
// plain elements.
func validBakedModCache(dir string) error {
	if dir == "" {
		return nil
	}
	if len(dir) > maxBakedCacheLen || !bakedPattern.MatchString(dir) || path.Clean(dir) != dir {
		return fmt.Errorf("bakedModCache %q is not an absolute clean path (or empty)", dir)
	}
	return nil
}

// anchored compiles a repository pattern so it must match the whole
// owner/name. The bare pattern is compiled first: one with an unbalanced
// ")" the wrapper happens to balance, such as "a)|(.*", would otherwise
// close the anchoring group early and add an unanchored alternative that
// matches every repository. A pattern that compiles alone is balanced, so
// wrapping it is safe.
func anchored(pattern string) (*regexp.Regexp, error) {
	if _, err := regexp.Compile(pattern); err != nil {
		return nil, err
	}
	return regexp.Compile(`^(?:` + pattern + `)$`)
}

// Set is a validated profile set, sorted by name. The zero Set is empty and
// matches nothing.
type Set struct {
	profiles []Profile
}

// NewSet validates profiles (each on its own, then unique names and ports,
// at most MaxProfiles) and compiles their patterns. An empty list is an
// empty Set, not an error: whether a caller may run without profiles is the
// caller's decision.
func NewSet(profiles []Profile) (Set, error) {
	if len(profiles) > MaxProfiles {
		return Set{}, fmt.Errorf("%d profiles; at most %d are allowed", len(profiles), MaxProfiles)
	}
	names := map[string]bool{}
	ports := map[int]string{}
	out := make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		if err := p.Validate(); err != nil {
			return Set{}, err
		}
		if names[p.Name] {
			return Set{}, fmt.Errorf("profile %s is named twice", p.Name)
		}
		names[p.Name] = true
		if other, ok := ports[p.Port]; ok {
			return Set{}, fmt.Errorf("profiles %s and %s both use port %d", other, p.Name, p.Port)
		}
		ports[p.Port] = p.Name
		p.Repositories = append([]string(nil), p.Repositories...)
		p.Modules = append(Prefixes(nil), p.Modules...)
		p.Registries = cloneGrants(p.Registries)
		p.Hosts, p.Clients, p.Upload = cloneTunnel(p)
		p.patterns = make([]*regexp.Regexp, len(p.Repositories))
		for i, r := range p.Repositories {
			re, err := anchored(r)
			if err != nil { // Validate compiled it already
				return Set{}, fmt.Errorf("profile %s: repository pattern %q: %w", p.Name, r, err)
			}
			p.patterns[i] = re
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return Set{profiles: out}, nil
}

// Parse decodes the profile set's JSON (an array of profiles, as the chart
// renders it), refusing unknown fields and trailing data, applies
// DefaultHost to a profile without a host, and validates it with NewSet.
// Empty input, "null" and "[]" are an empty Set.
func Parse(data []byte) (Set, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Set{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var profiles []Profile
	if err := dec.Decode(&profiles); err != nil {
		return Set{}, fmt.Errorf("network profiles: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Set{}, errors.New("network profiles: trailing data after the JSON array")
	}
	for i := range profiles {
		if profiles[i].Host == "" {
			profiles[i].Host = DefaultHost
		}
	}
	set, err := NewSet(profiles)
	if err != nil {
		return Set{}, fmt.Errorf("network profiles: %w", err)
	}
	return set, nil
}

// Len is the number of profiles.
func (s Set) Len() int { return len(s.profiles) }

// Profiles returns a copy of the profiles, sorted by name.
func (s Set) Profiles() []Profile {
	out := make([]Profile, len(s.profiles))
	for i, p := range s.profiles {
		out[i] = p.clone()
	}
	return out
}

// Get returns the profile called name.
func (s Set) Get(name string) (Profile, bool) {
	for _, p := range s.profiles {
		if p.Name == name {
			return p.clone(), true
		}
	}
	return Profile{}, false
}

// clone copies the lists a caller could mutate; the compiled patterns are
// immutable and shared.
func (p Profile) clone() Profile {
	p.Repositories = append([]string(nil), p.Repositories...)
	p.Modules = append(Prefixes(nil), p.Modules...)
	p.Registries = cloneGrants(p.Registries)
	p.Hosts, p.Clients, p.Upload = cloneTunnel(p)
	return p
}

// cloneGrants deep-copies grants, keeping nil nil (a step-1 profile reads
// back exactly as it was given).
func cloneGrants(gs []Grant) []Grant {
	if gs == nil {
		return nil
	}
	out := make([]Grant, len(gs))
	for i, g := range gs {
		out[i] = g.clone()
	}
	return out
}

// Hosts are the forge hosts of every profile, sorted and unique: a
// registry may be none of them (DenyContext.Hosts).
func (s Set) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.profiles {
		if !seen[p.Host] {
			seen[p.Host] = true
			out = append(out, p.Host)
		}
	}
	sort.Strings(out)
	return out
}
