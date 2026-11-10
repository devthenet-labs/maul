// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package toolledger is the pure reading of a tool pod's ledger (DESIGN
// 2.2 step 10; SPEC S1 Q7, Q8; SPEC S2-02), shared by every host of the
// agent evaluator's tool pod: preview-controller's slot today, the agents
// namespace's next. It holds the tools container's log to the summary its
// termination message carries (Parse), and derives from the ledger alone
// what a verification's status may show: an index of every entry (Index),
// the replay and confirmation runs (ReplayRuns, ConfirmationRuns), the
// confirmed checks' bodies, read from the ledger's own dry-run records and
// never from the evaluator's report (ConfirmedChecks), and the relay's
// readiness wait (Readiness). ApplyStatus is the one write of those onto a
// verification's status both hosts make, and RoundReason why a ledger that
// holds still does not show its round (no evaluator called, a replay cut
// short). ConfigMapData shapes the full ledger under a
// ConfigMap's bound, its oldest outputs dropped first; the host names and
// owns the ConfigMap. A log that does not hold is maul's trouble (Infra
// ledger-mismatch), never a verdict.
//
// It is the ledger's trust root and stays pure: the standard library, the
// tool wire, the CRD types and the acceptance and check grammars only (a
// test pins that). The hosts keep the Kubernetes objects.
package toolledger
