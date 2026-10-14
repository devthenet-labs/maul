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
	// Route is the dependency route the profile opens; RouteGo only.
	Route string `json:"route"`
	// Port is the egress-proxy listener serving this profile alone.
	Port int `json:"port"`
	// Host is the forge host the repositories live on, lowercase.
	Host string `json:"host"`
	// Repositories are RE2 patterns matched anchored against a
	// repository's lowercased owner/name.
	Repositories []string `json:"repositories"`
	// Modules are the module-path prefixes the proxy forwards.
	Modules Prefixes `json:"modules"`
	// BakedModCache is the image's GOMODCACHE, served read-only after the
	// proxy; "" means the network only.
	BakedModCache string `json:"bakedModCache"`
	// AllowPseudoVersions admits v0.0.0-<time>-<commit> versions of
	// allowed modules; off, only tagged releases are forwarded.
	AllowPseudoVersions bool `json:"allowPseudoVersions"`

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
	if p.Route != RouteGo {
		return fmt.Errorf("profile %s: route %q is not supported (only %q)", p.Name, p.Route, RouteGo)
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
	if len(p.Modules) == 0 || len(p.Modules) > MaxModules {
		return fmt.Errorf("profile %s: modules must list 1-%d prefixes, not %d", p.Name, MaxModules, len(p.Modules))
	}
	for _, m := range p.Modules {
		if err := ValidPrefix(m); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
	}
	if err := validBakedModCache(p.BakedModCache); err != nil {
		return fmt.Errorf("profile %s: %w", p.Name, err)
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
// owner/name.
func anchored(pattern string) (*regexp.Regexp, error) {
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
	return p
}
