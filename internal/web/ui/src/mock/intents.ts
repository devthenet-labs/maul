// Demo data for the intents views (demo kit builds). Live mode never reads
// it. Shapes mirror the wire types exactly, so a field added to types.ts
// shows up here as a type error.

import type {
  IntentBoard,
  IntentCard,
  IntentDetail,
  IntentLimits,
  IntentPlanText,
  IntentRunDetail,
  IntentRunRow,
  Me,
  RunEvaluation,
  RunOutput,
  TranscriptTurn,
} from "../types";

const LIMITS: IntentLimits = { maxRevisions: 3, maxCheckFixes: 2, maxCostMicroUSD: 10_000_000, maxAttempts: 2 };

function ago(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString();
}

function cards(): IntentCard[] {
  return [
    {
      name: "storefront-12",
      project: "storefront",
      issue: 12,
      phase: "Building",
      column: "building",
      summary: "Add a health endpoint with a readiness probe",
      repositories: ["acme/storefront"],
      runningRuns: [
        {
          name: "storefront-12-bld-r1-web-a1",
          stage: "build",
          repository: "acme/storefront",
          round: 1,
          attempt: 1,
          phase: "Running",
          startedAt: ago(23),
        },
      ],
      attempt: { stage: "build", current: 1, max: 2 },
      revisions: 0,
      checkFixes: 0,
      costMicroUSD: 1_840_000,
      requestedBy: "octocat",
      requestedAt: ago(140),
      phaseSince: ago(24),
    },
    {
      name: "storefront-9",
      project: "storefront",
      issue: 9,
      phase: "AwaitingApproval",
      column: "approval",
      summary: "Paginate the order history",
      repositories: ["acme/storefront"],
      revisions: 0,
      checkFixes: 0,
      costMicroUSD: 620_000,
      requestedBy: "octocat",
      requestedAt: ago(300),
      phaseSince: ago(180),
    },
    {
      name: "storefront-7",
      project: "storefront",
      issue: 7,
      phase: "Blocked",
      column: "review",
      blockedFrom: "InReview",
      blockedReasons: ["the revision limit is reached (3 of 3)"],
      summary: "Rename the cart API",
      pullRequests: [{ repository: "acme/storefront", number: 41, state: "open" }],
      preview: { phase: "Ready", revision: "3f2a9c1d0b7e" },
      revisions: 3,
      checkFixes: 1,
      costMicroUSD: 7_310_000,
      requestedBy: "hubot",
      requestedAt: ago(4000),
      phaseSince: ago(900),
    },
    {
      name: "storefront-3",
      project: "storefront",
      issue: 3,
      phase: "Merged",
      column: "done",
      summary: "Fix the currency rounding",
      pullRequests: [{ repository: "acme/storefront", number: 33, state: "merged", mergedAt: ago(2000) }],
      revisions: 1,
      checkFixes: 0,
      costMicroUSD: 2_100_000,
      requestedBy: "octocat",
      requestedAt: ago(9000),
      phaseSince: ago(2000),
      completedAt: ago(2000),
    },
  ];
}

export function mockMe(): Me {
  return { name: "demo", loggedIn: true, projects: [{ name: "storefront", tier: "transcripts", settings: true }] };
}

export function mockIntentBoard(): IntentBoard {
  return {
    generatedAt: new Date().toISOString(),
    projects: [
      {
        name: "storefront",
        tier: "transcripts",
        repositories: [{ key: "web", slug: "acme/storefront" }],
        limits: LIMITS,
      },
    ],
    intents: cards(),
  };
}

function runs(card: IntentCard): IntentRunRow[] {
  const plan: IntentRunRow = {
    name: `${card.name}-plan-r1-a1`,
    stage: "plan",
    repository: "acme/storefront",
    round: 1,
    attempt: 1,
    phase: "Complete",
    outcome: "ok",
    reason: "ok: completed",
    createdAt: ago(120),
    startedAt: ago(119),
    finishedAt: ago(104),
    costMicroUSD: 620_000,
    usage: { inputTokens: 210_000, outputTokens: 9_800, costMicroUSD: 620_000 },
    imageSource: "default",
    transcript: { turns: 4 },
    grant: { maxTurns: 80, tokenBudget: 400_000, timeoutMilliseconds: 1_800_000 },
  };
  if (card.name === "storefront-7") {
    return [
      plan,
      {
        name: `${card.name}-ev-r1-a1`,
        stage: "evaluate",
        repository: "acme/storefront",
        round: 1,
        attempt: 1,
        phase: "Complete",
        outcome: "ok",
        reason: "ok: completed",
        createdAt: ago(960),
        startedAt: ago(958),
        finishedAt: ago(940),
        costMicroUSD: 1_120_000,
        imageSource: "default",
        grant: { maxTurns: 120, timeoutMilliseconds: 1_200_000 },
      },
    ];
  }
  if (card.phase !== "Building") return [plan];
  return [
    plan,
    {
      name: `${card.name}-bld-r1-web-a1`,
      stage: "build",
      repository: "acme/storefront",
      round: 1,
      attempt: 1,
      phase: "Running",
      createdAt: ago(24),
      startedAt: ago(23),
      imageSource: "repository",
      imageDigest: "9c1e7a42b3d0",
      grant: { maxTurns: 150, tokenBudget: 800_000, timeoutMilliseconds: 3_600_000 },
      running: true,
    },
  ];
}

export function mockIntentDetail(name: string): IntentDetail | null {
  const card = cards().find((c) => c.name === name);
  if (!card) return null;
  return {
    ...card,
    tier: "transcripts",
    limits: LIMITS,
    phaseTimes: [
      { phase: "Pending", at: card.requestedAt ?? ago(140) },
      { phase: "Planning", at: ago(120) },
      { phase: "AwaitingApproval", at: ago(104) },
      ...(card.phase === "Building" ? [{ phase: "Building" as const, at: ago(24) }] : []),
    ],
    input: { revision: 1, digest: `sha256:e3f1c09a5b7d2468ace013579bdf2468ace013579bdf02468ace13579bdf0246` },
    plan: { revision: 1, digest: `sha256:9c1e7a42b3d05f6e8a1b2c3d4e5f60718293a4b5c6d7e8f9012a3b4c5d6e7f80`, summary: card.summary, postedAt: ago(104) },
    approval:
      card.phase === "Building"
        ? {
            by: "octocat",
            source: "label",
            at: ago(25),
            planRevision: 1,
            planDigest: `sha256:9c1e7a42b3d05f6e8a1b2c3d4e5f60718293a4b5c6d7e8f9012a3b4c5d6e7f80`,
            inputDigest: `sha256:e3f1c09a5b7d2468ace013579bdf2468ace013579bdf02468ace13579bdf0246`,
          }
        : undefined,
    runs: runs(card),
    planTexts: [1],
  };
}

export function mockIntentRun(intent: string, run: string): IntentRunDetail | null {
  const detail = mockIntentDetail(intent);
  const row = detail?.runs.find((r) => r.name === run);
  if (!detail || !row) return null;
  return {
    ...row,
    intent,
    project: detail.project,
    tier: "transcripts",
    countedAttempt: row.attempt,
    lastAttempt: false,
    limits: LIMITS,
    intentCostMicroUSD: detail.costMicroUSD,
    job: row.running
      ? { createdAt: ago(24), startedAt: ago(23), deadlineSeconds: 5400, idleTimeoutSeconds: 1200 }
      : undefined,
    report: row.stage === "plan" ? "# Plan\n\nAdd GET /healthz returning 200 once the store is reachable.\n" : undefined,
  };
}

// mockRunEvaluation is the evidence of the demo's one evaluate run: one
// outcome seen to hold on an http call and the evaluator's own page
// script, one left unsure because its verdict cites only a refused call,
// and a look-around anomaly. Demo builds have no server, so no evidence
// image is named.
export function mockRunEvaluation(intent: string, run: string): RunEvaluation | null {
  const detail = mockIntentRun(intent, run);
  if (!detail || detail.stage !== "evaluate") return null;
  const sha = (c: string) => c.repeat(64);
  return {
    intent,
    run,
    tier: "transcripts",
    verification: `${intent}-v1-a1`,
    round: 1,
    attempt: 1,
    phase: "Observed",
    current: true,
    lead:
      "maul counts a verdict only when it cites calls the evaluator made in this evaluation through maul's tools; " +
      "whether a cited call shows what the verdict says is for the reviewers to judge.",
    handoff: { mode: "gated", released: true, releasedAt: ago(939), reason: "unconfirmed" },
    outcomes: [
      {
        id: "cart-renamed",
        outcome: "The cart API answers on /api/basket",
        kind: "change",
        result: "passed",
        reason: "seen-to-hold",
        words: "seen to hold by maul's evaluator",
        cites: [2, 3],
        verdict: "pass",
        cited: [2, 3],
        note: "Added an item and read it back under the new path; the page shows it too.",
      },
      {
        id: "cart-page",
        outcome: "The cart page still lists added items",
        kind: "keep",
        result: "unsure",
        reason: "no-valid-evidence",
        words: "unsure; the evaluator's verdict cites no call of its own in this evaluation that maul counts",
        verdict: "pass",
        cited: [4],
        note: "The page lists the item; the snapshot was refused at the call limit.",
      },
    ],
    anomalies: [{ summary: "Console error on /cart: TypeError: total is undefined", route: "/cart", ledger: [3] }],
    ledger: {
      count: 5,
      sha256: sha("c"),
      outputs: true,
      entries: [
        { seq: 1, kind: "runner", verb: "session", resultSHA256: sha("1"), output: "browser session open" },
        {
          seq: 2,
          kind: "agent",
          verb: "http",
          resultSHA256: sha("2"),
          args: '{"method":"POST","path":"/api/basket","body":{"sku":"tea"}}',
          output: 'HTTP/1.1 200 OK\n{"items":[{"sku":"tea"}]}',
        },
        {
          seq: 3,
          kind: "agent",
          verb: "browser",
          own: true,
          resultSHA256: sha("3"),
          args: '{"verb":"eval","args":["() => document.querySelectorAll(\'[data-testid=cart-row]\').length"]}',
          output: "### Result\n1",
          artifacts: [{ sha256: sha("e") }],
        },
        { seq: 4, kind: "agent", verb: "browser", refused: true, resultSHA256: sha("4"), outputDropped: true },
        { seq: 5, kind: "relay", verb: "finish", resultSHA256: sha("5"), output: "finished" },
      ],
    },
    capture: { components: [{ name: "web", bytes: 18_432 }], events: 3 },
  };
}

export function mockIntentPlan(intent: string, revision: number): IntentPlanText | null {
  if (!mockIntentDetail(intent)) return null;
  return {
    intent,
    revision,
    current: true,
    digest: `sha256:9c1e7a42b3d05f6e8a1b2c3d4e5f60718293a4b5c6d7e8f9012a3b4c5d6e7f80`,
    text: "---\nsummary: Add a health endpoint\n---\n# Plan\n\n1. Add GET /healthz.\n2. Probe the store.\n",
  };
}

// mockRunTurns is a build that hands the store's tests to a subagent: its
// turns name the Agent call they ran under (parent), so the panel indents
// them there.
export function mockRunTurns(): TranscriptTurn[] {
  return [
    { seq: 1, at: ago(22), role: "assistant", kind: "text", text: "Reading the router to find where routes register." },
    { seq: 2, at: ago(21), role: "assistant", kind: "tool_use", tool: "Read", text: "internal/http/router.go", call: "toolu_read1" },
    { seq: 3, at: ago(21), role: "user", kind: "tool_result", text: "package http …" },
    { seq: 4, at: ago(19), role: "assistant", kind: "tool_use", tool: "Agent", text: "Find how the store is probed in tests", call: "toolu_agent1" },
    { seq: 5, at: ago(19), role: "system", kind: "notice", text: "subagent Explore started", parent: "toolu_agent1" },
    { seq: 6, at: ago(18), role: "assistant", kind: "tool_use", tool: "Grep", text: "store.Ping", call: "toolu_grep1", parent: "toolu_agent1" },
    { seq: 7, at: ago(18), role: "user", kind: "tool_result", text: "internal/store/store_test.go:41: s.Ping(ctx)", parent: "toolu_agent1" },
    { seq: 8, at: ago(17), role: "system", kind: "notice", text: "delegated work finished: completed, 1 tool use", parent: "toolu_agent1" },
    { seq: 9, at: ago(17), role: "user", kind: "tool_result", text: "The store is probed with s.Ping(ctx) in store_test.go." },
    { seq: 10, at: ago(9), role: "assistant", kind: "tool_use", tool: "Bash", text: "npm run test:ci", call: "toolu_bash1" },
  ];
}

// mockRunOutput is what the open Bash tool's "npm run test:ci" prints, in
// the chunks the stream delivers it in: a few lines at a time, one stretch
// the agent left out (the panel shows it as lines not shown), then the
// summary and the command's last chunk.
export function mockRunOutput(): RunOutput[] {
  const files = ["router", "healthz", "store", "cart", "checkout", "catalog", "search", "session", "pricing"];
  const lines = ["> storefront@1.4.0 test:ci", "> vitest run --reporter=verbose", ""];
  lines.push(" RUN  v3.2.4 /workspace/storefront");
  files.forEach((file, f) => {
    for (let t = 1; t <= 10; t++) lines.push(` ✓ src/${file}.test.ts > case ${t} ${(f * 7 + t * 3) % 40}ms`);
  });
  lines.push("", " Test Files  9 passed (9)", "      Tests  90 passed (90)", "   Duration  14.2s");
  const task = "b578qoc1g";
  const sent = (n: number) => n < 41 || n > 62; // lines 41-62 never arrive
  const chunks: RunOutput[] = [];
  for (let i = 0; i < lines.length; ) {
    if (!sent(i + 1)) {
      i++;
      continue;
    }
    let j = i;
    while (j < lines.length && j - i < 1 + (chunks.length % 3) && sent(j + 1)) j++;
    chunks.push({ task, line: i + 1, lines: lines.slice(i, j) });
    i = j;
  }
  chunks.push({ task, line: lines.length + 1, lines: [], done: true });
  return chunks;
}
