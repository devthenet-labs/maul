// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package acceptance is the pure core of acceptance verification: what an
// approved intent plan declares under its `acceptance` key, and everything
// maul decides about it without touching a cluster or GitHub.
//
// A plan states outcomes (outcome.go): plain statements of what must hold,
// each `change` or `keep`, some `manual`, which the agent evaluator judges.
// NotCovered (coverage.go) lists the issue lines no outcome answers, and
// LintOutcomes (lint.go) flags outcomes no check can show on a preview whose
// data is shared, for the approver: both are advice in the plan comment,
// never a refusal or a verdict. OutcomesDigest binds the full list.
//
// Decide (decide.go) is the only place a round's outcomes are judged: the
// tools ledger's runs, the check reviewer's answers and the evaluator's
// claims go in, a result per outcome, at most one fix request and the
// hand-off come out (DESIGN 4.3's pass rules; base_comparison.go is rule
// 5's reading of an agents round's base runs). EvaluationFingerprint
// (fingerprint.go) names a round's failure for RepeatedFailure detection,
// from ids, digests and the environment's failing parts only, never a value
// an application chose to print; httpstep.ClassifyEnvironment sorts an
// environment failure into the application's or maul's.
//
// Slice 1's criteria grammar (criterion.go, validate.go, digest.go) is kept
// only to read a plan that states criteria: nothing runs or judges a
// criterion any more. Under the evaluator such a plan is refused for a
// replan (report.ErrSliceOneCriteria); elsewhere it is recorded by its
// criteria Digest alone, which a record of slice 1's era also keeps once the
// API server has pruned its criteria. A bare request/expect pair is
// shorthand for one step; Normalize folds it into Steps, and Digest binds
// the normalised list. The step grammar, substitution and matchers, and the
// environment facts (PartFact, ClassifyEnvironment, TargetWait), live in
// internal/httpstep, which the evaluator's checks share.
//
// The package imports only the standard library and internal/httpstep (a
// test pins that), so the report parser and the controllers share it.
package acceptance
