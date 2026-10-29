// The evidence of an evaluate run's round, under its run panel: each
// approved outcome with maul's decided result and check line, every check
// (its steps and, for a browser check, the Playwright spec maul compiled),
// the ledger index, disputes and anomalies. A transcripts-tier reader also
// gets the evaluator's own claims (verdict, note, cited entries), each
// ledger entry's arguments and output and the evidence screenshots. Every
// string from the agent or the application is shown as plain text.

import { useEffect, useState } from "preact/hooks";
import type { EvaluationCheck, EvaluationLedgerEntry, EvaluationOutcome, RunEvaluation } from "../types";
import { fetchRunEvaluation } from "../api";
import { formatDate } from "../format";
import { HANDOFF_REASONS, evidenceImageURL, resultTone, shortDigest } from "../intents";
import { PlainText } from "./PlainText";
import { Pill } from "./Pills";

function useEvaluation(intent: string, run: string) {
  const [data, setData] = useState<RunEvaluation | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    setData(null);
    setError(null);
    fetchRunEvaluation(intent, run)
      .then((v) => live && setData(v))
      .catch((e: unknown) => live && setError(e instanceof Error ? e.message : String(e)));
    return () => {
      live = false;
    };
  }, [intent, run]);
  return { data, error };
}

function seqs(list?: number[]): string {
  return list && list.length > 0 ? list.map((n) => `#${n}`).join(", ") : "";
}

function Outcome({ o, transcripts }: { o: EvaluationOutcome; transcripts: boolean }) {
  return (
    <li class="rounded-[11px] border border-line px-3.5 py-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="ps-mono-tag">{o.id}</span>
        {o.result ? <Pill tone={resultTone(o.result)}>{o.result}</Pill> : <Pill tone="neutral">not decided</Pill>}
        <span class="ps-chip">{o.kind}</span>
        {o.manual ? <span class="ps-chip">manual</span> : null}
      </div>
      <p class="mt-2 mb-0 text-[13px]">{o.outcome}</p>
      {o.line ? <p class="mt-1.5 mb-0 font-mono text-[12px] text-muted">checked by: {o.line}</p> : null}
      {o.reason ? <p class="mt-1.5 mb-0 text-[12px] text-muted">{o.reason}</p> : null}
      {transcripts && o.verdict ? (
        <p class="mt-1.5 mb-0 text-[12px] text-faint">
          The evaluator said {o.verdict}
          {o.cited && o.cited.length > 0 ? `, citing ${seqs(o.cited)}` : ""}.
        </p>
      ) : null}
      {transcripts && o.note ? (
        <div class="mt-2">
          <PlainText label="the evaluator's note" text={o.note} maxHeight={false} />
        </div>
      ) : null}
    </li>
  );
}

function Check({ c }: { c: EvaluationCheck }) {
  const run = c.confirmation ?? c.replay;
  return (
    <li class="rounded-[11px] border border-line px-3.5 py-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="ps-mono-tag">{c.id}</span>
        <span class="ps-chip">
          {c.form} · {c.source} · {shortDigest(c.digest)}
        </span>
        {c.replay ? <Pill tone={resultTone(c.replay.result)}>replay {c.replay.result}</Pill> : null}
        {c.confirmation ? (
          <Pill tone={resultTone(c.confirmation.result)}>confirmation {c.confirmation.result}</Pill>
        ) : null}
        {c.disputed ? <Pill tone="amber">disputed</Pill> : null}
      </div>
      <p class="mt-1.5 mb-2 text-[12px] text-muted">
        For outcome <span class="font-mono">{c.outcome}</span>
        {run ? ` · ledger #${run.seq}${run.dryRunSeq ? ` (dry run #${run.dryRunSeq})` : ""} · token ${run.token}` : ""}
      </p>
      <PlainText label="steps, written by the evaluator" text={c.steps} />
      {c.compiled ? (
        <details class="mt-2">
          <summary class="cursor-pointer text-[12px] text-muted">
            Compiled Playwright spec{c.compiledToken ? ` (run token ${c.compiledToken})` : ""}
          </summary>
          <div class="mt-2">
            <PlainText label="compiled by maul" text={c.compiled} />
          </div>
        </details>
      ) : null}
    </li>
  );
}

function LedgerRow({ e, intent, run }: { e: EvaluationLedgerEntry; intent: string; run: string }) {
  const images = (e.artifacts ?? []).map((a) => (a.image ? evidenceImageURL(intent, run, a.image) : "")).filter(Boolean);
  const detail = e.args || e.output || e.outputDropped || images.length > 0;
  const head = (
    <span class="font-mono text-[12px]">
      #{e.seq} {e.kind} {e.verb}
      {e.refused ? " · refused" : ""}
      {e.exit ? ` · exit ${e.exit}` : ""}
      {e.artifacts && e.artifacts.length > 0 ? ` · ${e.artifacts.length} artifact(s)` : ""}
      <span class="text-faint"> · {shortDigest(e.resultSHA256)}</span>
    </span>
  );
  if (!detail) return <li class="py-1">{head}</li>;
  return (
    <li class="py-1">
      <details>
        <summary class="cursor-pointer">{head}</summary>
        <div class="mt-2 flex flex-col gap-2">
          {e.args ? <PlainText label="request, from the evaluator" text={e.args} /> : null}
          {e.output ? <PlainText label="output, recorded by maul (app output)" text={e.output} /> : null}
          {e.outputDropped ? (
            <p class="m-0 text-[12px] text-faint">The output was not kept whole; only its sha256 is.</p>
          ) : null}
          {images.map((src) => (
            <img key={src} src={src} alt={`evidence of ledger entry ${e.seq}`} class="max-w-full rounded border border-line" />
          ))}
        </div>
      </details>
    </li>
  );
}

export function EvaluationEvidence({ intent, run }: { intent: string; run: string }) {
  const { data, error } = useEvaluation(intent, run);
  if (error) return <p class="mt-6 text-[12px] text-faint">The evaluation could not be loaded: {error}</p>;
  if (!data) return null;
  const transcripts = data.tier === "transcripts";
  return (
    <section class="mt-6">
      <h2 class="ps-heading mb-2">Evaluation</h2>
      <p class="mt-0 mb-3 text-[12.5px] text-muted">
        Round {data.round}, attempt {data.attempt}
        {data.rung ? ` · ${data.rung}` : ""} · {data.verification}
        {data.phase ? ` · ${data.phase}` : ""}
        {data.reason ? ` (${data.reason})` : ""}
        {data.current ? "" : " · a later round has superseded this one's results"}
      </p>
      {data.handoff ? (
        <p class="mt-0 mb-3 text-[12.5px]">
          {data.handoff.released
            ? `Preview handed over${data.handoff.releasedAt ? ` ${formatDate(data.handoff.releasedAt)}` : ""}: ${
                HANDOFF_REASONS[data.handoff.reason ?? ""] ?? data.handoff.reason ?? ""
              }.`
            : `Preview held (${data.handoff.mode}) until the evaluation settles.`}
        </p>
      ) : null}

      <h3 class="ps-heading mt-4 mb-2">Outcomes</h3>
      <ul class="m-0 flex list-none flex-col gap-2 p-0">
        {data.outcomes.map((o) => (
          <Outcome key={o.id} o={o} transcripts={transcripts} />
        ))}
      </ul>

      {data.anomalies && data.anomalies.length > 0 ? (
        <>
          <h3 class="ps-heading mt-4 mb-2">Anomalies</h3>
          <ul class="m-0 flex list-disc flex-col gap-1 pl-5 text-[12.5px]">
            {data.anomalies.map((a, i) => (
              <li key={i}>
                {a.summary}
                {a.route ? <span class="font-mono text-muted"> · {a.route}</span> : null}
                {a.ledger && a.ledger.length > 0 ? <span class="text-faint"> · {seqs(a.ledger)}</span> : null}
              </li>
            ))}
          </ul>
        </>
      ) : null}

      {data.disputes && data.disputes.length > 0 ? (
        <>
          <h3 class="ps-heading mt-4 mb-2">Disputes</h3>
          <ul class="m-0 flex list-disc flex-col gap-1 pl-5 text-[12.5px]">
            {data.disputes.map((d) => (
              <li key={d.checkDigest}>
                <span class="font-mono">{shortDigest(d.checkDigest)}</span>
                {d.reason ? `: ${d.reason}` : ""}
                {d.replacement ? <span class="text-muted"> · replacement {d.replacement}</span> : null}
                {d.ledger && d.ledger.length > 0 ? <span class="text-faint"> · {seqs(d.ledger)}</span> : null}
              </li>
            ))}
          </ul>
        </>
      ) : null}

      <h3 class="ps-heading mt-4 mb-2">Checks</h3>
      {data.checks.length === 0 ? (
        <p class="m-0 text-[12px] text-faint">No check was replayed or confirmed in this round.</p>
      ) : (
        <ul class="m-0 flex list-none flex-col gap-2 p-0">
          {data.checks.map((c) => (
            <Check key={c.digest} c={c} />
          ))}
        </ul>
      )}

      {data.ledger ? (
        <>
          <h3 class="ps-heading mt-4 mb-2">
            Ledger · {data.ledger.count} entries · {shortDigest(data.ledger.sha256)}
          </h3>
          <ul class="m-0 list-none p-0">
            {data.ledger.entries.map((e) => (
              <LedgerRow key={e.seq} e={e} intent={intent} run={run} />
            ))}
          </ul>
          {!transcripts ? (
            <p class="mt-1.5 mb-0 text-[12px] text-faint">
              Ledger outputs, screenshots and the evaluator's notes need the transcripts tier on this project.
            </p>
          ) : null}
        </>
      ) : null}

      {data.capture ? (
        <p class="mt-3 mb-0 text-[12px] text-muted">
          Captured for the evaluator: {data.capture.events ?? 0} slot events
          {(data.capture.components ?? []).map(
            (c) => ` · ${c.name} ${c.bytes.toLocaleString("en-US")} log bytes${c.truncated ? " (older dropped)" : ""}`,
          )}
        </p>
      ) : null}
    </section>
  );
}
