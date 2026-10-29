// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package previewtools

import (
	"context"
	"errors"
	"net"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestE2ESeams: the test build maps only to loopback listeners, listens
// only on loopback, and refuses every address it was not given.
func TestE2ESeams(t *testing.T) {
	for _, bad := range []map[string]string{
		{DialMapEnv: `{"10.0.0.1:80":"10.0.0.2:80"}`},
		{DialMapEnv: `{"10.0.0.1:80":"example.com:80"}`},
		{DialMapEnv: `not json`},
		{ListenEnv: "0.0.0.0:8090"},
	} {
		if _, err := SeamsFrom(envOf(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	s, err := SeamsFrom(envOf(map[string]string{DialMapEnv: `{"10.0.0.1:80":"` + l.Addr().String() + `"}`,
		TerminationEnv: "/tmp/x", ListenEnv: "127.0.0.1:18090"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.TerminationPath != "/tmp/x" || s.ListenAddr != "127.0.0.1:18090" || !s.AllowLocal {
		t.Errorf("seams %+v", s)
	}
	conn, err := s.Dial(context.Background(), "tcp", "10.0.0.1:80")
	if err != nil {
		t.Fatalf("mapped address: %v", err)
	}
	_ = conn.Close()
	var dnsErr *net.DNSError
	if _, err := s.Dial(context.Background(), "tcp", "10.0.0.9:80"); !errors.As(err, &dnsErr) {
		t.Errorf("unmapped address: %v, want a DNS error", err)
	}
}
