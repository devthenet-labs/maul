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
// (a mounted Secret, polled) and awsca (a minted CodeArtifact token).
//
// A profile holding registry grants also serves /r/<registry>/... (§2.2 of
// the step-2 spec; registry.go): the grant check, the ecosystem's grammar
// judged on the escaped path (npm.go, gorepo.go, pypi.go; the grammars
// themselves are netprofile's), the grant's package policy, the limits, the
// credential, then a forward to the registry's fixed base with RawPath kept
// and exactly the credential added, on the registry's own transport (its
// dial policy, AddrPolicy.ForRegistry, and its CA). Redirects are followed
// only to the registry's redirectHosts, without the credential, re-judged
// against the deny table on every hop (redirect.go). Every non-200 answer
// carries a body the proxy wrote, and every non-archive body is scanned
// for the credential sent (echo.go). A pip simple page is the one answer
// rewritten: pyindex regenerates it with every file link pointing back
// into the route, and the file route re-judges each such path statelessly
// with the same rules. A go grant's private modules are refused on the
// profile's /go route too, so they never fall through to Google.
//
// Step 3's CONNECT tunnel will judge a request target and the client's
// first TLS records with two pure parsers that nothing serves yet:
// ParseConnectTarget (connect.go: authority-form host:443 only) and
// ParseClientHello/ReadClientHello (clienthello.go: the SNI, ALPN and ECH
// signals of one ClientHello, reassembled across records and reads, never
// more than MaxHelloBytes).
//
// Each request emits one audit line (never a header or body) and the
// patchy.egress_proxy metrics.
// Attribution of a source IP to an agent pod (PodLookup) is audit garnish
// only and never decides anything. The core imports no Kubernetes package;
// the pod index adapter lives in podindex.
package egressproxy
