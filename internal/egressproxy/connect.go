// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"strings"
)

// MaxConnectTargetLen bounds a CONNECT request target (§2.3 of the step-3
// spec): a 253-byte host, the colon and the port, with room to spare.
const MaxConnectTargetLen = 260

// TunnelPort is the only port a CONNECT may name.
const TunnelPort = "443"

// The two ways ParseConnectTarget refuses. ErrConnectTarget is a request
// that is not authority-form at all (the tunnel answers 400 bad_request);
// ErrConnectPort is a well-formed target naming another port (403 port, a
// policy refusal).
var (
	ErrConnectTarget = errors.New("egress-proxy: the CONNECT target is not host:443 authority-form")
	ErrConnectPort   = errors.New("egress-proxy: only port 443 may be tunnelled")
)

// ParseConnectTarget splits a CONNECT request's target (http.Request's
// RequestURI, exactly as it arrived) into its host. It accepts authority-form
// "host:443" only: at most MaxConnectTargetLen bytes of printable ASCII, no
// '/' (so no scheme and no path), no '@' (no userinfo), no '[' or ']' (no
// IPv6 literal), exactly one ':', a non-empty host and the port string
// exactly "443" (not "0443", not "+443").
//
// It judges shape only. The host comes back as sent, not lowercased and not
// checked against the host grammar: refusing IP literals, normalising and
// matching the profile's hosts is the tunnel's host policy, which runs next
// on what this returns.
func ParseConnectTarget(requestURI string) (string, error) {
	if requestURI == "" || len(requestURI) > MaxConnectTargetLen {
		return "", ErrConnectTarget
	}
	for i := range len(requestURI) {
		c := requestURI[i]
		if c <= ' ' || c >= 0x7f || c == '/' || c == '@' || c == '[' || c == ']' {
			return "", ErrConnectTarget
		}
	}
	host, port, ok := strings.Cut(requestURI, ":")
	if !ok || host == "" || strings.Contains(port, ":") {
		return "", ErrConnectTarget
	}
	if port != TunnelPort {
		return "", ErrConnectPort
	}
	return host, nil
}
