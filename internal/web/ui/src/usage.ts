// Pure helpers of the Usage page: stack keys, segment values and the bar
// geometry of the daily chart. No DOM, so tests/usage.test.mjs runs them.

import type { UsageDay, UsageFigures, UsageSegment } from "./types";

// Measure is what the chart's bars measure.
export type UsageMeasure = "tokens" | "cost" | "notional";

// DEFAULT_MEASURE is what the chart opens on: cost. A token total is mostly
// cache reads, which cost a tenth of input, so it says little about spend.
export const DEFAULT_MEASURE: UsageMeasure = "cost";

// StackBy is what a day's bar is split by. "type" (input, output, cache
// read, cache write) applies to tokens only: no cost is recorded per type.
export type StackBy = "project" | "kind" | "type";

// TokenType is a metered token class. The rollups record cache writes
// (five-minute and one-hour) as one figure, so the page shows one.
export type TokenType = "input" | "output" | "cacheRead" | "cacheWrite";

export const TOKEN_TYPES: TokenType[] = ["input", "output", "cacheRead", "cacheWrite"];

export const TOKEN_TYPE_LABELS: Record<TokenType, string> = {
  input: "input",
  output: "output",
  cacheRead: "cache read",
  cacheWrite: "cache write",
};

// tokenTypeValue is one class's tokens of a figure.
export function tokenTypeValue(f: UsageFigures, t: TokenType): number {
  if (t === "input") return f.inputTokens ?? 0;
  if (t === "output") return f.outputTokens ?? 0;
  if (t === "cacheRead") return f.cacheReadTokens ?? 0;
  return f.cacheWriteTokens ?? 0;
}

// stackOptions are the splits a measure offers: by token type only for
// tokens, since the rollups record no cost per type.
export function stackOptions(measure: UsageMeasure): StackBy[] {
  return measure === "tokens" ? ["type", "project", "kind"] : ["project", "kind"];
}

// stackFor is the split after the measure changes from previous: tokens
// open split by type (a token total alone hides that most of it is cheap
// cache reads), and a split the new measure does not offer falls back to
// by project.
export function stackFor(by: StackBy, measure: UsageMeasure, previous: UsageMeasure): StackBy {
  if (measure === "tokens" && previous !== "tokens") return "type";
  return stackOptions(measure).includes(by) ? by : "project";
}

// stackKeys are the display keys of a split: the token types in their fixed
// order, or the report's Project or kind keys folded by displayKeys.
export function stackKeys(by: StackBy, projectKeys: string[], kindKeys: string[]): string[] {
  if (by === "type") return [...TOKEN_TYPES];
  return displayKeys(by === "project" ? projectKeys : kindKeys);
}

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
// display order, folded keys summed into OTHER. Split by type, the parts
// are the day's token classes (tokens only: nothing for a cost).
export function stackDay(day: UsageDay, by: StackBy, measure: UsageMeasure, shown: string[]): StackPart[] {
  if (by === "type") {
    if (measure !== "tokens") return [];
    return TOKEN_TYPES.filter((t) => shown.includes(t))
      .map((t): StackPart => ({ key: t, value: tokenTypeValue(day, t) }))
      .filter((p) => p.value > 0);
  }
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
  "verify-fix": "acceptance fix",
  evaluation: "evaluation",
};

// personLabel names a user key for display: the login when known.
export function personLabel(user: string, login?: string): string {
  if (user === "org") return "org (no person)";
  return login ? `@${login}` : user;
}

// Latest sequences overlapping loads: each begin() hands back a check that
// holds only until the next begin() or cancel(). A load applies its answer
// only while its check holds, so a slower, older response (the 30-day
// report after a switch to 90 days, or the unfiltered one after a filter
// is cleared) can never overwrite a newer one.
export interface Latest {
  begin(): () => boolean;
  cancel(): void;
}

export function latestOnly(): Latest {
  let seq = 0;
  return {
    begin() {
      const mine = ++seq;
      return () => mine === seq;
    },
    cancel() {
      seq++;
    },
  };
}
