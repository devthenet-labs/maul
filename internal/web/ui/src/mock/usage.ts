// Demo data of the Usage page: deterministic, so the committed kit build
// renders the same every time.

import type { UsageAccess, UsageDay, UsageReport } from "../types";

export function mockUsageAccess(): UsageAccess {
  return { name: "demo", self: true, all: true, reason: "" };
}

const PROJECTS = ["storefront", "billing"];
const KINDS = ["plan", "build", "revise", "investigation", "remediation"];

function day(i: number, days: number, to: Date): UsageDay {
  const d = new Date(to.getTime() - (days - 1 - i) * 86_400_000);
  const seed = (i * 7919) % 13;
  const store = seed > 3 ? seed * 210_000 : 0;
  const bill = seed % 4 === 1 ? seed * 90_000 : 0;
  const none = seed % 3 === 0 ? 120_000 : 0;
  const tokens = store + bill + none;
  const cost = Math.round(tokens * 3.1);
  const projects = [
    { key: PROJECTS[0], runs: store ? 3 : 0, tokens: store, costMicroUSD: Math.round(store * 3.1), notionalCostMicroUSD: 0 },
    { key: PROJECTS[1], runs: bill ? 1 : 0, tokens: bill, costMicroUSD: Math.round(bill * 3.1), notionalCostMicroUSD: 0 },
    { key: "(no project)", runs: none ? 1 : 0, tokens: none, costMicroUSD: Math.round(none * 3.1), notionalCostMicroUSD: 0 },
  ].filter((s) => s.tokens > 0);
  const kinds = KINDS.map((k, j) => {
    const share = j === 1 ? 0.6 : j === 0 ? 0.15 : 0.0625;
    const t = Math.round(tokens * share);
    return { key: k, runs: t ? 1 : 0, tokens: t, costMicroUSD: Math.round(t * 3.1), notionalCostMicroUSD: 0 };
  }).filter((s) => s.tokens > 0);
  // Keep the kind split summing to the day.
  if (kinds.length > 0) {
    const sum = kinds.reduce((a, s) => a + s.tokens, 0);
    kinds[0].tokens += tokens - sum;
    kinds[0].costMicroUSD += cost - kinds.reduce((a, s) => a + s.costMicroUSD, 0);
  }
  const runs = projects.reduce((a, s) => a + s.runs, 0);
  return {
    day: d.toISOString().slice(0, 10),
    runs,
    partialRuns: 0,
    reportedRuns: 0,
    unpricedRuns: 0,
    tokens,
    inputTokens: Math.round(tokens * 0.2),
    outputTokens: Math.round(tokens * 0.05),
    cacheReadTokens: tokens - Math.round(tokens * 0.2) - Math.round(tokens * 0.05) - Math.round(tokens * 0.03),
    cacheWriteTokens: Math.round(tokens * 0.03),
    unmeteredTokens: seed === 7 ? 1800 : 0,
    costMicroUSD: cost,
    notionalCostMicroUSD: 0,
    reportedTokens: 0,
    reportedCostMicroUSD: 0,
    projects,
    kinds,
  };
}

// projectTotals sums one Project's segments over the window.
function projectTotals(daily: UsageDay[], project: string) {
  const t = {
    project, runs: 0, partialRuns: 0, reportedRuns: 0, unpricedRuns: 0, tokens: 0, inputTokens: 0, outputTokens: 0,
    cacheReadTokens: 0, cacheWriteTokens: 0, unmeteredTokens: 0, costMicroUSD: 0, notionalCostMicroUSD: 0,
    reportedTokens: 0, reportedCostMicroUSD: 0,
  };
  for (const d of daily) {
    for (const s of d.projects) {
      if (s.key !== project) continue;
      t.runs += s.runs;
      t.tokens += s.tokens;
      t.costMicroUSD += s.costMicroUSD;
    }
  }
  // The same split by type as the days', summing to the total.
  t.inputTokens = Math.round(t.tokens * 0.2);
  t.outputTokens = Math.round(t.tokens * 0.05);
  t.cacheWriteTokens = Math.round(t.tokens * 0.03);
  t.cacheReadTokens = t.tokens - t.inputTokens - t.outputTokens - t.cacheWriteTokens;
  return t;
}

export function mockUsage(days: number, all: boolean): UsageReport {
  const to = new Date();
  const daily = Array.from({ length: days }, (_, i) => day(i, days, to));
  const totals = daily.reduce(
    (t, d) => ({
      runs: t.runs + d.runs,
      partialRuns: 0,
      reportedRuns: 1,
      unpricedRuns: 0,
      tokens: t.tokens + d.tokens,
      inputTokens: t.inputTokens + d.inputTokens,
      outputTokens: t.outputTokens + d.outputTokens,
      cacheReadTokens: t.cacheReadTokens + d.cacheReadTokens,
      cacheWriteTokens: t.cacheWriteTokens + d.cacheWriteTokens,
      unmeteredTokens: t.unmeteredTokens + d.unmeteredTokens,
      costMicroUSD: t.costMicroUSD + d.costMicroUSD,
      notionalCostMicroUSD: 0,
      reportedTokens: 48_000,
      reportedCostMicroUSD: 210_000,
    }),
    {
      runs: 1, partialRuns: 0, reportedRuns: 1, unpricedRuns: 0, tokens: 0, inputTokens: 0, outputTokens: 0,
      cacheReadTokens: 0, cacheWriteTokens: 0, unmeteredTokens: 0, costMicroUSD: 0, notionalCostMicroUSD: 0,
      reportedTokens: 48_000, reportedCostMicroUSD: 210_000,
    },
  );
  const report: UsageReport = {
    view: all ? "all" : "self",
    user: all ? "" : "github-id:583231",
    login: all ? "" : "demo",
    project: "",
    days,
    from: daily[0].day,
    to: daily[days - 1].day,
    totals,
    daily,
    projectKeys: [...PROJECTS, "(no project)"],
    kindKeys: KINDS,
    topIntents: [
      { intent: "storefront-41", project: "storefront", runs: 9, tokens: 4_100_000, costMicroUSD: 12_700_000, notionalCostMicroUSD: 0 },
      { intent: "billing-12", project: "billing", runs: 4, tokens: 1_300_000, costMicroUSD: 4_000_000, notionalCostMicroUSD: 0 },
    ],
    topIntentMonths: [daily[0].day.slice(0, 7), daily[days - 1].day.slice(0, 7)].filter((m, i, a) => a.indexOf(m) === i),
    projects: PROJECTS.map((p) => projectTotals(daily, p)),
  };
  if (all) {
    report.people = [
      { ...totals, user: "github-id:583231", login: "demo" },
      { ...totals, user: "org", login: "" },
    ];
  }
  return report;
}
