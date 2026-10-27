// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestE2EDialMap: the test build maps only to loopback listeners and
// refuses every address it was not given.
func TestE2EDialMap(t *testing.T) {
	for _, bad := range []string{`{"svc:80":"10.0.0.1:80"}`, `{"svc:80":"example.com:80"}`, `not json`} {
		if _, err := testDial(env(map[string]string{dialMapEnv: bad})); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	dial, err := testDial(env(map[string]string{dialMapEnv: `{"preview-p:80":"` + l.Addr().String() + `"}`}))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dial(context.Background(), "tcp", "preview-p:80")
	if err != nil {
		t.Fatalf("mapped address: %v", err)
	}
	_ = conn.Close()
	var dnsErr *net.DNSError
	if _, err := dial(context.Background(), "tcp", "other:80"); !errors.As(err, &dnsErr) {
		t.Errorf("unmapped address: %v, want a DNS error", err)
	}
	if got := terminationPath(env(map[string]string{terminationEnv: "/tmp/x"})); got != "/tmp/x" {
		t.Errorf("terminationPath = %q", got)
	}
}
