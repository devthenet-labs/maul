// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package toolexec is the exec shim of a tool pod's sandbox container
// (DESIGN 2.1, 4.2; SPEC S1-08): the tiny maul server through which the
// tools container runs a program in sandbox, the one container of the pod
// where agent-written code runs.
//
// The shim (cmd/tool-exec) listens on a unix socket in an emptyDir only
// tools and sandbox mount (SocketPath), and serves one call: POST ExecPath
// with a toolwire.ExecRequest. It writes the request's files under the
// scratch directory (ScratchDir; never outside it, by name or through a
// symlink, os.Root), then runs argv directly, never through a shell unless
// argv names one, as the sandbox container's own UID, in the scratch
// directory, with a fixed environment the shim chooses (its own is
// withheld). One program runs at a time, for at most Timeout; past it the
// program is killed with every process of its group, as is anything a
// finished program left behind. stdout and stderr are kept together, in
// order, at most MaxOutputBytes of valid UTF-8, a longer output ending in
// toolwire.TruncationMarker and a killed one in TimeoutMarker.
//
// The shim records nothing: the tools container writes the ledger entry
// from the Result. Code in sandbox can lie about what its own program
// printed (it runs as the shim's UID), which DESIGN 4.2 accepts; it cannot
// write the tools container's log.
//
// The package imports only the standard library and internal/toolwire (a
// test pins that).
package toolexec
