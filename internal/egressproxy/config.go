// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"fmt"
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

	// lookup replaces the dialer's resolver in tests.
	lookup lookupFunc
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
	return cfg, nil
}
