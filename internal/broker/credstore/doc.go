// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package credstore is the egress broker's adapter for per-user model
// credentials (design L3): the broker.CredentialStore port over the API
// server. Two informers in one controller-runtime cache, each confined to
// its namespace, keep the ModelCredential records (release namespace) and
// the credential Secrets' metadata only (credentials namespace: names,
// labels and resourceVersions, never data) in memory; a Secret's data is
// read live, by name, only when a request needs it. Health is written as a
// JSON merge patch of the status subresource with the record's
// resourceVersion as a precondition, so a write over a record that changed
// since it was read is a conflict, never a lost update.
//
// It needs exactly the chart's broker grants under
// egressBroker.userCredentials.mode: get, list and watch on
// modelcredentials and patch on modelcredentials/status in the release
// namespace, and get, list and watch on secrets in the credentials
// namespace.
package credstore
