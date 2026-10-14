// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package netprofile is the pure core of operator network profiles (the
// egress-proxy design, step 1): a profile names a dependency route (only
// "go" in step 1), the egress-proxy port that serves it, the repositories it
// applies to and the module-path prefixes the proxy will forward for it.
//
// Profiles are the operator's grant, set in chart values and passed to the
// egress proxy and the two job controllers as one JSON document (Parse).
// Repositories declare nothing. A Set answers which profile, if any, a
// repository gets (Set.For): its host must equal the profile's and one of the
// profile's patterns must match the lowercased owner/name ANCHORED, so
// devthenet-labs/patchy never matches devthenet-labs/patchy-target. This
// deliberately differs from a Forge's unanchored repository match. A
// repository matching two profiles gets neither (fail closed).
//
// Prefixes.Allows is the proxy's module allowlist: a prefix admits itself
// and the paths below it on path-element boundaries, so golang.org/x never
// admits golang.org/xyz.
//
// The package imports only the standard library (a test pins that), so the
// proxy, the controllers and the jobs package can share it without linking
// anything else.
package netprofile
