// Demo data of the Project settings page: one Project, storefront, whose
// viewer is an approver who may save. Helm (server-side apply) and kubectl
// (client-side apply) each manage some of its fields, so the warnings about
// other field managers show. A save is judged by a small stand-in for the
// server's rules and changes only this in-memory copy; live mode never
// reads it.

import type {
  ProjectSettingsChange,
  ProjectSettingsConfirmation,
  ProjectSettingsField,
  ProjectSettingsManager,
  ProjectSettingsRefusal,
  ProjectSettingsRequest,
  ProjectSettingsResult,
  ProjectSettingsView,
} from "../types";
import {
  APPROVERS_FIELD,
  CHECKS_FIELD,
  CLASS_FIELD,
  SUBAGENTS_FIELD,
  WORKFLOWS_FIELD,
  type SaveAnswer,
  draftChanges,
  storedValue,
  valueWords,
  withFieldValue,
} from "../projectSettings.ts";

const HELM: ProjectSettingsManager = { name: "helm", operation: "Apply" };
const KUBECTL: ProjectSettingsManager = { name: "kubectl-client-side-apply", operation: "Update" };

// How long the demo's intent-controller takes to observe a save.
const OBSERVE_MS = 2500;

function ago(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString();
}

type FieldOpts = Partial<Omit<ProjectSettingsField, "id" | "group" | "kind">>;

function field(
  id: string,
  group: ProjectSettingsField["group"],
  kind: ProjectSettingsField["kind"],
  o: FieldOpts = {},
): ProjectSettingsField {
  return {
    id,
    group,
    kind,
    min: 0,
    max: 0,
    default: 0,
    ceiling: 0,
    floor: 0,
    locked: "",
    effect: "live",
    managers: [],
    ...o,
  };
}

// The controller's per-stage ceilings: turns, tokens, timeout in seconds.
const STAGES: [string, number, number, number][] = [
  ["plan", 40, 200_000, 1200],
  ["build", 150, 800_000, 3600],
  ["revise", 80, 400_000, 2700],
  ["evaluate", 100, 400_000, 1200],
];

function fields(): ProjectSettingsField[] {
  const out: ProjectSettingsField[] = [
    field(APPROVERS_FIELD, "approvers", "logins", { min: 1, max: 20, managers: [HELM] }),
    field("verification.enabled", "evaluator", "bool"),
    field("verification.handoff", "handoff", "handoff", { effect: "new-intents" }),
    field("verification.handoffMax", "handoff", "seconds", { min: 600, max: 14_400, default: 3600, floor: 1800 }),
    field("verification.maxToolCalls", "evaluator", "count", {
      min: 50,
      max: 300,
      default: 300,
      ceiling: 300,
      floor: 100,
      effect: "next-round",
    }),
    field("models.plan", "models", "model", { effect: "new-runs" }),
    field("models.build", "models", "model", { effect: "new-runs" }),
    field("models.evaluate", "models", "model", { effect: "new-runs", managers: [HELM] }),
    field("limits.maxCostMicroUSD", "cost", "microUSD", {
      min: 1,
      max: 1_000_000_000,
      default: 10_000_000,
      ceiling: 25_000_000,
    }),
    field("limits.maxEvaluateCostMicroUSD", "cost", "microUSD", {
      min: 1,
      max: 1_000_000_000,
      default: 6_000_000,
      ceiling: 6_000_000,
      floor: 2_000_000,
    }),
    field("limits.maxRevisions", "rounds", "count", { max: 20, default: 3, ceiling: 10, managers: [HELM] }),
    field("limits.maxCheckFixes", "rounds", "count", { max: 20, default: 2, ceiling: 10 }),
    field("limits.maxVerifyFixes", "rounds", "count", { max: 10, default: 2, ceiling: 5 }),
    field("limits.maxVerifications", "rounds", "count", { min: 1, max: 100, default: 20, ceiling: 50, floor: 3 }),
    field("limits.maxActiveIntents", "activeIntents", "count", { min: 1, max: 20, default: 2, ceiling: 5 }),
  ];
  for (const [stage, turns, tokens, timeout] of STAGES) {
    const evaluate = stage === "evaluate";
    out.push(
      field(`limits.${stage}.maxTurns`, "stages", "count", {
        max: 1000,
        default: turns,
        ceiling: turns,
        floor: evaluate ? 40 : 0,
        effect: "new-runs",
      }),
      field(`limits.${stage}.tokenBudget`, "stages", "count", {
        max: 100_000_000,
        default: tokens,
        ceiling: tokens,
        floor: evaluate ? 100_000 : 0,
        effect: "new-runs",
      }),
      // The evaluate stage's cost has the evaluation cost's floor.
      field(`limits.${stage}.maxCostMicroUSD`, "stages", "microUSD", {
        max: 1_000_000_000,
        floor: evaluate ? 2_000_000 : 0,
        effect: "new-runs",
      }),
      field(`limits.${stage}.timeout`, "stages", "seconds", {
        min: 60,
        max: 14_400,
        default: timeout,
        ceiling: timeout,
        floor: evaluate ? 600 : 0,
        effect: "new-runs",
      }),
    );
  }
  out.push(
    field(SUBAGENTS_FIELD, "delegation", "bool", { effect: "new-runs" }),
    // The demo controller allows subagents and not workflows, so the
    // workflows switch shows locked.
    field(WORKFLOWS_FIELD, "delegation", "bool", { effect: "new-runs", locked: "not-admitted" }),
    field(CHECKS_FIELD, "checks", "checks", { max: 32, managers: [KUBECTL] }),
    field("checks.timeout", "checks", "seconds", { min: 60, max: 21_600, default: 1800 }),
    field("checks.rerunFailed", "checks", "bool"),
    field(CLASS_FIELD, "resources", "class", { effect: "new-runs", managers: [HELM] }),
  );
  return out;
}

function initial(): ProjectSettingsView {
  const stage = () => ({ maxTurns: 0, tokenBudget: 0, maxCostMicroUSD: 0, timeoutSeconds: 0 });
  return {
    project: "storefront",
    generation: 7,
    observedGeneration: 7,
    schema: 2,
    canEdit: true,
    reason: "",
    settings: {
      approvers: ["octo-lead", "octocat", "mona"],
      verification: { enabled: true, handoff: "gated", handoffMaxSeconds: 0, maxToolCalls: 0 },
      models: { plan: "", build: "", evaluate: "anthropic/claude-sonnet-4-6" },
      limits: {
        maxCostMicroUSD: 10_000_000,
        maxEvaluateCostMicroUSD: 0,
        maxRevisions: 5,
        maxCheckFixes: 2,
        maxVerifyFixes: null,
        maxVerifications: 0,
        maxActiveIntents: 2,
      },
      stages: { plan: stage(), build: { ...stage(), maxTurns: 120 }, revise: stage(), evaluate: stage() },
      delegation: { subagents: false, workflows: false },
      checks: { fix: ["test", "lint"], timeoutSeconds: 1800, rerunFailed: false },
      repositories: [
        { name: "web", agentResourceClass: "small" },
        { name: "api", agentResourceClass: "" },
      ],
    },
    fields: fields(),
    approvers: [
      { login: "octo-lead", state: "ok", you: false, managers: [HELM] },
      { login: "octocat", state: "ok", you: true, managers: [] },
      { login: "mona", state: "no-write-access", you: false, managers: [HELM] },
    ],
    approversResolvedAt: ago(4),
    approversStale: false,
    options: {
      published: true,
      models: ["anthropic/claude-opus-4-6", "anthropic/claude-sonnet-4-6", "anthropic/claude-haiku-4-5"],
      defaultModels: {
        plan: "anthropic/claude-opus-4-6",
        build: "anthropic/claude-sonnet-4-6",
        evaluate: "anthropic/claude-sonnet-4-6",
      },
      resourceClasses: [
        { name: "small", requestCPU: "1", requestMemory: "2Gi", limitCPU: "2", limitMemory: "4Gi" },
        { name: "large", requestCPU: "4", requestMemory: "8Gi", limitCPU: "8", limitMemory: "16Gi" },
      ],
      evaluator: true,
      costBudgets: false,
      subagents: true,
      workflows: false,
    },
    allow: { approvers: true, disableEvaluator: true, emptyChecks: false },
    facts: {
      intentRepository: "https://github.com/acme/intents",
      repositories: [
        {
          name: "web",
          url: "https://github.com/acme/storefront",
          previewed: true,
          previewPath: "/",
          previewImage: "123456789012.dkr.ecr.us-east-1.amazonaws.com/maul/previews/storefront",
          previewPort: 8080,
          previewReadinessPath: "/healthz",
          classManagers: [HELM],
        },
        {
          name: "api",
          url: "https://github.com/acme/storefront-api",
          previewed: false,
          previewPath: "",
          previewImage: "",
          previewPort: 0,
          previewReadinessPath: "",
          classManagers: [],
        },
      ],
      triggerLabel: "maul:storefront",
      approveLabel: "maul:approved",
      harness: "claude",
      surfaces: ["browser"],
      suspend: false,
      requireRepositoryImage: true,
      manualGatesStatus: false,
      postLogs: false,
      activeIntents: 3,
    },
    lastEdit: { githubID: 583231, login: "octo-lead", at: ago(3 * 24 * 60), fields: ["limits.maxRevisions"], more: 0 },
    conditions: [
      {
        type: "Ready",
        status: "True",
        reason: "Validated",
        text: "the project is ready: its repositories resolve and the App can act on them",
      },
      {
        type: "ApproversResolved",
        status: "True",
        reason: "Resolved",
        text: "GitHub answered who each approver is",
      },
    ],
  };
}

let demo: ProjectSettingsView | null = null;
// observedAt is when the demo's controller catches up with the last save.
let observedAt = 0;

function state(): ProjectSettingsView {
  if (!demo) demo = initial();
  if (Date.now() >= observedAt) demo.observedGeneration = demo.generation;
  return demo;
}

// mockProjectSettings is the demo Project's view; null for any other name.
export function mockProjectSettings(name: string): ProjectSettingsView | null {
  return name === "storefront" ? structuredClone(state()) : null;
}

function answer(status: number, result: Partial<ProjectSettingsResult>, generation: number): SaveAnswer {
  return {
    status,
    message: "",
    result: { reason: "", generation, changes: [], refusals: [], confirm: [], ...result },
  };
}

// mockSaveProjectSettings judges a save as the server would, in short:
// the generation, the operator's locks, switches and ceilings, the
// approver rules and the confirmations; then stores it.
export function mockSaveProjectSettings(name: string, req: ProjectSettingsRequest): SaveAnswer {
  const v = name === "storefront" ? state() : null;
  if (!v) return { status: 404, result: null, message: "not found" };
  if (req.generation !== v.generation) return answer(409, { reason: "generation-changed" }, v.generation);
  if (req.schema !== v.schema) return answer(409, { reason: "schema-changed" }, v.generation);

  const changes = draftChanges(v.settings, req.settings, v.fields);
  const refusals: ProjectSettingsRefusal[] = [];
  const confirm: ProjectSettingsConfirmation[] = [];
  const refuse = (c: (typeof changes)[number], reason: string, limit = 0, value = "") =>
    refusals.push({ field: c.field, item: c.item, reason, limit, value });
  const ask = (c: (typeof changes)[number], kind: string, key: string, value = "") =>
    confirm.push({ key, kind, field: c.field, value });
  const you = v.approvers.find((a) => a.you)?.login ?? "";

  for (const c of changes) {
    const f = v.fields.find((x) => x.id === c.field)!;
    const delegation = c.field === SUBAGENTS_FIELD || c.field === WORKFLOWS_FIELD;
    if (delegation) {
      // Only turning a switch on is judged, against what is allowed.
      const allowed = c.field === SUBAGENTS_FIELD ? v.options.subagents : v.options.workflows;
      if (f.locked === "locked") refuse(c, "locked");
      else if (c.to === true && !allowed) refuse(c, "delegation-not-admitted");
    } else if (f.locked) refuse(c, f.locked === "locked" ? "locked" : "admits-unpublished");
    else if (c.field === APPROVERS_FIELD) {
      if (c.added.length + c.removed.length > 1) refuse(c, "approvers-one-at-a-time");
      else if (c.removed.includes(you)) refuse(c, "approvers-self-removal", 0, you);
      c.added.forEach((l) => ask(c, "approver-add", `approver-add:${l}`, l));
      c.removed.forEach((l) => ask(c, "approver-remove", `approver-remove:${l}`, l));
    } else if (c.field === "verification.enabled" && c.to === false) {
      ask(c, "evaluator-off", "evaluator-off");
    } else if (c.field === "verification.handoff" && c.to === "early") {
      ask(c, "handoff", "handoff:early", "early");
    } else if (c.field === CHECKS_FIELD && (c.to as string[]).length === 0) {
      if (!v.allow.emptyChecks) refuse(c, "checks-empty-not-allowed");
      else ask(c, "checks-empty", "checks-empty");
    } else if (typeof c.to === "number" && f.ceiling > 0 && c.to > f.ceiling && c.to > ((c.from as number) ?? 0)) {
      refuse(c, "above-ceiling", f.ceiling, String(c.to));
    }
  }
  const moved = changes.some((c) => c.field === SUBAGENTS_FIELD || c.field === WORKFLOWS_FIELD);
  if (moved && req.settings.delegation.workflows && !req.settings.delegation.subagents) {
    refusals.push({ field: WORKFLOWS_FIELD, item: "", reason: "workflows-need-subagents", limit: 0, value: "" });
  }
  if (refusals.length) return answer(422, { reason: "refused", refusals }, v.generation);
  const missing = confirm.filter((c) => !req.confirm.includes(c.key));
  if (missing.length) return answer(409, { reason: "confirm-required", confirm: missing }, v.generation);
  if (changes.length === 0) return answer(200, {}, v.generation);

  const made: ProjectSettingsChange[] = [];
  for (const c of changes) {
    const f = v.fields.find((x) => x.id === c.field)!;
    const to = storedValue(f, c.to);
    v.settings = withFieldValue(v.settings, c.field, to, c.item);
    made.push({
      field: c.field,
      item: c.item,
      group: f.group,
      from: valueWords(f, c.from),
      to: valueWords(f, to),
      added: c.added,
      removed: c.removed,
      effect: f.effect,
    });
  }
  v.generation += 1;
  observedAt = Date.now() + OBSERVE_MS;
  v.approvers = v.settings.approvers.map(
    (login) =>
      v.approvers.find((a) => a.login === login) ?? { login, state: "unresolved", you: false, managers: [] },
  );
  v.lastEdit = {
    githubID: 1,
    login: you,
    at: new Date().toISOString(),
    fields: [...new Set(made.map((c) => c.field))],
    more: 0,
  };
  return answer(200, { changes: made }, v.generation);
}
