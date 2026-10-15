// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package egressproxy is the engine of cmd/egress-proxy: a GOPROXY-protocol
// reverse proxy to proxy.golang.org and sum.golang.org that serves operator
// network profiles (internal/netprofile), one listener per profile.
//
// Identity is the listener: a profile's port is reachable only from agent
// pods carrying that profile's label (NetworkPolicy on both sides), so the
// handler for a port trusts the port and judges only the request. Every
// request runs one pipeline, in order: suspended, method, body, path shape,
// parse (path.go, pure), the profile's module allowlist and version gate,
// the limits, then a forward to a fixed upstream base. Every refusal of a
// well-formed GET the go command could send is 410 with a one-line reason,
// because the go command moves on to the next GOPROXY entry only on 404 or
// 410 and aborts on anything else; a refusal never contacts the upstream.
//
// Outbound, no inbound header is copied, redirects are never followed, only
// 200, 404 and 410 pass through, only three response headers are copied,
// bodies are size-capped per kind, and the dialer refuses every non-public
// or non-443 address at connect time (dial.go), so a rebinding answer is
// judged on the address actually dialled.
//
// The proxy caches nothing, and its step-1 routes hold no credential. Step
// 2's credentialed registry routes take theirs from a CredentialSource
// (credential.go): a Credential's value sits in unexported fields and every
// formatter prints it as [redacted]. The sources are adapters: credfile
// (a mounted Secret, polled) and awsca (a minted CodeArtifact token). Each request emits one
// audit line (never a header or body) and the patchy.egress_proxy metrics.
// Attribution of a source IP to an agent pod (PodLookup) is audit garnish
// only and never decides anything. The core imports no Kubernetes package;
// the pod index adapter lives in podindex.
package egressproxy
