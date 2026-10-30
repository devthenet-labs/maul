// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package playwright compiles a saved browser check (internal/checks) to a
// Playwright test a human can read and run (DESIGN 2.7, SPEC S1-04 and
// spike SP-5). maul runs only what it compiled; the agent never writes
// JavaScript that runs in the tool pod.
//
// Compile emits one .spec.ts per check. It imports test and expect from
// maul's fixture (FixtureModule, the runner's keyed CDP attach) and maul's
// helpers from the support module (SupportModule, whose source is
// Support()); the runner writes both beside the spec. The spec runs at a
// fixed, recorded viewport (DefaultViewport, 1280x800: the overlap class of
// defect exists at desktop width only) with a 5 s action timeout, waits for
// the page to load after every goto, click and key press, substitutes
// {{run}} at compile time, and puts agent text only in JSON string
// literals (a seeded property and a fuzz target pin that).
//
// visible, legible and clickable run maul's page functions (pagefns.js,
// embedded, run through locator.evaluate so nothing is installed in the
// page). visible is human visible: a non-zero box with its centre in the
// viewport after scrolling, effective opacity above 0.1 and, for its own
// non-emoji text, contrast of at least 1.5:1. legible is WCAG AA, 4.5:1
// (3:1 for large text). The background is what is painted under the
// element (the hit-test stack at its centre, pointer-events lifted), a
// gradient judged per colour stop by unanimity, a background image
// indeterminate, and a paint effect (text-shadow, blend, filter,
// background-clip text) never a fail. clickable is a hit test at the box
// centre, a fail final and a pass confirmed by a trial click. An
// indeterminate judgement adds the AnnotationIndeterminate annotation and
// throws: a check error (unsure), never a fail. Box relations (left-of,
// above, no-overlap, equal-size, aligned, size) compare Playwright
// bounding boxes within the tolerance, retried up to the action timeout.
//
// The package imports only the standard library, internal/checks and
// internal/acceptance (a test pins that).
package playwright
