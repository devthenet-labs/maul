// Pure helpers of the Usage page: stack keys, segment values and the bar
// geometry of the daily chart. No DOM, so tests/usage.test.mjs runs them.

import type { UsageDay, UsageFigures, UsageSegment } from "./types";

// Measure is what the chart's bars measure.
export type UsageMeasure = "tokens" | "cost" | "notional";

// StackBy is what a day's bar is split by.
export type StackBy = "project" | "kind";

// NO_PROJECT mirrors usageview.NoProject: usage of no Project (Finding and
// evaluation runs).
export const NO_PROJECT = "(no project)";

// OTHER folds the stack keys past the palette's slots.
export const OTHER = "other";

// MAX_SERIES is how many keys get their own hue; the rest fold into OTHER,
// never into a generated hue.
export const MAX_SERIES = 6;

export const MEASURE_LABELS: Record<UsageMeasure, string> = {
  tokens: "tokens",
  cost: "cost",
  notional: "equivalent API cost",
};

export function segmentValue(seg: UsageSegment | UsageFigures, measure: UsageMeasure): number {
  if (measure === "cost") return seg.costMicroUSD ?? 0;
  if (measure === "notional") return seg.notionalCostMicroUSD ?? 0;
  return seg.tokens ?? 0;
}

// displayKeys keeps the report's order (largest first), at most MAX_SERIES
// keys, then OTHER for the rest; NO_PROJECT (a neutral, not a series) is
// kept, last.
export function displayKeys(keys: string[]): string[] {
  const named = keys.filter((k) => k !== NO_PROJECT);
  const out = named.length > MAX_SERIES ? [...named.slice(0, MAX_SERIES), OTHER] : named;
  return keys.includes(NO_PROJECT) ? [...out, NO_PROJECT] : out;
}

// foldKey maps a segment key onto its display key.
export function foldKey(key: string, shown: string[]): string {
  return shown.includes(key) ? key : OTHER;
}

export interface StackPart {
  key: string;
  value: number;
}

// stackDay is one day's bar: a part per display key with a value, in
// display order, folded keys summed into OTHER.
export function stackDay(day: UsageDay, by: StackBy, measure: UsageMeasure, shown: string[]): StackPart[] {
  const segs = by === "project" ? day.projects : day.kinds;
  const sums = new Map<string, number>();
  for (const s of segs ?? []) {
    const k = foldKey(s.key, shown);
    sums.set(k, (sums.get(k) ?? 0) + segmentValue(s, measure));
  }
  return shown.filter((k) => (sums.get(k) ?? 0) > 0).map((k) => ({ key: k, value: sums.get(k) ?? 0 }));
}

// niceMax rounds a positive maximum up to 1, 2 or 5 times a power of ten,
// for the one gridline the chart labels.
export function niceMax(v: number): number {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 5, 10]) {
    if (v <= m * p) return m * p;
  }
  return 10 * p;
}

export interface Rect {
  key: string;
  x: number;
  y: number;
  width: number;
  height: number;
  value: number;
}

// layoutBars places the stacked bars of days in a chart of the given size:
// one slot per day, parts stacked from the baseline up with a 2px surface
// gap between them, heights proportional to max.
export function layoutBars(
  days: UsageDay[],
  by: StackBy,
  measure: UsageMeasure,
  shown: string[],
  width: number,
  height: number,
  max: number,
): Rect[][] {
  const slot = days.length > 0 ? width / days.length : width;
  const barWidth = Math.max(1, Math.min(18, slot * 0.7));
  const gap = 2;
  return days.map((day, i) => {
    const x = i * slot + (slot - barWidth) / 2;
    let y = height;
    const rects: Rect[] = [];
    for (const part of stackDay(day, by, measure, shown)) {
      const h = Math.max(1, (part.value / max) * height - (rects.length > 0 ? gap : 0));
      y -= h + (rects.length > 0 ? gap : 0);
      rects.push({ key: part.key, x, y, width: barWidth, height: h, value: part.value });
    }
    return rects;
  });
}

// KIND_LABELS are the run kinds' readable names.
export const KIND_LABELS: Record<string, string> = {
  investigation: "investigation",
  remediation: "remediation",
  plan: "intent plan",
  build: "intent build",
  revise: "intent revise",
  "check-fix": "check fix",
  evaluation: "evaluation",
};

// personLabel names a user key for display: the login when known.
export function personLabel(user: string, login?: string): string {
  if (user === "org") return "org (no person)";
  return login ? `@${login}` : user;
}
