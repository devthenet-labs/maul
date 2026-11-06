// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import (
	"testing"

	"github.com/devthenet-labs/maul/internal/maultool"
)

// TestE2EPlaces: the e2e build's scratch directory is the run's own, and
// its key file sits where the pod's does beside it (<workspace>/input
// beside <workspace>/scratch), so a workspace round's key reaches the tool
// pod; unset, both are the pod's.
func TestE2EPlaces(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := scratchDir(env(nil)); got != maultool.ScratchDir {
		t.Errorf("scratch %q, want the pod's", got)
	}
	if got := keyFile(env(nil)); got != maultool.KeyFile {
		t.Errorf("key file %q, want the pod's", got)
	}
	run := env(map[string]string{scratchEnv: "/tmp/job-1/scratch"})
	if got := scratchDir(run); got != "/tmp/job-1/scratch" {
		t.Errorf("scratch %q, want the run's", got)
	}
	if got := keyFile(run); got != "/tmp/job-1/input/tools-key" {
		t.Errorf("key file %q, want the run's input/tools-key", got)
	}
}
