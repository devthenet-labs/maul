// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package acceptance is the pure core of acceptance verification: what an
// approved intent plan declares under its `acceptance` key, and everything
// maul decides about it without touching a cluster or GitHub.
//
// A plan states outcomes (outcome.go): plain statements of what must hold,
// each `change` or `keep`, some `manual`, which the agent evaluator judges.
// NotCovered (coverage.go) lists the issue lines no outcome answers, for
// the approver: advice in the plan comment, never a refusal or a verdict.
// OutcomesDigest binds the full list.
//
// Judge (judge.go) is the only place a round's outcomes are judged ("evaluator
// observed"): an outcome passes or fails on the evaluator's verdict only
// when that verdict cites an entry ValidCite admits from the round's
// verified ledger, and OutcomeFingerprint (fingerprint.go) names a round's
// failure for RepeatedFailure detection, from outcome ids and digests only,
// never a value the evaluator or an application chose;
// httpstep.ClassifyEnvironment sorts an environment failure into the
// application's or maul's.
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
// internal/httpstep.
//
// The package imports only the standard library and internal/httpstep (a
// test pins that), so the report parser and the controllers share it.
package acceptance
