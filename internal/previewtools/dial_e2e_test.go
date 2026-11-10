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

// TestE2EWorkspaceSeams: the test build takes a workspace's exec-socket and
// trees directories from its environment, absolute only, as the e2e
// kubelet stages a workspace tool pod's volumes in directories of its own;
// unset, they stay the pod's.
func TestE2EWorkspaceSeams(t *testing.T) {
	for _, bad := range []map[string]string{{ExecDirEnv: "rel/exec"}, {TreesDirEnv: "rel/trees"}} {
		if _, err := SeamsFrom(envOf(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	s, err := SeamsFrom(envOf(map[string]string{ExecDirEnv: "/tmp/e/exec", TreesDirEnv: "/tmp/e/trees"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.ExecDir != "/tmp/e/exec" || s.TreesDir != "/tmp/e/trees" {
		t.Errorf("seams %+v, want the directories given", s)
	}
	if s, err = SeamsFrom(envOf(nil)); err != nil || s.ExecDir != DefaultExecDir || s.TreesDir != DefaultTreesDir ||
		s.HeadUID != HeadSandboxUID || s.BaseUID != BaseSandboxUID {
		t.Errorf("unset: seams %+v, %v; want the pod's directories and UIDs", s, err)
	}
	for _, bad := range []string{"x", "-1"} {
		if _, err := SeamsFrom(envOf(map[string]string{SandboxUIDEnv: bad})); err == nil {
			t.Errorf("accepted sandbox UID %q", bad)
		}
	}
	if s, err = SeamsFrom(envOf(map[string]string{SandboxUIDEnv: "1000"})); err != nil || s.HeadUID != 1000 ||
		s.BaseUID != 1000 {
		t.Errorf("sandbox UID: seams %+v, %v; want both sides' 1000", s, err)
	}
}
