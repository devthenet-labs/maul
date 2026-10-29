// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package cdpshim is the keyed CDP shim of a tool pod's browser container
// (DESIGN 4.2 "The CDP hole, closed"; SPEC S1-07, SP-1 (c)): the only way
// to drive the pod's Chromium.
//
// The three containers of a tool pod share one network namespace, so a TCP
// debugging port would be open to the sandbox container's code, and
// Chromium's own --remote-allow-origins check stops page JS but not a
// client that sends no Origin. So Chromium never opens one: each browser is
// started with --remote-debugging-pipe (fd 3 it reads, fd 4 it writes,
// NUL-delimited JSON; Chromium) and --remote-allow-origins left unset, and
// the shim serves CDP over HTTP to a caller that presents the
// per-evaluation key in the KeyHeader request header (compared in constant
// time) and sends no Origin header at all. Both are checked before any
// upgrade, so a refusal is a plain status and starts nothing. Only the tools
// container holds the key; page JS cannot set the header.
//
// Two endpoints: GET /json/version (what Playwright's connectOverCDP reads
// first, naming the WebSocket at the address the caller dialled) and GET
// /devtools/browser/<id>, a WebSocket. Each accepted WebSocket gets a
// Chromium of its own (Q3: no CDP multiplexing, a fresh browser per check),
// at most MaxLive at once (a further one is refused 503), killed with its
// whole process group when the client disconnects, when Chromium exits, and
// after Idle without a message either way (SP-1: 10 minutes, so the agent's
// CLI session survives its thinking). A client message holding a NUL would
// be two messages on the pipe and ends the connection.
//
// Chromium runs with --no-sandbox: RuntimeDefault seccomp refuses its
// namespace sandbox (SP-3; an opt-in Localhost profile is deferred to the
// owner), in a container that holds nothing.
//
// The package imports only the standard library and
// golang.org/x/net/websocket (a test pins that), and never logs the key.
package cdpshim
