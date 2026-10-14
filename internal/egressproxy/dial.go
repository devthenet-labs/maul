// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"
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
	policy AddrPolicy
	lookup lookupFunc
	dialer net.Dialer
}

func newGuardedDialer(policy AddrPolicy, lookup lookupFunc) *guardedDialer {
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
	return &http.Transport{
		DialContext:           d.DialContext,
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	}
}
