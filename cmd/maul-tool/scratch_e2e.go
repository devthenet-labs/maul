// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build e2edial

package main

import "github.com/devthenet-labs/maul/internal/maultool"

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
