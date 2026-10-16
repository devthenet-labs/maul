// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package usagemeter is the run controllers' side of trusted usage (per-user
// credentials design, revision 1, L2): when a run settles, its controller
// reads what the egress broker metered for the run's Job and records it,
// priced, as the run's status usage measured figure, with a source and the
// final mark.
//
// The four controllers that launch agent Jobs (investigation, remediation,
// intent and evaluation) share one Meter, built from the
// --usage-measurement flags (Flags); off, the default, it is nil and no run
// is ever written. On, a terminal run whose usage is not yet final is
// settled by Meter.Settle:
//
//   - a run with no Job, or one on a harness the broker does not carry
//     (codex, copilot, the fake harness), is source reported at once: only
//     the pod-reported counts exist, and they stay out of trusted totals;
//   - otherwise the broker is asked for the Job's pods (Client, a projected
//     token for usagewire.Audience read fresh per call). While a pod still
//     has requests in flight, or the broker cannot be reached, the run is
//     looked at again, for at most Wait (two minutes) from the first look;
//   - the answer is measured only when the broker booted before the Job was
//     created, so its ledger saw the whole pod; partial when it booted
//     later or requests were still in flight when the wait ended; reported
//     when it has no entry for the Job or never answered.
//
// Pricing (Price) normalises each reported model id to the registry
// (model.Resolve), prices only observed tokens at the versioned list rates
// (model.MeasuredCostUSD, cache writes at their multipliers), keeps an
// unknown model's tokens with no cost, never prices the estimated
// shortfall, and marks a subscription credential's cost notional.
//
// The broker is a single replica, and the figure depends on it.
package usagemeter
