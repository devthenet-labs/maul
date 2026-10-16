// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package usagerollup is the run controllers' side of durable usage (per-user
// credentials design, revision 1, L2): each run's own controller folds the
// run into its UsageRollups once it settles, exactly once, before the run
// can be deleted.
//
// The four controllers that launch agent Jobs (investigation, remediation,
// intent and evaluation) share it:
//
//   - A run created with spec.attributedTo (--attribution-mode attribute)
//     is created with v1alpha1.FinalizerUsageRollup too (Finalizers). A run
//     without it (one from before the feature, or created under mode off)
//     is not applicable: it is never aggregated and never held.
//   - The run is due once it is settled (Complete or Failed) and its usage
//     is final (or the meter is off, or the run never reported a stage, so
//     nothing will ever make it final), or at once when it is being deleted
//     (its usage then counts as whatever it has, usually reported).
//   - Settle folds it into the user scope (the person, or "org"), and for an
//     intent's run into the user x project and project scopes, under each
//     rollup's exactly-once ledger keyed u:<run uid> (stats.ApplyUsage),
//     bucketed by the run's UTC completion, then marks the run
//     v1alpha1.ConditionUsageRolledUp and releases the finalizer. A crash
//     anywhere in between replays to the same result: the ledgers refuse a
//     second application.
//   - The patchy.usage.tokens and patchy.usage.cost metrics are emitted
//     with the user attribute when the user scope first takes the run.
//
// Rollups are read and written uncached (the API reader), so no controller
// holds an informer, or list/watch, on UsageRollups. Compaction is the
// rollup loop's (internal/controller/rollup).
package usagerollup
