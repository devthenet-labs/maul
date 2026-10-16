// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package usageview is the pure projection of UsageRollups for human readers:
// the status page's Usage views and `patchy get usage` render the one report
// it builds, so the two can never disagree about a figure.
//
// A report covers a window of whole UTC days ending today. It answers for one
// person (their user scope, stacked by the Projects of their user-project
// scopes and by run kind), one Project, one person within one Project, or
// everyone (every user scope, every project scope). Every day of the window
// is listed, zero-filled. A day's per-Project segments are the Projects'
// figures plus a "no project" remainder (Finding and evaluation runs belong
// to no Project); per-kind segments come from the buckets' kind split.
//
// Trusted figures are the broker's metered tokens and their list-rate cost.
// A subscription's list-rate equivalent is carried as notional cost, never as
// money spent; the broker's worst-case unmetered shortfall and the pods' own
// reported figures are carried apart and never folded into a trusted total.
//
// It imports the standard library and api/v1alpha1, and nothing else (a test
// pins that): no client.
package usageview
