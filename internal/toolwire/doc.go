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
// digests and the locators generated for refs acted on. At finish it emits the cited images as evidence lines
// (EvidencePrefix), then the Summary. Writer is the only producer; it
// numbers, bounds and checks each entry.
//
// summary.go is the reading side: the Summary (count and sha256 of the
// entry lines in order) is both the last log line and the container's
// termination message, and Verify holds the log to it. There is no hash
// chain: only the tools container writes that stdout (DESIGN 3).
//
// SPEC S2-05 adds the verbs files (a look at a pristine source tree, with
// caps) and run (a Go program the agent wrote, compiled against a tree), the
// relay-internal VerbBuild (no endpoint), Side (head, the default, or base) on
// exec, run and files requests and on entries, and KeyHeader, the
// per-evaluation key. An entry with no Side encodes as in slice 1.
//
// workspace.go holds the spellings the prompts and the relay share: the
// placeholders an agents round's exec and run calls may name
// (BinPlaceholder, TreePlaceholder) and the fixed DefaultViewport every
// browser session opens at, which the plan prompt states.
//
// The check runner's verbs (check-run, check-results, confirm) are retired
// spellings (RetiredVerb): an earlier release's ledger may name them, but no
// endpoint answers them, and its check run record is no longer part of an
// entry, so a line that carries one does not verify.
//
// The package imports only the standard library (a test pins that).
package toolwire
