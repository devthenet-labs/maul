// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package usagewire is the wire contract of the egress broker's usage
// endpoint (per-user credentials design, revision 1, L2): what the broker
// serves on its separate usage listener and what the run controllers read
// from it when a run settles.
//
// The query is keyed by Job name: an agent Job runs one pod, named
// "<job>-<five characters>" (backoffLimit is 0), and the broker's ledger is
// keyed by the pod name and UID TokenReview reports. The answer carries the
// broker's boot time and each pod entry's firstSeen, so a controller can
// tell a ledger that saw the whole pod (the broker booted before the Job
// existed) from one that saw part of it, and can drop an entry seen before
// its Job was created (a recycled pod name). inflight is how many of the
// pod's requests have not yet settled: a controller waits for zero before it
// calls the figure final.
//
// Callers authenticate with a projected ServiceAccount token bound to
// Audience, sent as a bearer token. The broker accepts it only on the usage
// listener and only for the controller ServiceAccounts it is configured
// with, so neither token opens the other's surface.
//
// The package imports only the standard library (a test pins that), so the
// broker and every controller can share it without linking each other.
package usagewire
