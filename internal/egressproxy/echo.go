// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"errors"
)

// errCredentialEcho is a response that carries the credential sent.
var errCredentialEcho = errors.New("egress-proxy: the upstream response carries the registry credential")

// echoGuard is the streaming credential-echo scanner (§2.7 of the step-2
// spec): it holds back the last len(longest needle)-1 bytes it has been fed,
// so a needle split across two reads is still seen whole, and nothing that
// could be the start of a needle is released before the bytes after it
// have been checked. Each feed searches only the held-back tail plus the
// new bytes, so the cost is linear in the body.
type echoGuard struct {
	needles [][]byte
	hold    int
	pending []byte
}

// newEchoGuard scans for needles; an empty needle is ignored. A guard
// without needles passes everything through.
func newEchoGuard(needles [][]byte) *echoGuard {
	g := &echoGuard{}
	for _, n := range needles {
		if len(n) == 0 {
			continue
		}
		g.needles = append(g.needles, n)
		if len(n)-1 > g.hold {
			g.hold = len(n) - 1
		}
	}
	return g
}

// feed adds p and returns the bytes now safe to write, or
// errCredentialEcho when a needle has appeared. The returned slice is the
// caller's to keep.
func (g *echoGuard) feed(p []byte) ([]byte, error) {
	if len(g.needles) == 0 {
		return p, nil
	}
	g.pending = append(g.pending, p...)
	for _, n := range g.needles {
		if bytes.Contains(g.pending, n) {
			g.pending = nil
			return nil, errCredentialEcho
		}
	}
	if len(g.pending) <= g.hold {
		return nil, nil
	}
	cut := len(g.pending) - g.hold
	out := append([]byte(nil), g.pending[:cut]...)
	g.pending = append(g.pending[:0], g.pending[cut:]...)
	return out, nil
}

// flush returns the held-back tail at the end of the body: every byte of
// it has been searched with all that came before.
func (g *echoGuard) flush() []byte {
	out := g.pending
	g.pending = nil
	return out
}

// containsAny reports whether b holds any needle (a buffered body's
// one-shot check).
func containsAny(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if len(n) > 0 && bytes.Contains(b, n) {
			return true
		}
	}
	return false
}
