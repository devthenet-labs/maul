// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PackagePolicyRegistry is the one packagePolicy: the registry's own
// curation is the policy, and the proxy forwards any well-formed name.
const PackagePolicyRegistry = "registry"

// MaxGrantPackages bounds a grant's packages allowlist.
const MaxGrantPackages = 512

// Grant is one profile's grant of one registry
// (agent.networkProfiles.<p>.registries.<name>), as the chart renders it
// into the profile JSON: the grant's own policy plus the registry's
// ecosystem, privateModules and hosts, so a job controller never needs the
// registry block.
type Grant struct {
	// Name is the registry granted.
	Name string `json:"name"`
	// Ecosystem is the registry's ecosystem.
	Ecosystem string `json:"ecosystem"`
	// Hosts are the registry's hosts (Registry.Hosts), for the job
	// controllers' Forge check.
	Hosts []string `json:"hosts,omitempty"`
	// Packages is an explicit allowlist: go module prefixes, npm names or
	// @scope/*, pip project names. Exactly one of Packages and
	// PackagePolicy is set.
	Packages []string `json:"packages,omitempty"`
	// PackagePolicy "registry" leaves the policy to the registry.
	PackagePolicy string `json:"packagePolicy,omitempty"`
	// AllowPseudoVersions (go) admits pseudo-versions.
	AllowPseudoVersions bool `json:"allowPseudoVersions,omitempty"`
	// AllowPrerelease (npm) admits prerelease and build versions.
	AllowPrerelease bool `json:"allowPrerelease,omitempty"`
	// NPMScripts (npm) runs install scripts.
	NPMScripts bool `json:"scripts,omitempty"`
	// PipSdists (pypi) allows source builds.
	PipSdists bool `json:"sdists,omitempty"`
	// PrivateModules (go) are the registry's private module prefixes.
	PrivateModules []string `json:"privateModules,omitempty"`
}

// Validate checks a grant on its own: the name, the ecosystem, exactly one
// package policy (guard 11), the allowlist's entries per ecosystem, the
// per-ecosystem switches (guard 17) and the hosts' shape and deny table.
func (g Grant) Validate() error {
	if !namePattern.MatchString(g.Name) {
		return fmt.Errorf("grant %q: registry name is not a DNS label of at most %d characters starting with a letter",
			g.Name, MaxNameLen)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("grant %s: %s", g.Name, fmt.Sprintf(format, args...))
	}
	if !validEcosystem(g.Ecosystem) {
		return fail("ecosystem %q is not go, npm or pypi", g.Ecosystem)
	}
	if err := g.validatePolicy(); err != nil {
		return fail("%v", err)
	}
	if err := g.validateSwitches(); err != nil {
		return fail("%v", err)
	}
	if len(g.Hosts) > 1+2*MaxRegistryHosts {
		return fail("hosts lists %d entries; at most %d", len(g.Hosts), 1+2*MaxRegistryHosts)
	}
	for _, h := range g.Hosts {
		if err := g.checkHost(h, DenyContext{Internal: true}); err != nil {
			return fail("hosts: %v", err)
		}
	}
	return nil
}

// validatePolicy is guard 11 and the allowlist's entries.
func (g Grant) validatePolicy() error {
	switch {
	case g.PackagePolicy != "" && g.PackagePolicy != PackagePolicyRegistry:
		return fmt.Errorf("packagePolicy %q is not %q", g.PackagePolicy, PackagePolicyRegistry)
	case g.PackagePolicy == "" && len(g.Packages) == 0:
		return errors.New("set packages (an allowlist) or packagePolicy: registry; there is no default")
	case g.PackagePolicy != "" && len(g.Packages) > 0:
		return errors.New("set packages or packagePolicy: registry, not both")
	case len(g.Packages) > MaxGrantPackages:
		return fmt.Errorf("packages lists %d entries; at most %d", len(g.Packages), MaxGrantPackages)
	}
	for _, p := range g.Packages {
		if err := validPackage(g.Ecosystem, p); err != nil {
			return fmt.Errorf("packages: %w", err)
		}
	}
	return nil
}

// validateSwitches is guard 17's grant half: each switch on its own
// ecosystem only.
func (g Grant) validateSwitches() error {
	switch {
	case (g.NPMScripts || g.AllowPrerelease) && g.Ecosystem != EcosystemNPM:
		return errors.New("scripts and allowPrerelease are for npm grants only")
	case g.PipSdists && g.Ecosystem != EcosystemPyPI:
		return errors.New("sdists is for pypi grants only")
	case g.AllowPseudoVersions && g.Ecosystem != EcosystemGo:
		return errors.New("allowPseudoVersions is for go grants only")
	case len(g.PrivateModules) > 0 && g.Ecosystem != EcosystemGo:
		return errors.New("privateModules is for go grants only")
	}
	return validModuleList("privateModules", g.PrivateModules)
}

// checkHost judges one of the grant's hosts against ctx. The first host is
// the registry URL's, which may be an internal address; it is judged on
// its shape and the name table, since the registry's internal CIDRs are
// not in the profile JSON (the proxy judges the address itself).
func (g Grant) checkHost(h string, ctx DenyContext) error {
	if strings.HasPrefix(h, "*.") {
		return DeniedPattern(h, ctx)
	}
	if len(h) > maxHostLen || !hostPattern.MatchString(h) || !validLabels(h) {
		return fmt.Errorf("%q is not a lowercase host name or *.suffix pattern", h)
	}
	if isAddr(h) {
		return nil
	}
	return DeniedHost(h, ctx)
}

// DeniedHosts runs the grant's hosts through ctx's deny list: the job
// controllers pass the hosts of every Forge CR they can list, which the
// chart cannot know. A pattern is refused when it can match a denied host.
func (g Grant) DeniedHosts(ctx DenyContext) error {
	ctx.Internal = true // in-cluster names are the proxy's to judge, against internal
	for _, h := range g.Hosts {
		if err := g.checkHost(h, ctx); err != nil {
			return fmt.Errorf("grant %s: %w", g.Name, err)
		}
	}
	return nil
}

// validPackage judges one allowlist entry for the ecosystem.
func validPackage(ecosystem, p string) error {
	switch ecosystem {
	case EcosystemGo:
		return ValidPrefix(p)
	case EcosystemNPM:
		if scope, ok := strings.CutSuffix(p, "/*"); ok && strings.HasPrefix(scope, "@") {
			if _, _, err := NPMName(scope + "/x"); err != nil {
				return fmt.Errorf("npm scope wildcard %q: %w", p, err)
			}
			return nil
		}
		_, _, err := NPMName(p)
		return err
	case EcosystemPyPI:
		if !PyName(p) {
			return fmt.Errorf("pip project %q is not a PEP 508 project name", p)
		}
		return nil
	}
	return fmt.Errorf("unknown ecosystem %q", ecosystem)
}

// Allows reports whether the grant admits the package: under
// packagePolicy registry every name (the grammar has judged it already);
// otherwise for go a module path below one of the prefixes, for npm an
// exact name or a name in an "@scope/*" entry's scope, for pypi a project
// whose PEP 503 normalization equals an entry's.
func (g Grant) Allows(pkg string) bool {
	if g.PackagePolicy == PackagePolicyRegistry {
		return pkg != ""
	}
	switch g.Ecosystem {
	case EcosystemGo:
		return Prefixes(g.Packages).Allows(pkg)
	case EcosystemNPM:
		for _, p := range g.Packages {
			if p == pkg {
				return true
			}
			if scope, ok := strings.CutSuffix(p, "/*"); ok && strings.HasPrefix(pkg, scope+"/") {
				return true
			}
		}
	case EcosystemPyPI:
		n := PyNormalize(pkg)
		for _, p := range g.Packages {
			if PyNormalize(p) == n {
				return true
			}
		}
	}
	return false
}

// PrivateModulePrefixes is the grant's privateModules as Prefixes.
func (g Grant) PrivateModulePrefixes() Prefixes { return Prefixes(g.PrivateModules) }

// clone copies the lists a caller could mutate.
func (g Grant) clone() Grant {
	g.Hosts = slices.Clone(g.Hosts)
	g.Packages = slices.Clone(g.Packages)
	g.PrivateModules = slices.Clone(g.PrivateModules)
	return g
}

// CheckGrants holds every profile's grants to the registry set (guard 9
// and its agreement rules): each grant names a registry in the set, and
// carries that registry's ecosystem, privateModules and hosts exactly. The
// proxy runs it at startup with both documents in hand.
func CheckGrants(profiles Set, registries RegistrySet) error {
	for _, p := range profiles.profiles {
		for _, g := range p.Registries {
			r, ok := registries.Get(g.Name)
			if !ok {
				return fmt.Errorf("profile %s: grant %s names no configured registry", p.Name, g.Name)
			}
			if g.Ecosystem != r.Ecosystem {
				return fmt.Errorf("profile %s: grant %s is ecosystem %s but the registry is %s",
					p.Name, g.Name, g.Ecosystem, r.Ecosystem)
			}
			var private []string
			if r.PrivateModules != nil {
				private = *r.PrivateModules
			}
			if !slices.Equal(g.PrivateModules, private) && (len(g.PrivateModules) > 0 || len(private) > 0) {
				return fmt.Errorf("profile %s: grant %s privateModules differ from the registry's", p.Name, g.Name)
			}
			if !slices.Equal(g.Hosts, r.Hosts()) {
				return fmt.Errorf("profile %s: grant %s hosts %v differ from the registry's %v",
					p.Name, g.Name, g.Hosts, r.Hosts())
			}
		}
	}
	return nil
}

// errOneGrantPerEcosystem is guard 10.
var errOneGrantPerEcosystem = errors.New("a profile holds at most one grant per ecosystem " +
	"(each package manager takes one index from the environment)")
