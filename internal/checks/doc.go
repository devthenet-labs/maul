// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package checks is the saved-check grammar of the agent evaluator (DESIGN
// 2.7 and 4.3, SPEC S1-02): the only forms a check the evaluator saves may
// take, and everything maul decides about one statically, before it is
// ever run.
//
// A Check (check.go) is bound to one approved outcome and has exactly one
// of three forms, all of them data that maul runs, never code an agent
// wrote for a token-holding pod:
//
//   - http (http.go): slice 1's request steps (internal/acceptance's
//     Step, with {{run}}, captures, the path and header rules and the
//     $order/$approx/$contains/$present matchers, all by import), plus
//     the numeric range ({$range: [lo, hi]}), list-contains
//     ({$includes: v}, an object matching as a subset) and header
//     presence ({$present}/{$absent}) matchers of demo finding 11.
//     MatchJSON and MatchHeader judge an observed value.
//   - browser (browser.go): a JSON step grammar maul compiles to
//     Playwright: goto, waitForURL, click, fill, select (by label),
//     press, scroll; locators by role and name, label, text, test id or
//     CSS (last resort), scoped with within, hasText and nth, never a
//     snapshot ref; and the assertions visible and legible (human
//     visibility and WCAG contrast, Q9), text, count, url, the box
//     relations left-of, above, no-overlap, equal-size, aligned and size
//     (each within a tolerance in px), and clickable.
//   - exec (exec.go): an argv run in the slot's sandbox container with
//     its program files, asserting on the exit code, stdout text and
//     stdout JSON.
//
// Parse decodes a check strictly; Canonical is its normalised JSON
// (defaults explicit, object keys sorted, values canonical) and Digest the
// sha256 of that, so a check digests alike however it was spelled.
//
// Validate (validate.go) holds a check to the grammar and to the rules of
// DESIGN 4.3, returning a *Refusal naming the Rule it broke: a snapshot
// ref anywhere (RuleSnapshotRef), a colour literal in any assertion
// (RuleColourLiteral), a CSS locator of more than MaxCSSCompounds
// compounds (RuleCSSDepth), more than MaxSteps steps (RuleTooManySteps),
// a check that is not substantive (RuleNotSubstantive: one step acting on
// a surface and one assertion about content; a status or exit code alone
// never is), a list, row, count, $order or $includes assertion not
// filtered by {{run}} text (RuleRunToken), navigation off the preview's
// paths (RuleOffPreview), a check too large to store (RuleTooLarge) and
// everything else the grammar refuses (RuleGrammar).
//
// The package imports only the standard library and internal/acceptance
// (a test pins that).
package checks
