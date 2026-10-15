// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package credfile is the egress proxy's file-backed credential adapter: a
// static registry credential (bearer token, or basic username and
// password) read from the projected Secret volume kubelet mounts at
// <dir>/<registry>/<key>, behind egressproxy.CredentialSource.
//
// A poller re-reads every registry's files on an interval and swaps the
// in-memory credential atomically when the bytes changed and the new value
// validates, so a Secret rotation reaches the proxy without a restart
// (kubelet's atomic "..data" symlink swap included; subPath mounts are
// never used, since kubelet does not refresh them). An invalid or missing
// file keeps the last good value and logs credential_invalid once per
// change; with no good value yet the registry's route answers 503. An
// upstream 401 calls Invalidate, which re-reads at once, at most once per
// poll interval per registry.
//
// The proxy has no Kubernetes API access for Secrets: kubelet mounts them,
// and this package only reads files. No value is ever logged; the
// lifecycle lines carry the registry and the outcome only.
package credfile
