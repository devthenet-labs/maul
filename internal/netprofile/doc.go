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
// Step 2 adds credentialed registries. A Registry (registry.go) is an
// operator-defined upstream (egressProxy.registries.<name>): its ecosystem
// (go, npm or pypi), https base URL, credential type (never a Secret's name
// or value), file and redirect hosts, optional internal admission and, for
// go, the checksum choice. ParseRegistries reads the proxy's registries
// JSON. A profile holds Grants (grant.go), at most one per ecosystem, each
// with an explicit package policy (an allowlist, or "registry"); a profile
// may then omit its public route. CheckGrants holds the two documents to
// each other.
//
// The grammars the proxy judges registry paths by are pure functions here
// (pkgname.go): NPMName, NPMPath and NPMVersion for npm (the scope
// separator is the only escape admitted), PyNormalize, PyProjectOfFile and
// PyFilePath for pip (only %2B and %21 admitted, no empty, . or ..
// segment). DeniedHost and DeniedPattern (denied.go) are the hard deny
// table every registry, file and redirect host passes: forges, model APIs,
// the Kubernetes API, in-cluster names and IP literals outside a
// registry's own internal CIDRs.
//
// Step 3 adds the CONNECT tunnel. A profile may list Hosts, exact public
// host names (ValidHost: lowercase LDH labels, the last starting with a
// letter, so no IP form passes; NormalizeHost and IPLiteral judge what a
// client sends), with route "tunnel" (the tunnel alone) or "go" (both on
// the one port); Clients, the package-manager presets (client.go) the job
// controllers apply beside it; and Upload, per-profile upload caps.
// DeniedTunnelHost is DeniedHost's table applied to every suffix of the
// host, plus the tunnel's own rows: more forges and model APIs, the managed
// Kubernetes control planes, special-use names and package-publish hosts.
// A tunnel host may not be a host of one of the profile's grants, which
// would bypass the grant's package policy, and a client preset may not sit
// beside a grant for the same package manager.
//
// The package imports only the standard library (a test pins that), so the
// proxy, the controllers and the jobs package can share it without linking
// anything else.
package netprofile
