// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !e2edial

package main

import "github.com/devthenet-labs/maul/internal/maultool"

// scratchDir is the scratch directory maul-tool reads and writes under: a
// released maul-tool's is the evaluator pod's fixed /work/scratch, whatever
// its environment says (scratch_e2e.go is the e2e suite's seam).
func scratchDir(func(string) string) string { return maultool.ScratchDir }
