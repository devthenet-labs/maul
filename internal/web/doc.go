// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package web is the status-server backend: the embedded status page SPA, a
// JSON projection of Findings and FindingRollups, the three human actions
// (approve, suspend, resume), and an SSE change signal that tells open
// browsers to refetch.
//
// The exposure contract is deliberately asymmetric: rollup statistics are
// public (GET /api/rollups and the SSE stream serve without a session),
// while the findings surface — list, detail, and every action — always
// requires an authenticated identity (internal/web/auth) holding the RBAC
// grants (internal/web/authz). The server never writes finding status and
// never moves a phase: approve records spec.approval and suspend/resume
// toggle spec.suspend, leaving every phase edge to its owning controller.
//
// With the Account page on (account.go, --account-enabled) the server is
// also the one writer of people's model credentials: a signed-in person
// sets, replaces or deletes their own, behind an access review on the
// virtual modelcredentials/self, through the CredentialStore port
// (credstore is its Kubernetes adapter), which can write a credential
// Secret but never read one. No route returns a stored credential.
//
// The wire types in data.go mirror the status page's TypeScript contract
// (ui/src/types.ts) field for field; keep the two in lockstep.
package web
