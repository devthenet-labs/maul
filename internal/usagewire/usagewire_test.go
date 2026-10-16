// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagewire

import (
	"go/build"
	"strings"
	"testing"
)

func TestValidJobName(t *testing.T) {
	for name, want := range map[string]bool{
		"patchy-0a1b2c3d4e-inv-a1":  true,
		"patchy-0a1b2c3d4e-evl":     true,
		"":                          false,
		"Patchy":                    false,
		"-a":                        false,
		"a-":                        false,
		"a/b":                       false,
		"a.b":                       false,
		strings.Repeat("a", 57):     true,
		strings.Repeat("a", 58):     false,
		"patchy-0a1b2c3d4e-inv-a1/": false,
	} {
		if got := ValidJobName(name); got != want {
			t.Errorf("ValidJobName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPodOfJob(t *testing.T) {
	const job = "patchy-0a1b2c3d4e-inv-a1"
	for pod, want := range map[string]bool{
		job + "-x7k2p":   true,
		job + "-x7k2":    false,
		job + "-x7k2pq":  false,
		job + "-X7K2P":   false,
		job + "x-7k2p":   false,
		job + "1-x7k2p":  false, // a different Job, "...-a11"
		"other-x7k2p":    false,
		job + "-x7k2p-b": false,
	} {
		if got := PodOfJob(pod, job); got != want {
			t.Errorf("PodOfJob(%q) = %v, want %v", pod, got, want)
		}
	}
}

// TestStdlibOnly pins the package to the standard library, so the broker
// and the controllers share it without linking each other.
func TestStdlibOnly(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
			t.Errorf("usagewire imports %s; it must stay stdlib-only", imp)
		}
	}
}
