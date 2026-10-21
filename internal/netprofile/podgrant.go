// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// EnvEcosystems is the agent container variable listing the ecosystems a
// profile Job can fetch through the egress proxy, comma-separated in the
// fixed Ecosystems order ("go,npm,pypi", only those present). It is set
// only on a Job whose profile holds registry grants, so a step-1 profile
// Job carries exactly what it did.
const EnvEcosystems = "MAUL_NETWORK_ECOSYSTEMS"

// EnvGrants is the agent container variable describing a profile Job's
// registry grants to the agent runner (EncodePodGrants), so the prompt can
// say which packages each route serves. Set beside EnvEcosystems only.
const EnvGrants = "MAUL_NETWORK_GRANTS"

// MaxPodPackages bounds the allowlist entries a PodGrant lists by name;
// the rest are counted (MorePackages). It keeps the pod's environment, and
// the prompt, a bounded size whatever an operator's allowlist holds.
const MaxPodPackages = 64

// maxPodGrantsBytes bounds EnvGrants' value when decoding: 3 grants of
// MaxPodPackages names of at most 214 bytes, with room for the rest.
const maxPodGrantsBytes = 64 << 10

// PodGrant is what a profile Job's agent is told about one registry grant:
// the registry's name and ecosystem, its package policy (the first
// MaxPodPackages allowlist entries and a count of the rest, or AnyPackage
// under packagePolicy registry) and the grant's switches. It carries no
// host, URL or credential: the agent reaches the registry only through the
// proxy's /r/<registry> path, which the environment already names.
type PodGrant struct {
	// Registry is the registry granted.
	Registry string `json:"registry"`
	// Ecosystem is the registry's ecosystem.
	Ecosystem string `json:"ecosystem"`
	// Packages are the allowlist's first MaxPodPackages entries.
	Packages []string `json:"packages,omitempty"`
	// MorePackages counts the allowlist entries Packages leaves out.
	MorePackages int `json:"morePackages,omitempty"`
	// AnyPackage is packagePolicy registry: the registry's curation is the
	// policy.
	AnyPackage bool `json:"anyPackage,omitempty"`
	// AllowPrerelease (npm) admits prerelease and build versions.
	AllowPrerelease bool `json:"allowPrerelease,omitempty"`
	// AllowPseudoVersions (go) admits pseudo-versions.
	AllowPseudoVersions bool `json:"allowPseudoVersions,omitempty"`
	// Scripts (npm) runs install scripts.
	Scripts bool `json:"scripts,omitempty"`
	// Sdists (pypi) allows source builds.
	Sdists bool `json:"sdists,omitempty"`
	// PrivateModules (go) are the registry's private module prefixes.
	PrivateModules []string `json:"privateModules,omitempty"`
}

// PodGrantOf is what the agent is told about g.
func PodGrantOf(g Grant) PodGrant {
	pg := PodGrant{
		Registry:            g.Name,
		Ecosystem:           g.Ecosystem,
		AnyPackage:          g.PackagePolicy == PackagePolicyRegistry,
		AllowPrerelease:     g.AllowPrerelease,
		AllowPseudoVersions: g.AllowPseudoVersions,
		Scripts:             g.NPMScripts,
		Sdists:              g.PipSdists,
		PrivateModules:      slices.Clone(g.PrivateModules),
	}
	pg.Packages = slices.Clone(g.Packages[:min(len(g.Packages), MaxPodPackages)])
	pg.MorePackages = len(g.Packages) - len(pg.Packages)
	return pg
}

// Validate checks a PodGrant as the agent runner receives it: the grant
// grammar of its fields, a package policy that is one of the two, and at
// most MaxPodPackages named entries.
func (pg PodGrant) Validate() error {
	g := Grant{
		Name: pg.Registry, Ecosystem: pg.Ecosystem, Packages: pg.Packages,
		AllowPrerelease: pg.AllowPrerelease, AllowPseudoVersions: pg.AllowPseudoVersions,
		NPMScripts: pg.Scripts, PipSdists: pg.Sdists, PrivateModules: pg.PrivateModules,
	}
	if pg.AnyPackage {
		g.PackagePolicy = PackagePolicyRegistry
		if pg.MorePackages != 0 {
			return fmt.Errorf("grant %s: packagePolicy registry counts no packages", pg.Registry)
		}
	}
	switch {
	case len(pg.Packages) > MaxPodPackages:
		return fmt.Errorf("grant %s: %d packages named; at most %d", pg.Registry, len(pg.Packages), MaxPodPackages)
	case pg.MorePackages < 0 || pg.MorePackages > MaxGrantPackages:
		return fmt.Errorf("grant %s: %d more packages is out of range", pg.Registry, pg.MorePackages)
	case pg.MorePackages > 0 && len(pg.Packages) < MaxPodPackages:
		return fmt.Errorf("grant %s: packages are counted only past the first %d", pg.Registry, MaxPodPackages)
	}
	return g.Validate()
}

// EncodePodGrants is EnvGrants' value: grants as a JSON array in the fixed
// Ecosystems order. Each must Validate, and no two may share an ecosystem.
func EncodePodGrants(grants []PodGrant) (string, error) {
	sorted, err := checkPodGrants(grants)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodePodGrants reads EnvGrants' value: a JSON array of PodGrants (unknown
// fields and trailing data refused), each valid, at most one per
// ecosystem, in the fixed Ecosystems order. Empty is no grants.
func DecodePodGrants(s string) ([]PodGrant, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	if len(s) > maxPodGrantsBytes {
		return nil, fmt.Errorf("%s is %d bytes; at most %d", EnvGrants, len(s), maxPodGrantsBytes)
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var grants []PodGrant
	if err := dec.Decode(&grants); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvGrants, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: trailing data after the JSON array", EnvGrants)
	}
	sorted, err := checkPodGrants(grants)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvGrants, err)
	}
	if !slices.EqualFunc(sorted, grants, func(a, b PodGrant) bool { return a.Ecosystem == b.Ecosystem }) {
		return nil, fmt.Errorf("%s: grants are not in the order %s", EnvGrants, strings.Join(Ecosystems, ", "))
	}
	return grants, nil
}

// checkPodGrants validates grants and returns them sorted by ecosystem.
func checkPodGrants(grants []PodGrant) ([]PodGrant, error) {
	seen := map[string]bool{}
	for _, g := range grants {
		if err := g.Validate(); err != nil {
			return nil, err
		}
		if seen[g.Ecosystem] {
			return nil, fmt.Errorf("two %s grants: %w", g.Ecosystem, errOneGrantPerEcosystem)
		}
		seen[g.Ecosystem] = true
	}
	sorted := slices.Clone(grants)
	slices.SortStableFunc(sorted, func(a, b PodGrant) int {
		return slices.Index(Ecosystems, a.Ecosystem) - slices.Index(Ecosystems, b.Ecosystem)
	})
	return sorted, nil
}

// EcosystemList is EnvEcosystems' value for a profile with or without the
// public go route and these grants: each ecosystem present, once, in the
// fixed Ecosystems order.
func EcosystemList(publicGo bool, grants []PodGrant) string {
	present := map[string]bool{EcosystemGo: publicGo}
	for _, g := range grants {
		present[g.Ecosystem] = true
	}
	var out []string
	for _, e := range Ecosystems {
		if present[e] {
			out = append(out, e)
		}
	}
	return strings.Join(out, ",")
}

// ParseEcosystems reads EnvEcosystems' value: known ecosystems, each once,
// in the fixed Ecosystems order. Empty is none.
func ParseEcosystems(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	last := -1
	for e := range strings.SplitSeq(s, ",") {
		i := slices.Index(Ecosystems, e)
		if i < 0 {
			return nil, fmt.Errorf("%s: %q is not an ecosystem (%s)", EnvEcosystems, e, strings.Join(Ecosystems, ", "))
		}
		if i <= last {
			return nil, fmt.Errorf("%s: %q is repeated or out of the order %s", EnvEcosystems, s,
				strings.Join(Ecosystems, ", "))
		}
		last = i
		out = append(out, e)
	}
	return out, nil
}

// CheckPodRoutes holds a profile Job's two variables to each other as the
// agent runner reads them: every grant's ecosystem is listed, and every
// listed ecosystem but go (the public route) has a grant.
func CheckPodRoutes(ecosystems []string, grants []PodGrant) error {
	granted := map[string]bool{}
	for _, g := range grants {
		if !slices.Contains(ecosystems, g.Ecosystem) {
			return fmt.Errorf("grant %s is %s, which %s does not list", g.Registry, g.Ecosystem, EnvEcosystems)
		}
		granted[g.Ecosystem] = true
	}
	for _, e := range ecosystems {
		if e != EcosystemGo && !granted[e] {
			return fmt.Errorf("%s lists %s, which no grant serves", EnvEcosystems, e)
		}
	}
	if len(grants) == 0 && len(ecosystems) > 0 {
		return fmt.Errorf("%s is set without %s", EnvEcosystems, EnvGrants)
	}
	return nil
}
