// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package acceptance is the pure core of acceptance verification: the
// criteria an approved intent plan declares under its `acceptance` key, and
// everything maul decides about them without touching a cluster or GitHub.
//
// A Criterion (criterion.go) is one checkable statement of the plan: kind
// http (requests to a previewed component, by its Service name, in one or
// more steps that may capture values for later ones), command (slice 2: an
// argv run in a component's image or the repository's agent image) or
// manual (closed by a human's `/maul accept`). browser and agent are
// reserved and refused until their slices ship. A bare request/expect pair
// is shorthand for one step; Normalize folds it into Steps and makes every
// default explicit.
//
// Validate (validate.go) is the static grammar and the only source of a
// CriterionError: identifiers, kinds, bounds (MaxCriteria criteria,
// MaxSteps steps in all), request paths that can never leave their host
// (CheckPath, path.go), headers that cannot redirect, smuggle or carry a
// URL, the matcher grammar of expected values and the placeholders.
// Whether a target names a previewed repository of the Project is the
// controller's check, not this package's.
//
// Substitution (substitute.go) knows exactly two placeholders: {{run}}, a
// maul-generated token unique per try (RunTokens.Token), so checks create
// uniquely named data in a Preview that is never reset; and
// {{capture.<name>}}, a value an earlier step captured from a response.
// BuildRequest is the one function that turns a step into the request the
// tester sends: captures are escaped into their context, so a value an
// application returned can never add a path segment, a query parameter or a
// header line, and the result is re-checked against the same grammar.
//
// Digest (digest.go) binds the FULL list, manual criteria included, in plan
// order, over its normalised canonical JSON: intent-controller stores it
// beside the approved criteria and preview-controller re-checks it before
// rendering the tester (critique fix 4). Automatable picks the criteria a
// slice runs, in plan order.
//
// The tester observes; maul judges. Observe (observation.go) is the
// tester's loop with the I/O injected: it builds each step's request, hands
// it to a Doer, records a bounded StepObservation (the raw JSON at each
// expected pointer, reduced so it is judged the same as the full value, the
// captures, the named headers, a display-only body prefix), and re-runs a
// failing criterion once with a fresh {{run}}. JudgeStep decides only that
// retry. Judge (judge.go), run by intent-controller on what
// preview-controller read back, is the verdict: pass, quarantined (failed,
// then passed on the retry: shown to the human, never counted as passed,
// D7) or fail. There is no runtime error outcome: a transport failure, a
// body that is not JSON, a missing pointer or capture, an observation that
// does not fit its criterion, all fail.
//
// The observations travel on the wire format v1 (observation.go): one
// `MAUL-VERIFY: ` JSON line per criterion and a final summary carrying the
// count and sha256 of the lines before it, repeated in the tester's
// termination message so a truncated log is detected (ParseWire). The
// summary also carries the tester's readiness wait (readiness.go): one
// TargetWait per target, made of connection outcomes before any request,
// and the targets never reached, after which no criterion is observed and
// preview-controller marks the component unreachable: the application's
// failure (PartFact.UnreachableForMs).
//
// Fingerprint (fingerprint.go) names a failure for RepeatedFailure
// detection: failing criteria's ids, outcomes, statuses, transport classes
// and the matched/unmatched bit of each expectation, plus the failing
// parts' waiting or termination reasons and exit codes (fix 10). Never an
// actual value, body or log text, so it is stable across runs and immune to
// what an application chooses to print. ClassifyEnvironment
// (environment.go) sorts an environment failure into one the application
// caused (charged to the intent, fix 13) or infrastructure trouble (free
// retries).
//
// The package imports only the standard library (a test pins that), so the
// report parser, both controllers and the tester binary share it.
// v1alpha1.AcceptanceCriterion mirrors Criterion field for field, with the
// same JSON tags, for the CRDs that store criteria.
package acceptance
