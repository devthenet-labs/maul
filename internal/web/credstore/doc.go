// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package credstore is the status server's Kubernetes adapter of the
// Account page's credential store (web.CredentialStore; per-user
// credentials design L3). It writes a person's credential Secret in the
// credentials namespace and their ModelCredential record in the release
// namespace, both under the names derived from the owner and provider, and
// never reads a Secret: the chart grants the status server create, update
// and delete on Secrets there and nothing else (not even patch, whose
// response is the whole stored object). A Secret is created, or on
// AlreadyExists replaced by an unconditional update, whose response holds
// only what was sent. Records are read through the uncached API reader.
//
// Order matters for the broker. The Secret is written first, so a record
// never points at a credential that is not there yet; then the record's
// spec; then its status: the new fingerprint, and health reset to Unknown
// unless the broker already judged this very Secret version (its probe can
// land between the two writes, and its verdict must not be undone). The
// admission policies hold every write here to the derived names, the
// owner-hash label and one bounded data key.
package credstore
