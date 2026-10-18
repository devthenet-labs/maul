// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  DEFAULT_MEASURE,
  MAX_SERIES,
  NO_PROJECT,
  OTHER,
  TOKEN_TYPES,
  TOKEN_TYPE_LABELS,
  displayKeys,
  latestOnly,
  layoutBars,
  niceMax,
  personLabel,
  stackDay,
  stackFor,
  stackKeys,
  stackOptions,
  tokenTypeValue,
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

test("the chart opens on cost, and tokens open split by type", () => {
  assert.equal(DEFAULT_MEASURE, "cost");
  assert.deepEqual(stackOptions("cost"), ["project", "kind"]);
  assert.deepEqual(stackOptions("notional"), ["project", "kind"]);
  assert.deepEqual(stackOptions("tokens"), ["type", "project", "kind"]);
  // Switching to tokens splits by type; leaving tokens drops the type split.
  assert.equal(stackFor("project", "tokens", "cost"), "type");
  assert.equal(stackFor("kind", "tokens", "tokens"), "kind");
  assert.equal(stackFor("type", "cost", "tokens"), "project");
  assert.equal(stackFor("kind", "notional", "tokens"), "kind");
  assert.equal(stackFor("kind", "cost", "notional"), "kind");
});

test("stackDay by type splits a day's tokens and nothing else", () => {
  // The case the owner saw: 1.26M tokens, 91% of them cache reads.
  const d = {
    ...day([]),
    tokens: 1_260_000,
    inputTokens: 60_000,
    outputTokens: 30_000,
    cacheReadTokens: 1_150_000,
    cacheWriteTokens: 20_000,
  };
  const shown = stackKeys("type", ["a"], ["build"]);
  assert.deepEqual(shown, TOKEN_TYPES);
  const parts = stackDay(d, "type", "tokens", shown);
  assert.deepEqual(parts, [
    { key: "input", value: 60_000 },
    { key: "output", value: 30_000 },
    { key: "cacheRead", value: 1_150_000 },
    { key: "cacheWrite", value: 20_000 },
  ]);
  assert.equal(
    parts.reduce((a, p) => a + p.value, 0),
    d.tokens,
  );
  assert.deepEqual(stackDay(d, "type", "cost", shown), [], "no cost is recorded per type");
  assert.deepEqual(stackDay({ ...d, outputTokens: 0 }, "type", "tokens", shown).map((p) => p.key), ["input", "cacheRead", "cacheWrite"]);
  for (const t of TOKEN_TYPES) assert.equal(typeof TOKEN_TYPE_LABELS[t], "string");
  assert.equal(tokenTypeValue(d, "cacheRead"), 1_150_000);
  assert.deepEqual(stackKeys("project", ["a", NO_PROJECT], []), ["a", NO_PROJECT]);
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
    assert.equal(d.inputTokens + d.outputTokens + d.cacheReadTokens + d.cacheWriteTokens, d.tokens, d.day);
  }
  for (const p of r.projects) {
    assert.equal(p.inputTokens + p.outputTokens + p.cacheReadTokens + p.cacheWriteTokens, p.tokens, p.project);
  }
  assert.ok(mockUsage(90, true).people.length > 0);
});

// A deferred promise the test resolves by hand, so responses can arrive in
// any order.
function deferred() {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
}

// load mirrors UsageView's: begin, await, apply only while current.
async function load(latest, fetch, apply) {
  const current = latest.begin();
  const value = await fetch;
  if (current()) apply(value);
}

test("latestOnly keeps the newest load when an older one answers last", async () => {
  const latest = latestOnly();
  let shown = null;
  const days30 = deferred();
  const days90 = deferred();
  const first = load(latest, days30.promise, (v) => (shown = v));
  const second = load(latest, days90.promise, (v) => (shown = v));
  days90.resolve("90 days");
  await second;
  days30.resolve("30 days");
  await first;
  assert.equal(shown, "90 days");
});

test("latestOnly applies an in-order answer and nothing after cancel", async () => {
  const latest = latestOnly();
  let shown = null;
  await load(latest, Promise.resolve("mine"), (v) => (shown = v));
  assert.equal(shown, "mine");
  const late = deferred();
  const pending = load(latest, late.promise, (v) => (shown = v));
  latest.cancel();
  late.resolve("unmounted");
  await pending;
  assert.equal(shown, "mine");
});
