// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Limits on a tunnel host and a profile's host list; the chart schema
// carries the same numbers (step 3).
const (
	MaxTunnelHosts  = 64
	maxLabelLen     = 63
	minTunnelLabels = 2
)

// TunnelHostPattern is the tunnel host grammar as one RE2 pattern, the one
// the chart schema carries (with maxLength 253, since RE2 has no lookahead
// to bound the length): two or more lowercase LDH labels, none starting or
// ending with a hyphen, the last one starting with a letter. ValidHost is
// the same rule, written out; a test holds the two to each other.
const TunnelHostPattern = `^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`

// ErrBadHost is wrapped by every NormalizeHost refusal.
var ErrBadHost = errors.New("not a host name")

// ValidHost reports whether h is a tunnel host as written: lowercase LDH
// labels (letters, digits, hyphens), 2 to 127 of them, each 1 to 63 bytes
// and neither starting nor ending with a hyphen, at most 253 bytes in all,
// and a last label that starts with a letter. Punycode (xn--) labels pass;
// a wildcard, an underscore, a trailing dot or an uppercase letter does
// not. Because the last label starts with a letter, no form of IP address
// (dotted, decimal, hex, octal or short) is a valid host.
func ValidHost(h string) bool {
	if len(h) == 0 || len(h) > maxHostLen {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < minTunnelLabels {
		return false
	}
	for _, l := range labels {
		if !ldhLabel(l) {
			return false
		}
	}
	last := labels[len(labels)-1]
	return last[0] >= 'a' && last[0] <= 'z'
}

// ldhLabel reports whether l is one lowercase LDH label.
func ldhLabel(l string) bool {
	if l == "" || len(l) > maxLabelLen || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := range len(l) {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// NormalizeHost lowercases raw's ASCII letters and refuses what a lowercase
// rewrite cannot fix: an empty name, a trailing dot, an empty label, or
// any byte that is not a letter, digit, hyphen or dot (non-ASCII included,
// so no Unicode case folding can turn another character into an ASCII
// letter). The result still has to pass ValidHost; NormalizeHost judges
// bytes, ValidHost the grammar.
func NormalizeHost(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty", ErrBadHost)
	}
	if len(raw) > maxHostLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrBadHost, maxHostLen)
	}
	b := []byte(raw)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return "", fmt.Errorf("%w: byte 0x%02x is not a letter, digit, hyphen or dot", ErrBadHost, c)
		}
	}
	h := string(b)
	if strings.HasSuffix(h, ".") {
		return "", fmt.Errorf("%w: a trailing dot", ErrBadHost)
	}
	if slices.Contains(strings.Split(h, "."), "") {
		return "", fmt.Errorf("%w: an empty label", ErrBadHost)
	}
	return h, nil
}

// IPLiteral reports whether h is an IP address in any form a resolver or
// inet_aton would take: what net/netip parses (IPv4, IPv6, with a zone),
// a bracketed IPv6 literal, or one to four dot-separated numbers, each
// decimal, octal (a leading 0) or hex (0x), with an optional trailing dot
// (so 127.1, 2130706433, 0x7f.1 and 017700000001 are all addresses). It
// errs towards yes: a numeric part out of range still counts, since the
// question is only ever whether to refuse h.
func IPLiteral(h string) bool {
	if strings.HasPrefix(h, "[") {
		return true
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	parts := strings.Split(strings.TrimSuffix(h, "."), ".")
	if len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if !numericPart(p) {
			return false
		}
	}
	return true
}

// numericPart reports whether p is one inet_aton number: decimal digits,
// or 0x/0X and hex digits (possibly none).
func numericPart(p string) bool {
	if p == "" {
		return false
	}
	digits, hex := p, false
	if rest, ok := strings.CutPrefix(strings.ToLower(p), "0x"); ok {
		digits, hex = rest, true
	}
	for i := range len(digits) {
		c := digits[i]
		lower := c | 0x20 // ASCII lowercase; digits are unaffected
		ok := (c >= '0' && c <= '9') || (hex && lower >= 'a' && lower <= 'f')
		if !ok {
			return false
		}
	}
	return true
}

// Hosts is a profile's tunnel allowlist: exact host names, each already
// ValidHost. There is no suffix or wildcard match.
type Hosts []string

// Allows reports whether h, already normalized, is exactly one of the
// hosts: pkg.example.com never admits x.pkg.example.com or
// pkg.example.com.evil.example.
func (hs Hosts) Allows(h string) bool { return slices.Contains(hs, h) }
