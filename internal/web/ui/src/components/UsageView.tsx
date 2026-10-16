// The Usage page (design L2, Dashboard): the signed-in person's own daily
// tokens and cost, or, for holders of usage/all, everyone's. It gates on its
// own answers (401, 403), never on the findings grant. The chart is inline
// SVG, no chart library: one stacked bar per day, split by Project or run
// kind, measuring tokens, cost or a subscription's equivalent API cost.
// Trusted figures are the broker's; the unmetered worst case and the pods'
// own reported figures are shown apart and never added to a cost.

import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useMemo, useState } from "preact/hooks";
import type { UsageAccess, UsageDay, UsageFigures, UsageReport } from "../types";
import { AuthRequiredError, ForbiddenError, fetchUsage } from "../api";
import { signInURL } from "../auth";
import { formatMicroUSD } from "../format";
import { hrefForUsage } from "../router";
import {
  KIND_LABELS,
  MEASURE_LABELS,
  NO_PROJECT,
  OTHER,
  displayKeys,
  layoutBars,
  niceMax,
  personLabel,
  segmentValue,
  stackDay,
  type StackBy,
  type UsageMeasure,
} from "../usage";

type Loaded =
  | { kind: "loading" }
  | { kind: "auth" }
  | { kind: "denied"; message: string }
  | { kind: "error"; message: string }
  | { kind: "report"; report: UsageReport };

const WINDOWS = [30, 90];

function Panel({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <div class="mx-auto my-20 max-w-[460px] rounded-xl border border-line-2 bg-surface p-7 text-center shadow-card">
      <h1 class="mx-0 mt-0 mb-2 text-[19px] tracking-tight">{title}</h1>
      <div class="text-muted">{children}</div>
    </div>
  );
}

function count(n: number): string {
  return n.toLocaleString();
}

function measureValue(v: number, measure: UsageMeasure): string {
  return measure === "tokens" ? count(v) : formatMicroUSD(v);
}

// colorOf gives each display key its fixed slot: series follow the key's
// position in the report's order, never a day's rank.
function colorOf(key: string, shown: string[]): string {
  if (key === NO_PROJECT) return "var(--usage-none)";
  if (key === OTHER) return "var(--usage-other)";
  const i = shown.indexOf(key);
  return `var(--usage-${(i % 6) + 1})`;
}

function keyLabel(key: string, by: StackBy): string {
  if (by === "kind") return KIND_LABELS[key] ?? key;
  return key === OTHER ? "other projects" : key;
}

function Toggle<T extends string | number>({
  label,
  options,
  value,
  onChange,
}: {
  label: string;
  options: [T, string][];
  value: T;
  onChange: (v: T) => void;
}) {
  return (
    <div class="ps-filter-group" role="group" aria-label={label}>
      {options.map(([v, l]) => (
        <button key={String(v)} type="button" class={value === v ? "is-active" : ""} aria-pressed={value === v} onClick={() => onChange(v)}>
          {l}
        </button>
      ))}
    </div>
  );
}

function Tiles({ t }: { t: UsageFigures }) {
  const tiles: [string, string, string?][] = [
    ["tokens (metered)", count(t.tokens)],
    ["cost (list rates)", formatMicroUSD(t.costMicroUSD)],
    ["equivalent API cost", formatMicroUSD(t.notionalCostMicroUSD), "subscription: not money spent"],
    ["runs", count(t.runs)],
  ];
  return (
    <section class="grid grid-cols-4 overflow-hidden rounded-xl border border-line-2 bg-surface max-lg:grid-cols-2" aria-label="Usage totals">
      {tiles.map(([label, value, note]) => (
        <div class="ps-stat" key={label} title={note}>
          <strong>{value}</strong>
          <span>{label}</span>
        </div>
      ))}
    </section>
  );
}

// Apart lists the figures that are not trusted totals, so they are seen
// and never mistaken for spend.
function Apart({ t }: { t: UsageFigures }) {
  return (
    <section class="mt-3.5 grid gap-3 md:grid-cols-2" aria-label="Figures shown apart">
      <div class="rounded-xl border border-line bg-surface px-4.5 py-3.5">
        <h2 class="ps-heading">Unmetered (worst case)</h2>
        <p class="mt-1.5 mb-0 font-mono text-[15px]">{count(t.unmeteredTokens)} tokens</p>
        <p class="mt-1 mb-0 text-[12px] text-muted">
          Tokens the broker could not see in a response it relayed, counted at their worst case. Never priced and
          never part of a cost.
        </p>
      </div>
      <div class="rounded-xl border border-line bg-surface px-4.5 py-3.5">
        <h2 class="ps-heading">Source</h2>
        <ul class="mt-1.5 mb-0 list-none p-0 text-[12.5px] text-muted">
          <li>
            <strong class="text-fg">{count(t.runs - t.reportedRuns)}</strong> runs metered by the egress broker
            {t.partialRuns > 0 ? `, ${count(t.partialRuns)} of them partly (the broker restarted mid-run)` : ""}
          </li>
          <li>
            <strong class="text-fg">{count(t.reportedRuns)}</strong> runs with only their pod's own report:{" "}
            {count(t.reportedTokens)} tokens, {formatMicroUSD(t.reportedCostMicroUSD)} (untrusted, not in the totals)
          </li>
          {t.unpricedRuns > 0 ? (
            <li>
              <strong class="text-fg">{count(t.unpricedRuns)}</strong> metered runs used a model with no known price:
              tokens counted, cost not
            </li>
          ) : null}
        </ul>
      </div>
    </section>
  );
}

const CHART_W = 720;
const CHART_H = 180;

function DailyChart({ report }: { report: UsageReport }) {
  const [measure, setMeasure] = useState<UsageMeasure>("tokens");
  const [by, setBy] = useState<StackBy>("project");
  const keys = by === "project" ? report.projectKeys : report.kindKeys;
  const shown = useMemo(() => displayKeys(keys), [keys]);
  const peak = Math.max(0, ...report.daily.map((d) => stackDay(d, by, measure, shown).reduce((a, p) => a + p.value, 0)));
  const max = niceMax(peak);
  const bars = layoutBars(report.daily, by, measure, shown, CHART_W, CHART_H, max);
  const [hover, setHover] = useState<number | null>(null);
  const hovered = hover !== null ? report.daily[hover] : null;

  return (
    <section class="mt-3.5 rounded-xl border border-line bg-surface px-4.5 py-4" aria-label="Daily usage">
      <header class="flex flex-wrap items-center justify-between gap-3.5">
        <h2 class="ps-heading">Daily {MEASURE_LABELS[measure]}</h2>
        <div class="flex flex-wrap gap-2">
          <Toggle label="Measure" value={measure} onChange={setMeasure} options={[["tokens", "tokens"], ["cost", "cost"], ["notional", "equivalent API cost"]]} />
          <Toggle label="Stack by" value={by} onChange={setBy} options={[["project", "by project"], ["kind", "by run kind"]]} />
        </div>
      </header>
      {measure === "notional" ? (
        <p class="mt-2 mb-0 text-[12px] text-muted">
          What a Claude subscription's runs would have cost at API list rates. Notional: not money spent.
        </p>
      ) : null}
      <ul class="mt-3 mb-0 flex list-none flex-wrap gap-x-4 gap-y-1 p-0 text-[12px] text-muted" aria-label="Legend">
        {shown.map((k) => (
          <li key={k} class="inline-flex items-center gap-1.5">
            <span class="inline-block size-2.5 rounded-[3px]" style={{ background: colorOf(k, shown) }} />
            {keyLabel(k, by)}
          </li>
        ))}
      </ul>
      <div class="relative mt-2">
        <svg
          viewBox={`0 -14 ${CHART_W} ${CHART_H + 30}`}
          class="block h-auto w-full"
          role="img"
          aria-label={`${MEASURE_LABELS[measure]} per day, ${report.from} to ${report.to}, stacked ${by === "project" ? "by project" : "by run kind"}`}
          onMouseLeave={() => setHover(null)}
        >
          <line x1={0} x2={CHART_W} y1={0} y2={0} stroke="var(--patchy-line)" stroke-dasharray="3 4" />
          <text x={0} y={-4} font-size="10" fill="var(--patchy-muted)" font-family="var(--font-mono)">
            {measureValue(max, measure)}
          </text>
          <line x1={0} x2={CHART_W} y1={CHART_H} y2={CHART_H} stroke="var(--patchy-line-2)" />
          {bars.map((rects, i) => {
            const slot = CHART_W / report.daily.length;
            return (
              <g key={report.daily[i].day} onMouseEnter={() => setHover(i)}>
                {/* The hit target is the whole day's column, larger than the mark. */}
                <rect x={i * slot} y={0} width={slot} height={CHART_H} fill="transparent" />
                {rects.map((r, j) => (
                  <rect
                    key={r.key}
                    x={r.x}
                    y={r.y}
                    width={r.width}
                    height={r.height}
                    rx={j === rects.length - 1 ? 2 : 0}
                    fill={colorOf(r.key, shown)}
                    opacity={hover === null || hover === i ? 1 : 0.55}
                  />
                ))}
              </g>
            );
          })}
          <text x={0} y={CHART_H + 14} font-size="10" fill="var(--patchy-muted)" font-family="var(--font-mono)">
            {report.from}
          </text>
          <text x={CHART_W} y={CHART_H + 14} font-size="10" fill="var(--patchy-muted)" text-anchor="end" font-family="var(--font-mono)">
            {report.to}
          </text>
        </svg>
        {hovered ? <DayTip day={hovered} by={by} measure={measure} shown={shown} /> : null}
      </div>
      <details class="mt-3 text-[12.5px]">
        <summary class="cursor-pointer text-muted">Table view</summary>
        <DailyTable days={report.daily} />
      </details>
    </section>
  );
}

function DayTip({ day, by, measure, shown }: { day: UsageDay; by: StackBy; measure: UsageMeasure; shown: string[] }) {
  const parts = stackDay(day, by, measure, shown);
  return (
    <div class="pointer-events-none absolute top-0 right-0 min-w-[200px] rounded-lg border border-line-2 bg-surface-2 px-3 py-2 text-[12px] shadow-card" role="status">
      <div class="font-mono text-fg">{day.day}</div>
      <div class="text-muted">
        {measureValue(segmentValue(day, measure), measure)} {measure === "tokens" ? "tokens" : ""} · {count(day.runs)} runs
      </div>
      {parts.map((p) => (
        <div key={p.key} class="flex items-center justify-between gap-3">
          <span class="inline-flex items-center gap-1.5">
            <span class="inline-block size-2 rounded-[2px]" style={{ background: colorOf(p.key, shown) }} />
            {keyLabel(p.key, by)}
          </span>
          <span class="font-mono">{measureValue(p.value, measure)}</span>
        </div>
      ))}
    </div>
  );
}

function DailyTable({ days }: { days: UsageDay[] }) {
  return (
    <div class="mt-2 max-h-[320px] overflow-auto">
      <table class="w-full border-collapse font-mono text-[11.5px]">
        <thead>
          <tr class="text-left text-muted">
            <th class="py-1 pr-3">day</th>
            <th class="py-1 pr-3 text-right">runs</th>
            <th class="py-1 pr-3 text-right">tokens</th>
            <th class="py-1 pr-3 text-right">cost</th>
            <th class="py-1 pr-3 text-right">equivalent API cost</th>
            <th class="py-1 text-right">unmetered</th>
          </tr>
        </thead>
        <tbody>
          {[...days].reverse().map((d) => (
            <tr key={d.day} class="border-t border-line">
              <td class="py-1 pr-3">{d.day}</td>
              <td class="py-1 pr-3 text-right">{count(d.runs)}</td>
              <td class="py-1 pr-3 text-right">{count(d.tokens)}</td>
              <td class="py-1 pr-3 text-right">{formatMicroUSD(d.costMicroUSD)}</td>
              <td class="py-1 pr-3 text-right">{formatMicroUSD(d.notionalCostMicroUSD)}</td>
              <td class="py-1 text-right">{count(d.unmeteredTokens)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function FiguresTable<T extends UsageFigures>({
  title,
  rows,
  name,
  onPick,
}: {
  title: string;
  rows: T[];
  name: (r: T) => string;
  onPick?: (r: T) => void;
}) {
  if (rows.length === 0) return null;
  return (
    <section class="mt-3.5 rounded-xl border border-line bg-surface px-4.5 py-4" aria-label={title}>
      <h2 class="ps-heading">{title}</h2>
      <table class="mt-2 w-full border-collapse text-[12.5px]">
        <thead>
          <tr class="text-left text-muted">
            <th class="py-1 pr-3 font-normal" />
            <th class="py-1 pr-3 text-right font-normal">runs</th>
            <th class="py-1 pr-3 text-right font-normal">tokens</th>
            <th class="py-1 pr-3 text-right font-normal">cost</th>
            <th class="py-1 pr-3 text-right font-normal">equivalent API cost</th>
            <th class="py-1 text-right font-normal">unmetered</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={name(r)} class="border-t border-line">
              <td class="py-1.5 pr-3">
                {onPick ? (
                  <button type="button" class="cursor-pointer border-0 bg-transparent p-0 text-ink underline-offset-2 hover:underline" onClick={() => onPick(r)}>
                    {name(r)}
                  </button>
                ) : (
                  name(r)
                )}
              </td>
              <td class="py-1.5 pr-3 text-right font-mono">{count(r.runs)}</td>
              <td class="py-1.5 pr-3 text-right font-mono">{count(r.tokens)}</td>
              <td class="py-1.5 pr-3 text-right font-mono">{formatMicroUSD(r.costMicroUSD)}</td>
              <td class="py-1.5 pr-3 text-right font-mono">{formatMicroUSD(r.notionalCostMicroUSD)}</td>
              <td class="py-1.5 text-right font-mono">{count(r.unmeteredTokens)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function TopIntents({ report }: { report: UsageReport }) {
  return (
    <section class="mt-3.5 rounded-xl border border-line bg-surface px-4.5 py-4" aria-label="Top intents">
      <h2 class="ps-heading">Top intents</h2>
      <p class="mt-1 mb-0 text-[12px] text-muted">
        The most expensive intents of {report.topIntentMonths.length > 0 ? report.topIntentMonths.join(", ") : "the window"} (kept
        per month).
      </p>
      {report.topIntents.length === 0 ? (
        <p class="mt-2 mb-0 text-muted">No intent runs.</p>
      ) : (
        <ol class="mt-2 mb-0 list-none p-0 text-[12.5px]">
          {report.topIntents.map((ti) => (
            <li key={ti.intent} class="flex flex-wrap items-baseline justify-between gap-3 border-t border-line py-1.5">
              <span>
                <a href={`#/intents/${encodeURIComponent(ti.intent)}`}>{ti.intent}</a>{" "}
                <span class="text-muted">{ti.project}</span>
              </span>
              <span class="font-mono text-muted">
                {count(ti.runs)} runs · {count(ti.tokens)} tokens · {formatMicroUSD(ti.costMicroUSD)}
                {ti.notionalCostMicroUSD > 0 ? ` + ${formatMicroUSD(ti.notionalCostMicroUSD)} equivalent` : ""}
              </span>
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}

export function UsageView({ all, access }: { all: boolean; access: UsageAccess | null }) {
  const [days, setDays] = useState(30);
  const [user, setUser] = useState<string>("");
  const [project, setProject] = useState<string>("");
  const [state, setState] = useState<Loaded>({ kind: "loading" });

  const load = useCallback(async () => {
    try {
      setState({ kind: "report", report: await fetchUsage(all, { days, user, project }) });
    } catch (e) {
      if (e instanceof AuthRequiredError) setState({ kind: "auth" });
      else if (e instanceof ForbiddenError) setState({ kind: "denied", message: e.message });
      else setState({ kind: "error", message: e instanceof Error ? e.message : String(e) });
    }
  }, [all, days, user, project]);

  useEffect(() => {
    setState({ kind: "loading" });
    void load();
  }, [load]);
  useEffect(() => {
    setUser("");
    setProject("");
  }, [all]);

  if (state.kind === "loading") return <div class="px-5 py-11 text-center text-muted">Loading…</div>;
  if (state.kind === "auth") {
    return (
      <Panel title="Authentication required">
        <p class="mt-0">Sign in with GitHub to see your usage.</p>
        <a class="ps-action ps-action--primary no-underline" href={signInURL()}>
          Sign in
        </a>
      </Panel>
    );
  }
  if (state.kind === "denied") return <Panel title="Usage unavailable">{state.message}</Panel>;
  if (state.kind === "error") return <Panel title="Cannot load usage">{state.message}</Panel>;

  const report = state.report;
  const subject =
    report.view === "self"
      ? "Your usage"
      : report.view === "all"
        ? "Everyone's usage"
        : [report.user ? personLabel(report.user, report.login) : "", report.project].filter(Boolean).join(" in ");

  return (
    <div>
      <header class="mb-3.5 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="m-0 text-[21px] tracking-tight">{subject}</h1>
          <p class="mt-1 mb-0 text-[12.5px] text-muted">
            {report.from} to {report.to} (UTC). Metered by the egress broker, priced at list rates.
          </p>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          {access?.all ? (
            <Toggle
              label="Whose usage"
              value={all ? "all" : "self"}
              onChange={(v) => (location.hash = hrefForUsage(v === "all"))}
              options={[["self", "mine"], ["all", "everyone"]]}
            />
          ) : null}
          <Toggle label="Window" value={days} onChange={setDays} options={WINDOWS.map((d) => [d, `${d} days`] as [number, string])} />
        </div>
      </header>
      {all && (user || project) ? (
        <div class="ps-note mb-3.5">
          Showing {subject}.{" "}
          <button type="button" class="ps-action" onClick={() => { setUser(""); setProject(""); }}>
            Show everyone
          </button>
        </div>
      ) : null}
      <Tiles t={report.totals} />
      <DailyChart report={report} />
      <Apart t={report.totals} />
      <TopIntents report={report} />
      <FiguresTable
        title="People"
        rows={report.people ?? []}
        name={(p) => personLabel(p.user, p.login)}
        onPick={all ? (p) => setUser(p.user) : undefined}
      />
      <FiguresTable
        title="Projects"
        rows={report.projects ?? []}
        name={(p) => p.project}
        onPick={all ? (p) => { setUser(report.view === "user" ? report.user : ""); setProject(p.project); } : undefined}
      />
    </div>
  );
}
