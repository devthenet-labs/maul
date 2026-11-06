// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import (
	"path/filepath"

	"github.com/devthenet-labs/maul/internal/maultool"
)

// scratchEnv names the e2e suite's scratch directory. The suite's fake
// kubelet runs the evaluator in a temporary workspace, where /work/scratch
// does not exist, so the e2edial build reads it from here instead. Never in
// a released binary (scratch.go).
const scratchEnv = "MAUL_E2E_SCRATCH_DIR"

// scratchDir is scratchEnv when set, else the pod's fixed /work/scratch.
func scratchDir(getenv func(string) string) string {
	if d := getenv(scratchEnv); d != "" {
		return d
	}
	return maultool.ScratchDir
}

// keyFile is the per-evaluation key's file: beside the scratch directory
// scratchEnv names, where the pod's sits beside /work/scratch
// (<workspace>/input/tools-key, which agent-runner writes), else the pod's.
func keyFile(getenv func(string) string) string {
	if d := getenv(scratchEnv); d != "" {
		return filepath.Join(filepath.Dir(d), "input", "tools-key")
	}
	return maultool.KeyFile
}
