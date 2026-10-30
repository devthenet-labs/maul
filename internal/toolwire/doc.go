// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package toolwire is the agent evaluator's tool wire and ledger (DESIGN
// 2.1, 2.7, 4.2; SPEC S1-03): the contract between maul-tool (the
// evaluator's CLI), agent-runner, the tools container of a tool pod and the
// controllers that read its log.
//
// wire.go is the HTTP side: one endpoint per Verb (Path, POST /v1/<verb>),
// a typed request per endpoint held to its grammar and bounds by Check and
// read strictly by DecodeRequest, and one Response for all, which keeps
// the bytes the application chose (Untrusted) apart from maul's own
// measurements (Observed).
//
// ledger.go is the ledger v2, replacing slice 1's MAUL-VERIFY wire. The
// tools container records every call, and each of its own actions, as one
// LedgerEntry line on its stdout (EntryPrefix): the request as received,
// the full output and its sha256, what the agent saw inline, artifact
// digests, the locators generated for refs acted on, and a check run's
// attempts. At finish it emits the cited images as evidence lines
// (EvidencePrefix), then the Summary. Writer is the only producer; it
// numbers, bounds and checks each entry.
//
// summary.go is the reading side: the Summary (count and sha256 of the
// entry lines in order) is both the last log line and the container's
// termination message, and Verify holds the log to it. There is no hash
// chain: only the tools container writes that stdout (DESIGN 3).
//
// The package imports only the standard library (a test pins that).
package toolwire
