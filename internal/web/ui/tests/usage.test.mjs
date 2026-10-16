// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  MAX_SERIES,
  NO_PROJECT,
  OTHER,
  displayKeys,
  layoutBars,
  niceMax,
  personLabel,
  stackDay,
} from "../src/usage.ts";
import { hrefForUsage, parseRoute } from "../src/router.ts";
import { mockUsage } from "../src/mock/usage.ts";

const seg = (key, tokens, cost = 0, notional = 0) => ({
  key,
  runs: 1,
  tokens,
  costMicroUSD: cost,
  notionalCostMicroUSD: notional,
});

const day = (projects, kinds = []) => ({
  day: "2026-10-16",
  runs: 0,
  partialRuns: 0,
  reportedRuns: 0,
  unpricedRuns: 0,
  tokens: projects.reduce((a, s) => a + s.tokens, 0),
  inputTokens: 0,
  outputTokens: 0,
  cacheReadTokens: 0,
  cacheWriteTokens: 0,
  unmeteredTokens: 0,
  costMicroUSD: 0,
  notionalCostMicroUSD: 0,
  reportedTokens: 0,
  reportedCostMicroUSD: 0,
  projects,
  kinds,
});

test("usage routes", () => {
  assert.deepEqual(parseRoute("#/usage"), { view: "usage", all: false });
  assert.deepEqual(parseRoute("#/usage/all"), { view: "usage", all: true });
  assert.equal(hrefForUsage(), "#/usage");
  assert.equal(hrefForUsage(true), "#/usage/all");
});

test("displayKeys folds past the palette into other and keeps no-project last", () => {
  const keys = ["a", "b", "c", "d", "e", "f", "g", "h", NO_PROJECT];
  const shown = displayKeys(keys);
  assert.deepEqual(shown, ["a", "b", "c", "d", "e", "f", OTHER, NO_PROJECT]);
  assert.equal(shown.filter((k) => k !== OTHER && k !== NO_PROJECT).length, MAX_SERIES);
  assert.deepEqual(displayKeys(["a", NO_PROJECT]), ["a", NO_PROJECT]);
  assert.deepEqual(displayKeys([]), []);
});

test("stackDay sums folded keys and measures what it is asked", () => {
  const shown = displayKeys(["a", "b", "c", "d", "e", "f", "g", "h"]);
  const d = day([seg("a", 10, 5), seg("g", 3, 1), seg("h", 4, 2), seg("b", 0, 0)]);
  assert.deepEqual(stackDay(d, "project", "tokens", shown), [
    { key: "a", value: 10 },
    { key: OTHER, value: 7 },
  ]);
  assert.deepEqual(stackDay(d, "project", "cost", shown), [
    { key: "a", value: 5 },
    { key: OTHER, value: 3 },
  ]);
  assert.deepEqual(stackDay(d, "kind", "tokens", shown), []);
});

test("niceMax rounds up to 1, 2 or 5 times a power of ten", () => {
  assert.equal(niceMax(0), 1);
  assert.equal(niceMax(7), 10);
  assert.equal(niceMax(140), 200);
  assert.equal(niceMax(2_000_000), 2_000_000);
  assert.equal(niceMax(2_000_001), 5_000_000);
});

test("layoutBars stacks from the baseline with gaps and stays inside the chart", () => {
  const shown = ["a", "b"];
  const days = [day([seg("a", 50), seg("b", 50)]), day([]), day([seg("b", 25)])];
  const bars = layoutBars(days, "project", "tokens", shown, 300, 100, 100);
  assert.equal(bars.length, 3);
  assert.equal(bars[1].length, 0);
  const [a, b] = bars[0];
  assert.equal(a.y + a.height, 100, "the first part sits on the baseline");
  assert.equal(b.y + b.height + 2, a.y, "a 2px gap separates stacked parts");
  for (const rects of bars) {
    for (const r of rects) {
      assert.ok(r.y >= 0 && r.y + r.height <= 100, `rect ${JSON.stringify(r)} leaves the chart`);
      assert.ok(r.x >= 0 && r.x + r.width <= 300);
    }
  }
});

test("personLabel prefers the login and names the org", () => {
  assert.equal(personLabel("github-id:42", "octocat"), "@octocat");
  assert.equal(personLabel("github-id:42", ""), "github-id:42");
  assert.equal(personLabel("org"), "org (no person)");
});

test("the demo report covers its window and its kinds sum to each day", () => {
  const r = mockUsage(30, false);
  assert.equal(r.daily.length, 30);
  assert.equal(r.view, "self");
  for (const d of r.daily) {
    assert.equal(
      d.kinds.reduce((a, s) => a + s.tokens, 0),
      d.tokens,
      d.day,
    );
    assert.equal(
      d.projects.reduce((a, s) => a + s.tokens, 0),
      d.tokens,
      d.day,
    );
  }
  assert.ok(mockUsage(90, true).people.length > 0);
});
