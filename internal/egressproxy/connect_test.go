// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
)

func TestParseConnectTargetExamples(t *testing.T) {
	cases := []struct {
		target string
		host   string
		err    error
	}{
		{"pypi.org:443", "pypi.org", nil},
		{"files.pythonhosted.org:443", "files.pythonhosted.org", nil},
		// Shape only: case and grammar are the host policy's.
		{"PyPI.org:443", "PyPI.org", nil},
		{"1.2.3.4:443", "1.2.3.4", nil},
		{"example.com.:443", "example.com.", nil},
		{strings.Repeat("a", MaxConnectTargetLen-4) + ":443", strings.Repeat("a", MaxConnectTargetLen-4), nil},

		{"pypi.org:80", "", ErrConnectPort},
		{"pypi.org:8443", "", ErrConnectPort},
		{"pypi.org:0443", "", ErrConnectPort},
		{"pypi.org:+443", "", ErrConnectPort},
		{"pypi.org:", "", ErrConnectPort},
		{"pypi.org:https", "", ErrConnectPort},

		{"", "", ErrConnectTarget},
		{"pypi.org", "", ErrConnectTarget},
		{":443", "", ErrConnectTarget},
		{"a:b:443", "", ErrConnectTarget},
		{"pypi.org:443:443", "", ErrConnectTarget},
		{"https://pypi.org:443", "", ErrConnectTarget},
		{"pypi.org:443/", "", ErrConnectTarget},
		{"/simple/six/", "", ErrConnectTarget},
		{"*", "", ErrConnectTarget},
		{"user@pypi.org:443", "", ErrConnectTarget},
		{"[::1]:443", "", ErrConnectTarget},
		{"::1:443", "", ErrConnectTarget},
		{"pypi.org\r\nX: y:443", "", ErrConnectTarget},
		{"pypi .org:443", "", ErrConnectTarget},
		{"pypi\x00.org:443", "", ErrConnectTarget},
		{"pypi\x7f.org:443", "", ErrConnectTarget},
		{"pyp\xc3\xaf.org:443", "", ErrConnectTarget},
		{strings.Repeat("a", MaxConnectTargetLen-3) + ":443", "", ErrConnectTarget},
	}
	for _, c := range cases {
		host, err := ParseConnectTarget(c.target)
		if host != c.host || !errors.Is(err, c.err) {
			t.Errorf("ParseConnectTarget(%q) = %q, %v; want %q, %v", c.target, host, err, c.host, c.err)
		}
	}
}

// genHost is a lowercase LDH host name of 2-4 labels, the shape every
// allowlisted host has.
type genHost string

func (genHost) Generate(r *rand.Rand, _ int) reflect.Value {
	labels := make([]string, 2+r.Intn(3))
	for i := range labels {
		l := randString(r, lowerDigits+"-", 1, 20)
		l = strings.Trim(l, "-")
		if l == "" {
			l = "x"
		}
		labels[i] = l
	}
	last := labels[len(labels)-1]
	labels[len(labels)-1] = string("abcdefghijklmnopqrstuvwxyz"[r.Intn(26)]) + last
	return reflect.ValueOf(genHost(strings.Join(labels, ".")))
}

// genTarget is a CONNECT target drawn from an alphabet rich in the bytes
// the parser judges, so both outcomes are common.
type genTarget string

func (genTarget) Generate(r *rand.Rand, _ int) reflect.Value {
	const chars = "ab.-:443/@[]%\x00 \r\n\x7f\xff*"
	var b strings.Builder
	if r.Intn(2) == 0 {
		b.WriteString(string(genHost("").Generate(r, 0).Interface().(genHost)))
	}
	b.WriteString(randString(r, chars, 0, 8))
	if r.Intn(2) == 0 {
		b.WriteString(":443")
	}
	return reflect.ValueOf(genTarget(b.String()))
}

// TestPropertyConnectTargetHostRoundTrip: every allowlist-shaped host plus
// ":443" parses back to exactly that host, and with any other port is a
// port refusal, never a shape one.
func TestPropertyConnectTargetHostRoundTrip(t *testing.T) {
	prop := func(h genHost, port uint16) bool {
		host, err := ParseConnectTarget(string(h) + ":443")
		if err != nil || host != string(h) {
			t.Logf("ParseConnectTarget(%q) = %q, %v", string(h)+":443", host, err)
			return false
		}
		// An odd port is written with a leading zero, so "0443" is
		// generated too.
		p := strconv.Itoa(int(port))
		if port%2 == 1 {
			p = "0" + p
		}
		if p == TunnelPort {
			return true
		}
		_, err = ParseConnectTarget(string(h) + ":" + p)
		return errors.Is(err, ErrConnectPort)
	}
	if err := quick.Check(prop, quickConfig(3000)); err != nil {
		t.Error(err)
	}
}

// TestPropertyConnectTargetExact: an accepted target is exactly the host it
// returned plus ":443", and that host is non-empty printable ASCII with no
// byte that could make it anything but a reg-name (no '/', '@', '[', ']',
// ':', space or control byte).
func TestPropertyConnectTargetExact(t *testing.T) {
	prop := func(target genTarget) bool {
		host, err := ParseConnectTarget(string(target))
		if err != nil {
			return host == "" && (errors.Is(err, ErrConnectTarget) || errors.Is(err, ErrConnectPort))
		}
		if host == "" || host+":"+TunnelPort != string(target) || strings.ContainsAny(host, "/@[]: ") {
			t.Logf("ParseConnectTarget(%q) = %q", target, host)
			return false
		}
		for i := range len(host) {
			if host[i] < 0x21 || host[i] > 0x7e {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Error(err)
	}
}
