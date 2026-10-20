// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// ErrDeniedAddress is the dial refusal: the address an upstream name
// resolved to is not a public IPv4 address on port 443, or is in a denied
// range.
var ErrDeniedAddress = errors.New("egress-proxy: upstream address denied")

// upstreamPort is the only port the proxy dials.
const upstreamPort = 443

// builtinDeny are the IPv4 ranges never dialled, beside everything that is
// not global unicast: this network, private, shared, loopback, link-local
// (IMDS, EKS Pod Identity), documentation, benchmarking, multicast and
// reserved.
var builtinDeny = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// AddrPolicy decides which addresses the proxy may dial. The zero value is
// the production policy: a public IPv4 global-unicast address on port 443,
// outside the built-in ranges and every Deny entry.
type AddrPolicy struct {
	// Deny are further ranges never dialled (the chart's cluster CIDRs
	// and cluster DNS address).
	Deny []netip.Prefix

	// testLoopback admits 127.0.0.0/8 on any port, so the engine's tests
	// can reach httptest upstreams. Unexported: no flag can set it.
	testLoopback bool
}

// ParseDenyCIDRs parses the --deny-cidrs list. An IPv6 entry is accepted
// and harmless: IPv6 is never dialled.
func ParseDenyCIDRs(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("deny CIDR %q: %w", e, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Check reports whether ap may be dialled, wrapping ErrDeniedAddress when
// not.
func (p AddrPolicy) Check(ap netip.AddrPort) error {
	a := ap.Addr()
	if !a.Is4() {
		return fmt.Errorf("%w: %s is not IPv4", ErrDeniedAddress, a)
	}
	if p.testLoopback && a.IsLoopback() {
		return nil
	}
	if ap.Port() != upstreamPort {
		return fmt.Errorf("%w: port %d is not %d", ErrDeniedAddress, ap.Port(), upstreamPort)
	}
	if !a.IsGlobalUnicast() {
		return fmt.Errorf("%w: %s is not global unicast", ErrDeniedAddress, a)
	}
	for _, set := range [][]netip.Prefix{builtinDeny, p.Deny} {
		for _, pfx := range set {
			if pfx.Contains(a) {
				return fmt.Errorf("%w: %s is in %s", ErrDeniedAddress, a, pfx)
			}
		}
	}
	return nil
}

// DialPolicy judges an address the guarded dialer is about to connect to,
// wrapping ErrDeniedAddress when it may not. AddrPolicy is the forwarding
// routes' policy; a credential minter's clients bring their own (awsca).
type DialPolicy interface {
	Check(ap netip.AddrPort) error
}

// NewGuardedClient is an HTTP client whose every connection goes through
// the guarded dialer under policy: IPv4 only, the policy judged before the
// dial and again on the address actually connected, no proxy from the
// environment, redirects never followed. The CodeArtifact minter gives
// the AWS SDK two of these, one per policy.
func NewGuardedClient(policy DialPolicy, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: newTransport(newGuardedDialer(policy, nil), timeout),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ForRegistry is the dial policy of one registry's transport. A public
// registry gets exactly p, plus the always-denied ranges. An internal
// registry (internal.cidrs) admits an address inside one of its CIDRs on
// the URL's port, ahead of the built-in deny list, but never one in the
// always-denied set (netprofile.AlwaysDeniedCIDRs and always: the
// Kubernetes Service IP, --always-deny-cidrs), which is judged first; every
// other address falls to p. Redirect hops never use this policy. A port
// outside 1-65535 (validation refuses one; ForRegistry does not trust it
// did) admits no internal address at all rather than a truncated port.
func (p AddrPolicy) ForRegistry(r netprofile.Registry, always []netip.Prefix) DialPolicy {
	internal := r.InternalPrefixes()
	var port uint16
	if n := r.Port(); n >= 1 && n <= math.MaxUint16 {
		port = uint16(n)
	} else {
		internal = nil
	}
	return registryPolicy{public: p, internal: internal, port: port,
		always: append(append([]netip.Prefix(nil), netprofile.AlwaysDeniedCIDRs...), always...)}
}

// registryPolicy is ForRegistry's policy.
type registryPolicy struct {
	public   AddrPolicy
	internal []netip.Prefix
	port     uint16
	always   []netip.Prefix
}

// Check judges ap: the test loopback (public registries only), then the
// always-denied set, then the registry's internal admission, then the
// public policy.
func (p registryPolicy) Check(ap netip.AddrPort) error {
	a := ap.Addr().Unmap()
	if !a.Is4() {
		return fmt.Errorf("%w: %s is not IPv4", ErrDeniedAddress, a)
	}
	if len(p.internal) == 0 && p.public.testLoopback && a.IsLoopback() {
		return nil
	}
	for _, d := range p.always {
		if d.Contains(a) {
			return fmt.Errorf("%w: %s is in the always-denied %s", ErrDeniedAddress, a, d)
		}
	}
	for _, in := range p.internal {
		if in.Contains(a) {
			if ap.Port() != p.port {
				return fmt.Errorf("%w: port %d is not the internal registry's %d", ErrDeniedAddress, ap.Port(),
					p.port)
			}
			return nil
		}
	}
	return p.public.Check(ap)
}

// lookupFunc resolves a host to IPv4 addresses.
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

// guardedDialer dials tcp4 only, resolving the host itself and judging
// every address twice: before the dial, and in the dialer's Control hook on
// the address actually connected, so a rebinding answer is judged at
// connect time. A denied address ends the dial; it is never retried on
// another address.
type guardedDialer struct {
	policy DialPolicy
	lookup lookupFunc
	dialer net.Dialer
	// rewrite maps a host:port before the dial, in tests only (registry
	// hosts to httptest listeners); nil in production.
	rewrite func(addr string) string
}

func newGuardedDialer(policy DialPolicy, lookup lookupFunc) *guardedDialer {
	if lookup == nil {
		lookup = defaultLookup
	}
	d := &guardedDialer{policy: policy, lookup: lookup}
	d.dialer = net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: d.control}
	return d
}

// control is the connect-time check on the resolved address.
func (d *guardedDialer) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable address %q", ErrDeniedAddress, address)
	}
	return d.policy.Check(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
}

// DialContext resolves addr's host and dials each IPv4 address in turn,
// stopping at the first denied one.
func (d *guardedDialer) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	if d.rewrite != nil {
		addr = d.rewrite(addr)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("%w: bad port %q", ErrDeniedAddress, portStr)
	}
	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = d.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no IPv4 address for %s", host)
	}
	var firstErr error
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a.Unmap(), uint16(port))
		if err := d.policy.Check(ap); err != nil {
			return nil, err
		}
		conn, err := d.dialer.DialContext(ctx, "tcp4", ap.String())
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, ErrDeniedAddress) {
			return nil, err
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// newTransport is the outbound transport: the guarded dialer, no proxy
// from the environment, system roots, TLS 1.2 at least.
func newTransport(d *guardedDialer, headerTimeout time.Duration) *http.Transport {
	return newTransportRoots(d, headerTimeout, nil)
}

// newTransportRoots is newTransport trusting only roots (nil: the system
// roots): a registry with its own CA trusts nothing else.
func newTransportRoots(d *guardedDialer, headerTimeout time.Duration, roots *x509.CertPool) *http.Transport {
	return &http.Transport{
		DialContext:           d.DialContext,
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	}
}
