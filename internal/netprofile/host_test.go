// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"math/rand"
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
)

func TestValidHostExamples(t *testing.T) {
	valid := []string{
		"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "api.nuget.org", "index.crates.io",
		"repo.maven.apache.org", "xn--p1ai.xn--p1ai", "example.a1", "a.b", "1.example.com", "x-y.example.com",
		"a--b.example.org", strings.Repeat("a", 63) + ".com",
		strings.Repeat("a.", 126) + "b", // 127 labels, 253 bytes
	}
	for _, h := range valid {
		if !ValidHost(h) {
			t.Errorf("ValidHost(%q) = false, want true", h)
		}
	}
	invalid := []string{
		"", "pypi", "localhost", "PyPI.org", "pypi.org.", ".pypi.org", "pypi..org", "*.npmjs.org", "pkg_x.example.com",
		"-a.example.com", "a-.example.com", "example.-a", "example.a-", "example.123", "example.1a", "1.2.3.4",
		"127.1", "2130706433", "0x7f.1", "0x7f000001", "017700000001", "::1", "[::1]", "pypi.org:443", "pypi org",
		"pypí.org", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 126) + "bc", // 254 bytes
	}
	for _, h := range invalid {
		if ValidHost(h) {
			t.Errorf("ValidHost(%q) = true, want false", h)
		}
	}
}

func TestIPLiteralExamples(t *testing.T) {
	for _, h := range []string{
		"1.2.3.4", "1.2.3.4.", "127.1", "2130706433", "0x7f.1", "0x7F000001", "017700000001", "0x", "0", "::1",
		"[::1]", "fe80::1%eth0", "::ffff:1.2.3.4", "999.1.1.1",
	} {
		if !IPLiteral(h) {
			t.Errorf("IPLiteral(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"a1.com", "1.2.3.4.5", "0x7g", "1.2.3.a", "pypi.org", "", ".", "1..2", "-1"} {
		if IPLiteral(h) {
			t.Errorf("IPLiteral(%q) = true, want false", h)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"PyPI.Org", "pypi.org"}, {"pypi.org", "pypi.org"}, {"XN--P1AI.example", "xn--p1ai.example"},
		{"1.2.3.4", "1.2.3.4"}, // bytes only: ValidHost refuses it
	} {
		got, err := NormalizeHost(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{
		"", "pypi.org.", ".pypi.org", "pypi..org", "pypi_x.org", "*.pypi.org", "pypi.org:443", "pypi.org/", "py pi.org",
		"Kube.example", // KELVIN SIGN lowercases to k under Unicode rules; refused, never folded
		"p\x00ypi.org", strings.Repeat("a", 254),
	} {
		if got, err := NormalizeHost(in); err == nil {
			t.Errorf("NormalizeHost(%q) = %q, want a refusal", in, got)
		}
	}
}

// tunnelLabelPool are the labels generated hosts are built from: real registry
// labels, punycode, alphanumeric and all-digit labels (legal anywhere but
// last), and hyphenated ones.
var tunnelLabelPool = []string{
	"pypi", "org", "files", "pythonhosted", "registry", "npmjs", "com", "io", "api", "nuget", "xn--p1ai", "a1",
	"123", "0x7f", "x-y", "a", "crates", "index", "static", "gradle", "plugins", "maven", "apache", "repo",
}

// genTunnelHost is a ValidHost name of 2-5 labels from tunnelLabelPool, the last
// one starting with a letter.
type genTunnelHost string

func (genTunnelHost) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 2 + r.Intn(4)
	labels := make([]string, n)
	for i := range labels {
		labels[i] = tunnelLabelPool[r.Intn(len(tunnelLabelPool))]
	}
	for labels[n-1][0] < 'a' || labels[n-1][0] > 'z' {
		labels[n-1] = tunnelLabelPool[r.Intn(len(tunnelLabelPool))]
	}
	return reflect.ValueOf(genTunnelHost(strings.Join(labels, ".")))
}

// genHostish is a string over the bytes hosts and addresses are made of,
// upper case and stray bytes included, so it lands on both sides of every
// rule: IP forms, trailing dots, empty labels, hyphen edges.
type genHostish string

func (genHostish) Generate(r *rand.Rand, _ int) reflect.Value {
	const alphabet = "0123456789abcdefxXAZz.-._:[]"
	n := r.Intn(20)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return reflect.ValueOf(genHostish(b))
}

func TestPropertyHostsExact(t *testing.T) {
	prop := func(h, s genTunnelHost) bool {
		hs := Hosts{string(s)}
		if hs.Allows(string(h)) != (h == s) {
			return false
		}
		return !hs.Allows("x."+string(s)) && !hs.Allows(string(s)+".evil.com") && !hs.Allows(strings.ToUpper(string(s)))
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func TestPropertyValidHostNeverIP(t *testing.T) {
	check := func(h string) bool {
		if !ValidHost(h) {
			return true
		}
		_, err := netip.ParseAddr(h)
		return err != nil && !IPLiteral(h)
	}
	if err := quick.Check(func(h genHostish) bool { return check(string(h)) }, quickConfig(20000)); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(func(h genTunnelHost) bool { return check(string(h)) }, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func TestPropertyNormalizeIdempotent(t *testing.T) {
	check := func(x string) bool {
		n, err := NormalizeHost(x)
		if err != nil {
			return true
		}
		again, err := NormalizeHost(n)
		if err != nil || again != n {
			return false
		}
		return ValidHost(n) == ValidHost(strings.ToLower(x))
	}
	if err := quick.Check(func(h genHostish) bool { return check(string(h)) }, quickConfig(20000)); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(func(h genTunnelHost) bool { return check(strings.ToUpper(string(h))) },
		quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

// TestTunnelHostPatternMatchesValidHost holds the RE2 pattern the chart
// schema will carry (plus its maxLength) to ValidHost: on a fixed corpus,
// generated hosts and generated near-hosts they agree. The pattern has no
// lookahead, which RE2, and so Helm's schema validator, cannot compile.
func TestTunnelHostPatternMatchesValidHost(t *testing.T) {
	re := regexp.MustCompile(TunnelHostPattern)
	for _, bad := range []string{"(?=", "(?!", "(?<"} {
		if strings.Contains(TunnelHostPattern, bad) {
			t.Fatalf("TunnelHostPattern holds %s, which RE2 cannot compile", bad)
		}
	}
	schema := func(h string) bool { return len(h) <= maxHostLen && re.MatchString(h) }
	check := func(h string) bool {
		if schema(h) != ValidHost(h) {
			t.Logf("%q: pattern %v, ValidHost %v", h, schema(h), ValidHost(h))
			return false
		}
		return true
	}
	for _, h := range []string{
		"xn--p1ai.xn--p1ai", "example.a1", "1.2.3.4", "example.123", "pypi", "a.b", "a-.b", "a.b-", "a.-b",
		strings.Repeat("a.", 126) + "b", strings.Repeat("a.", 126) + "bc", strings.Repeat("a", 63) + ".com",
		strings.Repeat("a", 64) + ".com",
	} {
		check(h)
	}
	if err := quick.Check(func(h genHostish) bool { return check(strings.ToLower(string(h))) },
		quickConfig(20000)); err != nil {
		t.Fatal(err)
	}
	if err := quick.Check(func(h genTunnelHost) bool { return check(string(h)) }, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}
