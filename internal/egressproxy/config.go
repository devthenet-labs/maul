// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// The default upstreams.
const (
	DefaultGoProxy = "https://proxy.golang.org"
	DefaultSumDB   = "https://sum.golang.org"
)

// Config configures a Server.
type Config struct {
	// Profiles are the operator's profiles; one handler serves each. At
	// least one is required.
	Profiles netprofile.Set
	// GoProxy and SumDB are the fixed upstream bases; nil takes the
	// defaults. The binary admits https only; the engine also admits http
	// so its tests can use httptest upstreams.
	GoProxy, SumDB *url.URL
	// Limits bounds concurrency, rate, time and zip size.
	Limits Limits
	// Policy is the dial policy; the zero value is the production policy.
	Policy AddrPolicy
	// Pods attributes source IPs to agent pods for the audit line; nil
	// means no attribution.
	Pods PodLookup
	// Suspended answers every profile request 410: the operator's
	// immediate lever.
	Suspended bool
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// Registries are the operator's credentialed upstream registries
	// (step 2); a profile's grants name them. Empty: no /r/ route exists.
	Registries netprofile.RegistrySet
	// Credentials hands a registry route its credential. Required when a
	// registry's auth is not none.
	Credentials CredentialSource
	// MaxArtifactBytes caps an archive (npm tarball, pip file, go zip) on
	// a registry route whose registry sets no maxArtifactBytes; 0 is
	// DefaultMaxArtifactBytes.
	MaxArtifactBytes int64
	// AlwaysDeny are ranges never dialled, even when an internal
	// registry lists them: --always-deny-cidrs and the Kubernetes Service
	// IP. The built-in always-denied set (netprofile.AlwaysDeniedCIDRs)
	// applies regardless.
	AlwaysDeny []netip.Prefix
	// DenyHosts are further hosts no redirect hop may reach (the egress
	// broker's base-URL overrides), beside netprofile's deny table.
	DenyHosts []string
	// RegistryCAs are the CA bundles of the registries that set ca: such a
	// registry's transport trusts only its bundle.
	RegistryCAs map[string]*x509.CertPool
	// SelfCheckInterval is the least time between two credential
	// self-checks of one registry; 0 is DefaultSelfCheckInterval.
	SelfCheckInterval time.Duration

	// lookup replaces the dialer's resolver in tests.
	lookup lookupFunc
	// rootCAs replaces the system roots of the registry transports in
	// tests; dialRewrite redirects their dials to httptest listeners.
	rootCAs     *x509.CertPool
	dialRewrite func(addr string) string
}

// ParseUpstream parses an upstream base: an absolute http or https URL with
// a host and nothing after the path.
func ParseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("upstream URL %q: %w", raw, err)
	}
	if err := checkUpstream(u); err != nil {
		return nil, fmt.Errorf("upstream URL %q: %w", raw, err)
	}
	return u, nil
}

func checkUpstream(u *url.URL) error {
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return errors.New("not an http(s) URL")
	case u.Host == "":
		return errors.New("no host")
	case u.User != nil, u.RawQuery != "", u.ForceQuery, u.Fragment != "", u.Opaque != "":
		return errors.New("must carry no user, query or fragment")
	}
	return nil
}

// withDefaults validates cfg and fills its defaults.
func (cfg Config) withDefaults() (Config, error) {
	if cfg.Profiles.Len() == 0 {
		return cfg, errors.New("egress proxy: no network profile configured")
	}
	for _, up := range []struct {
		dst *(*url.URL)
		def string
	}{{&cfg.GoProxy, DefaultGoProxy}, {&cfg.SumDB, DefaultSumDB}} {
		if *up.dst == nil {
			u, err := url.Parse(up.def)
			if err != nil {
				return cfg, err
			}
			*up.dst = u
		}
		if err := checkUpstream(*up.dst); err != nil {
			return cfg, fmt.Errorf("egress proxy: upstream %s: %w", *up.dst, err)
		}
	}
	cfg.Limits = cfg.Limits.withDefaults()
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxArtifactBytes <= 0 {
		cfg.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if cfg.SelfCheckInterval <= 0 {
		cfg.SelfCheckInterval = DefaultSelfCheckInterval
	}
	if err := cfg.checkRegistries(); err != nil {
		return cfg, fmt.Errorf("egress proxy: %w", err)
	}
	return cfg, nil
}

// checkRegistries holds the registries to the profiles and to the rest of
// the configuration: every grant names a registry of its ecosystem, a
// registry with a login has a credential source, a registry with a CA has
// its bundle, and no internal CIDR overlaps an always-denied range.
func (cfg Config) checkRegistries() error {
	if err := netprofile.CheckGrants(cfg.Profiles, cfg.Registries); err != nil {
		return err
	}
	for _, r := range cfg.Registries.Registries() {
		if r.Auth.Type != netprofile.AuthNone && cfg.Credentials == nil {
			return fmt.Errorf("registry %s: auth %s needs a credential source", r.Name, r.Auth.Type)
		}
		if r.CA && cfg.RegistryCAs[r.Name] == nil {
			return fmt.Errorf("registry %s: ca is set and no CA bundle was loaded", r.Name)
		}
		for _, p := range r.InternalPrefixes() {
			for _, d := range cfg.AlwaysDeny {
				if d.Overlaps(p) {
					return fmt.Errorf("registry %s: internal.cidrs entry %s overlaps the always-denied %s",
						r.Name, p, d)
				}
			}
		}
	}
	return nil
}
