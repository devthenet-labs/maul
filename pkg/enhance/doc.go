// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package enhance is maul's public plugin seam for context enhancement. An
// Enhancer augments a newly opened security-finding issue with first/third
// party context — CMDB ownership, associated infrastructure, runbooks — as a
// comment (the issue body is owned by the accumulator) plus structured
// attributes the pipeline consumes, most importantly the owners used for
// assignment.
//
// Implementations may live outside this repository; the exported API is
// deliberately self-contained and depends on nothing under internal/.
// Enhancers run as an ordered chain and must be best-effort: a failing
// enhancer is logged and skipped, never blocking the pipeline.
package enhance
