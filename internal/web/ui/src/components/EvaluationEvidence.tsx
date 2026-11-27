// The evidence of an evaluate run's round, under its run panel: each
// approved outcome with maul's decided result in its words and the calls
// it counted for it (the verdict's cites in the round's ledger, held to
// its summary), the anomalies and the ledger index. A transcripts-tier reader also gets
// each cited call's request, an excerpt of its output, its screenshot and
// whether it ran the evaluator's own page script or program, the
// evaluator's own claims (verdict, note, cited entries), and each ledger
// entry's arguments and output. Every string from the agent or the
// application is shown as plain text.

import { useEffect, useState } from "preact/hooks";
import type { EvaluationBuild, EvaluationLedger, EvaluationLedgerEntry, EvaluationOutcome, RunEvaluation } from "../types";
import { fetchRunEvaluation } from "../api";
import { formatDate } from "../format";
import { type CitedCall, citedCalls, evidenceImageURL, handoffLine, resultTone, shortDigest } from "../intents";
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

// CitedCallRow is one call maul counted for an outcome: what it was, and to
// a transcripts-tier reader what it sent, how its output starts and its
// screenshot.
function CitedCallRow({ c }: { c: CitedCall }) {
  if (!c.indexed) return <li class="py-1 font-mono text-[12px] text-faint">#{c.seq} · not in this round's ledger index</li>;
  return (
    <li class="py-1">
      <p class="m-0 font-mono text-[12px]">
        #{c.seq} {c.verb}
        {c.base ? " · at the base" : ""}
        {c.exit ? ` · exit ${c.exit}` : ""}
        {c.own ? <span class="font-sans text-muted"> · {c.own}</span> : null}
      </p>
      {c.request || c.excerpt || c.outputNotKept || c.images.length > 0 ? (
        <div class="mt-1.5 flex flex-col gap-2">
          {c.request ? (
            <PlainText label={c.own ? `request, ${c.own}` : "request, from the evaluator"} text={c.request} />
          ) : null}
          {c.excerpt ? (
            <PlainText
              label={`${c.own ? "what it returned" : "output"}, recorded by maul${c.excerptCut ? ` (its start; the whole of it is ledger entry #${c.seq})` : ""}`}
              text={c.excerpt}
            />
          ) : null}
          {c.outputNotKept ? (
            <p class="m-0 text-[12px] text-faint">maul did not keep its output whole; only its sha256 is.</p>
          ) : null}
          {c.images.map((src) => (
            <img key={src} src={src} alt={`screenshot of ledger entry ${c.seq}`} class="max-w-full rounded border border-line" />
          ))}
        </div>
      ) : null}
    </li>
  );
}

function Outcome({
  o,
  transcripts,
  ledger,
  intent,
  run,
}: {
  o: EvaluationOutcome;
  transcripts: boolean;
  ledger?: EvaluationLedger;
  intent: string;
  run: string;
}) {
  const calls = citedCalls(o, ledger, intent, run);
  return (
    <li class="rounded-[11px] border border-line px-3.5 py-3">
      <div class="flex flex-wrap items-center gap-2">
        <span class="ps-mono-tag">{o.id}</span>
        {o.result ? <Pill tone={resultTone(o.result)}>{o.result}</Pill> : <Pill tone="neutral">not decided</Pill>}
        <span class="ps-chip">{o.kind}</span>
        {o.manual ? <span class="ps-chip">manual</span> : null}
      </div>
      <p class="mt-2 mb-0 text-[13px]">{o.outcome}</p>
      {o.words ? <p class="mt-1.5 mb-0 text-[12px] text-muted">{o.words}</p> : null}
      {calls.length > 0 ? (
        <>
          <p class="mt-2 mb-0 text-[12px] text-muted">Calls maul counted for it:</p>
          <ul class="m-0 list-none p-0">
            {calls.map((c) => (
              <CitedCallRow key={c.seq} c={c} />
            ))}
          </ul>
        </>
      ) : null}
      {transcripts && o.verdict ? (
        <p class="mt-1.5 mb-0 text-[12px] text-faint">
          The evaluator said {o.verdict}
          {o.cited && o.cited.length > 0 ? `, citing ${seqs(o.cited)}` : ""}
          {o.result ? `; maul counted ${o.cites && o.cites.length > 0 ? seqs(o.cites) : "none of them"}` : ""}.
        </p>
      ) : null}
      {transcripts && o.note ? (
        <div class="mt-2">
          <PlainText label="the evaluator's note, not checked by maul" text={o.note} maxHeight={false} />
        </div>
      ) : null}
    </li>
  );
}

function Builds({ builds }: { builds: EvaluationBuild[] }) {
  return (
    <>
      <h3 class="ps-heading mt-4 mb-2">Builds</h3>
      <ul class="m-0 list-none p-0">
        {builds.map((b) => (
          <li key={`${b.tree}-${b.seq}`} class="py-1 font-mono text-[12px]">
            #{b.seq} {b.tree} {b.result}
            {b.exitCode ? ` · exit ${b.exitCode}` : ""}
            {b.durationMs ? ` · ${(b.durationMs / 1000).toFixed(1)}s` : ""}
          </li>
        ))}
      </ul>
    </>
  );
}

function LedgerRow({ e, intent, run }: { e: EvaluationLedgerEntry; intent: string; run: string }) {
  const images = (e.artifacts ?? []).map((a) => (a.image ? evidenceImageURL(intent, run, a.image) : "")).filter(Boolean);
  const detail = e.args || e.output || e.outputDropped || images.length > 0;
  const head = (
    <span class="font-mono text-[12px]">
      #{e.seq} {e.kind} {e.verb}
      {e.side === "base" ? " · at the base" : ""}
      {e.own ? (e.verb === "browser" ? " · the evaluator's own page script" : " · the evaluator's own program") : ""}
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
        Round {data.round}, attempt {data.attempt} · {data.verification}
        {data.phase ? ` · ${data.phase}` : ""}
        {data.reason ? ` (${data.reason})` : ""}
        {data.current ? "" : " · a later round has superseded this one's results"}
      </p>
      {data.handoff ? (
        <p class="mt-0 mb-3 text-[12.5px]">
          {handoffLine(
            data.handoff,
            data.placement,
            data.handoff.releasedAt ? ` ${formatDate(data.handoff.releasedAt)}` : "",
          )}
        </p>
      ) : null}

      <h3 class="ps-heading mt-4 mb-2">Outcomes</h3>
      {data.lead ? <p class="mt-0 mb-2 text-[12px] text-muted">{data.lead}</p> : null}
      <ul class="m-0 flex list-none flex-col gap-2 p-0">
        {data.outcomes.map((o) => (
          <Outcome key={o.id} o={o} transcripts={transcripts} ledger={data.ledger} intent={intent} run={run} />
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
                {a.foundInRound ? <span class="text-muted"> · found in round {a.foundInRound}</span> : null}
              </li>
            ))}
          </ul>
        </>
      ) : null}

      {data.builds && data.builds.length > 0 ? <Builds builds={data.builds} /> : null}

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
              What each call sent and returned, which calls ran the evaluator's own page script or program,
              screenshots and the evaluator's notes need the transcripts tier on this project.
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
