// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package previewtester is the engine of cmd/preview-tester, the binary
// preview-controller runs as a Job inside a Preview's slot to observe the
// approved http acceptance criteria against the Preview's own components.
// It observes; it never judges: intent-controller runs acceptance.Judge on
// what preview-controller reads back from the tester's log.
//
// Spec (spec.go) is its one input, rendered by preview-controller as the
// literal environment variable EnvSpec (at most MaxSpecBytes): the
// criteria, the round, attempt and random run token their {{run}}
// placeholders take, and per previewed repository key the Service address
// its requests go to. ParseSpec refuses unknown fields, any criterion but
// http, and a target that is not "<DNS name>:<port>": a criterion never
// chooses a host.
//
// Client (client.go) sends each request acceptance.BuildRequest built, as
// built: plain HTTP to the spec's address, the escaped path and query
// exactly as observed, never through the environment's proxy, never
// following a redirect (the redirect is the response), with no cookie jar,
// no compression, a fresh connection, RequestTimeout per request and the
// body read to at most acceptance.MaxResponseBytes (too-large past it). A
// request that gets no response is a transport class, never an error.
//
// Run (run.go) drives acceptance.Observe (which retries a failing
// criterion once under a fresh {{run}}) over the criteria in plan order and
// writes the MAUL-VERIFY v1 wire format; WriteTermination repeats the
// summary as the container's termination message, so a truncated log is
// detected.
//
// The package imports only the standard library and internal/acceptance
// (a test pins that).
package previewtester
