// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package httpstep is the grammar of one HTTP step and the tool facts the
// agent evaluator shares with slice-1 acceptance verification. It moved out
// of internal/acceptance unchanged, so the saved checks (internal/checks),
// the tools relay (internal/previewtools), the ledger reader
// (internal/toolledger) and the controllers can build on it without the
// criteria runtime.
//
// A Step (step.go) is one request and what its response must show: a
// Request (method, path, headers, a JSON or raw body), an Expect (statuses,
// JSON pointers to literals or matchers, headers, bodyContains) and the
// captures later steps use. NormalizeSteps makes every default explicit
// (method GET, status [200]). Value (value.go) is a JSON value held in
// canonical form; StatusList decodes from an integer or a list.
//
// ValidateStep and its parts (validate.go) are the static grammar of a step:
// request paths that can never leave their host (CheckPath, path.go),
// headers that cannot redirect, smuggle or carry a URL, the matcher grammar
// of expected values ({$present}, {$absent}, {$contains}, {$approx,
// $tolerance}, {$order[, $by]}: ValidateMatcher) and the placeholders.
//
// Substitution (substitute.go) knows exactly two placeholders: {{run}}, a
// token maul draws per run, and {{capture.<name>}}, a value an earlier step
// captured. BuildRequest is the one function that turns a step into the
// request sent: captures are escaped into their context and the result is
// re-checked against the same grammar.
//
// The matcher engine (match.go) judges an observed value against an
// expected one (MatchValue, MatchHeaderValue) and reduces an observed value
// to a bounded one that judges the same (Reduce).
//
// The tool facts are the environment evidence and run causes both the
// slice-1 tester and the agent evaluator report: Transport classes
// (transport.go), PartFact and its roles (facts.go), ClassifyEnvironment
// (environment.go), the readiness wait (TargetWait, readiness.go) and
// RunCause (cause.go).
//
// The package imports only the standard library (a test pins that).
package httpstep
