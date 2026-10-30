// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package runnercfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProbeDNSFrom: the first usable nameserver line of resolv.conf becomes
// <ip>:53; comments and other directives are skipped, and a file naming no
// nameserver, or only one the probe could not judge from another pod (a
// loopback or unspecified address), is an error rather than a clean verdict.
func TestProbeDNSFrom(t *testing.T) {
	tests := []struct {
		name    string
		conf    string
		want    string
		wantErr bool
	}{
		{"kubelet's file", "search maul.svc.cluster.local svc.cluster.local cluster.local\n" +
			"nameserver 10.96.0.10\noptions ndots:5\n", "10.96.0.10:53", false},
		{"first of several", "nameserver 172.20.0.10\nnameserver 10.96.0.11\n", "172.20.0.10:53", false},
		{"comments and blanks", "# generated\n; also a comment\n\n  nameserver\t10.0.0.10  \n", "10.0.0.10:53", false},
		{"ipv6", "nameserver fd00:10:96::a\n", "[fd00:10:96::a]:53", false},
		{"ipv6 with a zone", "nameserver fe80::1%eth0\n", "[fe80::1]:53", false},
		{"commented-out nameserver", "#nameserver 10.96.0.10\n", "", true},
		{"no nameserver", "search cluster.local\n", "", true},
		{"empty", "", "", true},
		{"not an address", "nameserver kube-dns\n", "", true},
		{"loopback", "nameserver 127.0.0.53\n", "", true},
		{"unspecified", "nameserver 0.0.0.0\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := probeDNSFrom(strings.NewReader(tt.conf))
			if (err != nil) != tt.wantErr {
				t.Fatalf("probeDNSFrom = %q, %v; want error %v", got, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("probeDNSFrom = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestProbeDNSReadsTheFile: ProbeDNS reads the path it is given, and a
// missing file is an error.
func TestProbeDNSReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 10.96.0.10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ProbeDNS(path); err != nil || got != "10.96.0.10:53" {
		t.Errorf("ProbeDNS = %q, %v; want 10.96.0.10:53", got, err)
	}
	if _, err := ProbeDNS(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("ProbeDNS of a missing file succeeded")
	}
}
