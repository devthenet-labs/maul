// Pure helpers of the Project settings page: reading and writing a field of
// the settings document by its id, the text an input shows and what it
// reads back (dollars, durations), the draft's changes against what was
// loaded, re-applying a draft after the Project changed under it, and the
// page's wording for refusals, confirmations, locks and the other field
// managers. No DOM, so tests/projectSettings.test.mjs runs them. The server
// judges every save again (internal/projectsettings); nothing here is a
// guard, only an early answer.

import type {
  ProjectSettings,
  ProjectSettingsConfirmation,
  ProjectSettingsFacts,
  ProjectSettingsField,
  ProjectSettingsManager,
  ProjectSettingsRefusal,
  ProjectSettingsResult,
  ProjectSettingsView,
} from "./types";

// SettingValue is one field's value in the document's own terms: a number
// (micro-USD, whole seconds, a count; null for an unset round limit), a
// bool, a string, or a set of strings.
export type SettingValue = number | null | boolean | string | string[];

// The per-repository field; its value is one per repository, named by item.
export const CLASS_FIELD = "repositories.agentResourceClass";
export const APPROVERS_FIELD = "approvers.logins";
export const CHECKS_FIELD = "checks.fix";
export const SUBAGENTS_FIELD = "delegation.subagents";
export const WORKFLOWS_FIELD = "delegation.workflows";

// SCHEMA_DEFAULTED are the fields the Project schema defaults: the stored
// value is never unset, and a cleared input means the default
// (internal/projectsettings fields.go, `defaulted: true`).
const SCHEMA_DEFAULTED = new Set([
  "limits.maxCostMicroUSD",
  "limits.maxRevisions",
  "limits.maxCheckFixes",
  "limits.maxActiveIntents",
  "checks.timeout",
]);

// NULLABLE are the round limits where zero is a value of its own (no such
// rounds), so unset is null.
const NULLABLE = new Set(["limits.maxRevisions", "limits.maxCheckFixes", "limits.maxVerifyFixes"]);

// RETIRED_STAGES are stages whose limits and model the page never offers
// (the check reviewer, being retired: owner decision 8), whatever a server
// sends.
const RETIRED_STAGES = new Set(["checkReview"]);

// ---- document slots -----------------------------------------------------------

// fieldKey names one input: the field id, and the repository for the
// per-repository field.
export function fieldKey(id: string, item: string): string {
  return item ? `${id}[${item}]` : id;
}

// docPath is where a field id lives in the document.
function docPath(id: string): string[] {
  const parts = id.split(".");
  if (id === APPROVERS_FIELD) return ["approvers"];
  if (parts[0] === "limits" && parts.length === 3) parts.splice(0, 1, "stages");
  const leaf = parts[parts.length - 1];
  if (leaf === "timeout" || leaf === "handoffMax") parts[parts.length - 1] = `${leaf}Seconds`;
  return parts;
}

type Slot = { holder: Record<string, unknown>; key: string } | null;

function slot(doc: ProjectSettings, id: string, item: string): Slot {
  if (id === CLASS_FIELD) {
    const repo = doc.repositories.find((r) => r.name === item);
    return repo ? { holder: repo as unknown as Record<string, unknown>, key: "agentResourceClass" } : null;
  }
  const path = docPath(id);
  let holder = doc as unknown as Record<string, unknown>;
  for (const part of path.slice(0, -1)) {
    const next = holder[part];
    if (typeof next !== "object" || next === null) throw new Error(`no settings field ${id}`);
    holder = next as Record<string, unknown>;
  }
  const key = path[path.length - 1];
  if (!(key in holder)) throw new Error(`no settings field ${id}`);
  return { holder, key };
}

// fieldValue is a field's value in doc; undefined for the class of a
// repository doc does not hold. An id the document has no slot for throws.
export function fieldValue(doc: ProjectSettings, id: string, item = ""): SettingValue | undefined {
  const s = slot(doc, id, item);
  return s ? (s.holder[s.key] as SettingValue) : undefined;
}

// withFieldValue is a copy of doc with one field set; doc is left alone.
export function withFieldValue(doc: ProjectSettings, id: string, value: SettingValue, item = ""): ProjectSettings {
  const next = structuredClone(doc);
  const s = slot(next, id, item);
  if (s) s.holder[s.key] = Array.isArray(value) ? [...value] : value;
  return next;
}

// withDraftValue is withFieldValue as the form edits a draft: turning
// subagents off turns workflows off with it, since workflows need
// subagents and a save leaving them on alone is refused.
export function withDraftValue(doc: ProjectSettings, id: string, value: SettingValue, item = ""): ProjectSettings {
  const next = withFieldValue(doc, id, value, item);
  if (id === SUBAGENTS_FIELD && value === false) next.delegation.workflows = false;
  return next;
}

// shownText makes characters that render as nothing (controls, zero-width
// and bidi marks) visible as their code point, for operator-written text
// the page shows but must post back unchanged (check names).
export function shownText(s: string): string {
  return s.replace(
    /[\u0000-\u001f\u007f-\u009f\u00ad\u200b-\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]/g,
    (c) => `[U+${c.charCodeAt(0).toString(16).toUpperCase().padStart(4, "0")}]`,
  );
}

// ---- money and durations ----------------------------------------------------------

// microToDollars is micro-USD as dollars, at least two decimals and as many
// more as it needs to be exact.
export function microToDollars(micro: number): string {
  const whole = Math.floor(micro / 1_000_000);
  const frac = String(micro % 1_000_000)
    .padStart(6, "0")
    .replace(/0+$/, "");
  return `${whole}.${frac.padEnd(2, "0")}`;
}

// dollarsToMicro reads dollars ("12.50", "$25", "1,000") as micro-USD: 0
// for an empty input, null for anything that is not a non-negative amount
// to the micro-dollar.
export function dollarsToMicro(text: string): number | null {
  const t = text.trim().replace(/^\$/, "").replace(/,/g, "");
  if (text.trim() === "") return 0;
  const m = /^(\d+)(?:\.(\d{1,6}))?$/.exec(t);
  if (!m) return null;
  const micro = Number(m[1]) * 1_000_000 + Number((m[2] ?? "").padEnd(6, "0"));
  return Number.isSafeInteger(micro) ? micro : null;
}

// secondsToText is whole seconds as a duration ("1h30m"); "" for zero.
export function secondsToText(seconds: number): string {
  if (seconds <= 0) return "";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return `${h ? `${h}h` : ""}${m ? `${m}m` : ""}${s ? `${s}s` : ""}`;
}

// textToSeconds reads a duration ("1h30m", "90m", "45s") as whole seconds:
// 0 for an empty input, null for anything else.
export function textToSeconds(text: string): number | null {
  const t = text.replace(/\s+/g, "").toLowerCase();
  if (t === "") return 0;
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(t);
  if (!m || (!m[1] && !m[2] && !m[3])) return null;
  const s = Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0);
  return Number.isSafeInteger(s) ? s : null;
}

// ---- inputs -------------------------------------------------------------------------

// isUnset reports a number that states nothing: null for a round limit,
// zero for the rest.
function isUnset(f: ProjectSettingsField, v: SettingValue | undefined): boolean {
  return NULLABLE.has(f.id) ? v === null : v === 0;
}

// formatValue is a number in its field's own unit, as the page writes it.
export function formatValue(f: ProjectSettingsField, n: number): string {
  if (f.kind === "microUSD") return `$${microToDollars(n)}`;
  if (f.kind === "seconds") return secondsToText(n) || "0s";
  return n.toLocaleString("en-US");
}

// valueText is what a field's input shows for v: "" when unset.
export function valueText(f: ProjectSettingsField, v: SettingValue | undefined): string {
  if (typeof v !== "number" || isUnset(f, v)) return v === null || typeof v === "number" ? "" : String(v ?? "");
  if (f.kind === "microUSD") return microToDollars(v);
  if (f.kind === "seconds") return secondsToText(v);
  return String(v);
}

// parseFieldText reads a number input back: its value, or why it cannot be
// one. An empty input is unset, which for a field the schema defaults is
// that default.
export function parseFieldText(f: ProjectSettingsField, text: string): { value: number | null } | { error: string } {
  if (text.trim() === "") {
    if (SCHEMA_DEFAULTED.has(f.id)) return { value: f.default };
    return { value: NULLABLE.has(f.id) ? null : 0 };
  }
  if (f.kind === "microUSD") {
    const n = dollarsToMicro(text);
    return n === null ? { error: "Write an amount in dollars, such as 12.50." } : { value: n };
  }
  if (f.kind === "seconds") {
    const n = textToSeconds(text);
    return n === null ? { error: "Write a duration such as 45m, 1h30m or 90s." } : { value: n };
  }
  const t = text.trim().replace(/,/g, "");
  if (!/^\d+$/.test(t) || !Number.isSafeInteger(Number(t))) return { error: "Write a whole number." };
  return { value: Number(t) };
}

// ---- changes ----------------------------------------------------------------------------

// DraftChange is one setting the draft changes.
export interface DraftChange {
  key: string;
  field: string;
  item: string;
  from: SettingValue;
  to: SettingValue;
  // a set's entries the draft adds and removes; [] for other kinds.
  added: string[];
  removed: string[];
}

function isList(f: ProjectSettingsField): boolean {
  return f.kind === "logins" || f.kind === "checks";
}

// minus are the entries of a that b does not hold, each once, in a's order.
function minus(a: string[], b: string[]): string[] {
  return a.filter((s, i) => !b.includes(s) && a.indexOf(s) === i);
}

// storedValue is a value as the server stores and compares it: a cleared
// field the schema defaults is that default.
export function storedValue<T extends SettingValue | undefined>(f: ProjectSettingsField, v: T): T | number {
  return SCHEMA_DEFAULTED.has(f.id) && isUnset(f, v) ? f.default : v;
}

function same(f: ProjectSettingsField, a: SettingValue | undefined, b: SettingValue | undefined): boolean {
  if (isList(f)) {
    const x = (a ?? []) as string[];
    const y = (b ?? []) as string[];
    return minus(x, y).length === 0 && minus(y, x).length === 0;
  }
  return storedValue(f, a) === storedValue(f, b);
}

// slots are every input of the page in fields' order: one per field, one
// per repository for the per-repository field.
function slots(doc: ProjectSettings, fields: ProjectSettingsField[]): { f: ProjectSettingsField; item: string }[] {
  return fields.flatMap((f) =>
    f.id === CLASS_FIELD ? doc.repositories.map((r) => ({ f, item: r.name })) : [{ f, item: "" }],
  );
}

// draftChanges are the settings draft changes against loaded, in the
// page's order. Sets compare as sets, and a cleared field the schema
// defaults is its default, as the server compares them.
export function draftChanges(
  loaded: ProjectSettings,
  draft: ProjectSettings,
  fields: ProjectSettingsField[],
): DraftChange[] {
  const out: DraftChange[] = [];
  for (const { f, item } of slots(draft, fields)) {
    const from = fieldValue(loaded, f.id, item);
    const to = fieldValue(draft, f.id, item);
    if (from === undefined || to === undefined || same(f, from, to)) continue;
    const list = isList(f);
    out.push({
      key: fieldKey(f.id, item),
      field: f.id,
      item,
      from,
      to,
      added: list ? minus(to as string[], from as string[]) : [],
      removed: list ? minus(from as string[], to as string[]) : [],
    });
  }
  return out;
}

// Moved is a setting both the person and someone else changed since the
// page loaded, to different values.
export interface Moved {
  key: string;
  field: string;
  item: string;
  was: SettingValue;
  now: SettingValue;
  yours: SettingValue;
}

// rebaseDraft re-applies the person's edits (draft against old, the
// Project as the page loaded it) onto now, the Project as it is after the
// save was refused as stale. A set takes the entries they added and loses
// the ones they removed; any other setting takes their value, and when
// someone else changed it too it is listed as moved, for the person to
// review. An edit to a repository now gone is dropped, as is one turning
// workflows on once subagents are off (droppedText says which).
export function rebaseDraft(
  old: ProjectSettings,
  draft: ProjectSettings,
  now: ProjectSettings,
  fields: ProjectSettingsField[],
): { draft: ProjectSettings; moved: Moved[]; dropped: string[] } {
  let next = structuredClone(now);
  const moved: Moved[] = [];
  const dropped: string[] = [];
  for (const c of draftChanges(old, draft, fields)) {
    const f = fields.find((x) => x.id === c.field)!;
    const current = fieldValue(next, c.field, c.item);
    if (current === undefined) {
      dropped.push(c.key);
      continue;
    }
    if (isList(f)) {
      const kept = (current as string[]).filter((s) => !c.removed.includes(s));
      next = withFieldValue(next, c.field, [...kept, ...minus(c.added, kept)], c.item);
      continue;
    }
    if (!same(f, current, c.from) && !same(f, current, c.to)) {
      moved.push({ key: c.key, field: c.field, item: c.item, was: c.from, now: current, yours: c.to });
    }
    next = withDraftValue(next, c.field, c.to, c.item);
  }
  // Workflows need subagents: an edit turning workflows on cannot stand
  // once someone else turned subagents off.
  if (next.delegation.workflows && !next.delegation.subagents) {
    next = withFieldValue(next, WORKFLOWS_FIELD, false);
    dropped.push(WORKFLOWS_FIELD);
  }
  return { draft: next, moved, dropped };
}

// droppedText says why rebaseDraft dropped the edit at key.
export function droppedText(key: string): string {
  if (key === WORKFLOWS_FIELD) return `${key}: subagents are now off and workflows need them, so the edit was dropped.`;
  return `${key}: that repository is gone, so the edit was dropped.`;
}

// followView decides what a re-read of the Project (fresh) does to the
// person's draft. The draft was built on base: the settings its changes are
// listed against and the generation a save posts, which must both stay
// base's while the draft is. At the same generation nothing moves, nor at
// an older one (a re-read answering after the draft was reset). When
// someone else saved in between, the draft is rebased onto fresh as a save
// refused as stale is, so their change is never listed, nor saved, as the
// person's own edit putting the old value back.
export function followView(
  base: Pick<ProjectSettingsView, "generation" | "settings">,
  draft: ProjectSettings,
  fresh: Pick<ProjectSettingsView, "generation" | "settings" | "fields">,
): { rebased: boolean; draft: ProjectSettings; moved: Moved[]; dropped: string[] } {
  if (fresh.generation <= base.generation) return { rebased: false, draft, moved: [], dropped: [] };
  return { rebased: true, ...rebaseDraft(base.settings, draft, fresh.settings, fresh.fields) };
}

// ---- approvers ------------------------------------------------------------------------------

const LOGIN = /^[A-Za-z0-9][A-Za-z0-9_.-]*$/;
const MAX_LOGIN = 64;
const MAX_APPROVERS = 20;

// approverProblem says why draft cannot replace loaded as the approver
// list, or null when it can: one login added or removed per save, never
// the editor's own (you), and a valid list after it.
export function approverProblem(loaded: string[], draft: string[], you: string): string | null {
  const added = minus(draft, loaded);
  const removed = minus(loaded, draft);
  if (added.length + removed.length === 0) return null;
  if (added.length + removed.length > 1) return "Add or remove one approver per save.";
  if (you && removed.some((l) => l.toLowerCase() === you.toLowerCase())) {
    return "You cannot remove yourself: another approver can.";
  }
  if (draft.length === 0) return "A Project needs at least one approver.";
  if (draft.length > MAX_APPROVERS) return `A Project has at most ${MAX_APPROVERS} approvers.`;
  for (const l of added) {
    if (l.length > MAX_LOGIN) return `A GitHub login is at most ${MAX_LOGIN} characters.`;
    if (!LOGIN.test(l)) return `${l} is not a GitHub login.`;
  }
  const seen = new Set<string>();
  for (const l of draft) {
    if (seen.has(l.toLowerCase())) return `${l} is on the list twice (logins ignore case).`;
    seen.add(l.toLowerCase());
  }
  return null;
}

// ---- other field managers -------------------------------------------------------------------

// uniqueManagers names each manager once, Apply when it is recorded under
// both operations (its next apply is the one that stops on an edit), by
// name.
export function uniqueManagers(managers: ProjectSettingsManager[]): ProjectSettingsManager[] {
  const out: ProjectSettingsManager[] = [];
  for (const m of managers) {
    const i = out.findIndex((o) => o.name === m.name);
    if (i < 0) out.push({ ...m });
    else if (m.operation === "Apply") out[i].operation = "Apply";
  }
  return out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

// managerNotes say, per other manager of a field, what its next apply does
// to an edit made here: an Apply manager (server-side apply, Helm 4's
// default) stops with a conflict on a changed value it still states, and
// puts back a set entry it states that was removed here; an Update manager
// (client-side helm, kubectl apply) puts its own value back, and replaces a
// whole list. list: the field is a set.
export function managerNotes(managers: ProjectSettingsManager[], list: boolean): string[] {
  return uniqueManagers(managers).map((m) => {
    if (m.operation === "Apply") {
      return list
        ? `${m.name} also applies this list (server-side apply). An entry it applies that you remove here comes back ` +
            `on its next apply, without an error: remove it there too. An entry you add here stays.`
        : `${m.name} also applies this setting (server-side apply). Change it here and ${m.name}'s next apply that ` +
            `still states it, such as a default helm upgrade of maul-config, fails with a conflict on this field until ` +
            `its values say the same (Copy as values), or is forced and puts its value back.`;
    }
    return list
      ? `${m.name} also writes this list (client-side apply, such as kubectl apply). Its next apply of the list ` +
          `replaces the whole list with its own: make the same change there.`
      : `${m.name} also writes this setting (client-side apply, such as kubectl apply). Its next apply that sets it ` +
          `puts its own value back: set it in one place.`;
  });
}

// loginManagerNote says what the managers that apply one approver login do
// on their next apply once it is removed here; null when nothing happens.
export function loginManagerNote(managers: ProjectSettingsManager[], change: "remove" | "keep"): string | null {
  if (change !== "remove") return null;
  const ms = uniqueManagers(managers);
  if (ms.length === 0) return null;
  const apply = ms.filter((m) => m.operation === "Apply").map((m) => m.name);
  const update = ms.filter((m) => m.operation === "Update").map((m) => m.name);
  const parts: string[] = [];
  if (apply.length) {
    parts.push(`${apply.join(", ")} applies this login (server-side apply): it comes back on the next apply, without an ` +
      `error, unless it is removed there too.`);
  }
  if (update.length) {
    parts.push(`${update.join(", ")} writes the approver list (client-side apply): its next apply replaces the ` +
      `whole list and puts it back unless it is removed there too.`);
  }
  return parts.join(" ");
}

// ---- rows -----------------------------------------------------------------------------------

// stageRows are the stages the stage table shows, in the server's order,
// from the fields it sends: never the check reviewer.
export function stageRows(fields: ProjectSettingsField[]): string[] {
  const out: string[] = [];
  for (const f of fields) {
    const m = /^limits\.([A-Za-z]+)\.[A-Za-z]+$/.exec(f.id);
    if (f.group === "stages" && m && !RETIRED_STAGES.has(m[1]) && !out.includes(m[1])) out.push(m[1]);
  }
  return out;
}

// modelRows are the stages the models card shows: never the check reviewer.
export function modelRows(fields: ProjectSettingsField[]): string[] {
  return fields
    .filter((f) => f.group === "models" && f.id.startsWith("models."))
    .map((f) => f.id.slice("models.".length))
    .filter((s) => !RETIRED_STAGES.has(s));
}

// ---- wording -----------------------------------------------------------------------------------

const STAGE_LABELS: Record<string, string> = { plan: "Plan", build: "Build", revise: "Revise", evaluate: "Evaluate" };
const LEAF_LABELS: Record<string, string> = {
  maxTurns: "turns",
  tokenBudget: "tokens",
  maxCostMicroUSD: "cost",
  timeout: "time limit",
};

const FIELD_LABELS: Record<string, string> = {
  "approvers.logins": "Approvers",
  "verification.enabled": "Evaluator",
  "verification.handoff": "Hand-off",
  "verification.handoffMax": "Longest wait for the hand-off",
  "verification.maxToolCalls": "Evaluator tool calls per round",
  "models.plan": "Plan model",
  "models.build": "Build model (revisions too)",
  "models.evaluate": "Evaluate model",
  "limits.maxCostMicroUSD": "Cost per intent",
  "limits.maxEvaluateCostMicroUSD": "Evaluation cost per intent",
  "limits.maxRevisions": "Revisions",
  "limits.maxCheckFixes": "CI fix rounds",
  "limits.maxVerifyFixes": "Verify fix rounds",
  "limits.maxVerifications": "Verifications",
  "limits.maxActiveIntents": "Intents at once",
  "checks.fix": "Checks maul fixes",
  "checks.timeout": "Longest wait for checks",
  "checks.rerunFailed": "Rerun failed checks",
  "delegation.subagents": "Subagents in builds",
  "delegation.workflows": "Workflows in builds",
  "repositories.agentResourceClass": "Machine size",
};

// fieldLabel is a field's name on the page.
export function fieldLabel(id: string, item = ""): string {
  const stage = /^limits\.([A-Za-z]+)\.([A-Za-z]+)$/.exec(id);
  const base = stage
    ? `${STAGE_LABELS[stage[1]] ?? stage[1]} ${LEAF_LABELS[stage[2]] ?? stage[2]}`
    : (FIELD_LABELS[id] ?? id);
  return item ? `${base} (${item})` : base;
}

// EFFECTS say when a change takes effect.
export const EFFECTS: Record<string, string> = {
  live: "takes effect at once, open intents included",
  "new-runs": "applies to runs started after the save",
  "new-intents": "applies to intents whose hand-off is not decided yet",
  "next-round": "applies from the next evaluation round",
};

// LOCKS say why a field cannot be changed at all.
export const LOCKS: Record<string, string> = {
  locked: "The operator locked this setting.",
  "not-allowed": "The operator does not let this page change it.",
  "admits-unpublished": "intent-controller has not yet published the choices for this Project.",
  "no-classes": "The operator defines no machine sizes to choose from.",
  "not-admitted": "intent-controller does not allow this in this install.",
};

// lockText is why a field is locked, in words for the Project's harness.
export function lockText(locked: string, harness: string): string {
  if (locked === "not-admitted" && harness === "codex") return "codex runs never delegate.";
  return LOCKS[locked] ?? locked;
}

// delegationNote is what the page says beside a delegation switch, given
// the draft: a switch that is on where intent-controller does not allow it
// grants nothing (and can be turned off), and workflows need subagents.
// null for any other field, or nothing to say.
export function delegationNote(
  view: Pick<ProjectSettingsView, "settings" | "options" | "facts">,
  draft: ProjectSettings,
  id: string,
): string | null {
  if (id !== SUBAGENTS_FIELD && id !== WORKFLOWS_FIELD) return null;
  const on = fieldValue(draft, id) === true;
  const allowed = view.options.published && (id === SUBAGENTS_FIELD ? view.options.subagents : view.options.workflows);
  if (on && !allowed) {
    const why = view.facts.harness === "codex" ? "codex runs never delegate" : "not allowed in this install";
    return `On, but ${why}: it grants nothing. You can turn it off.`;
  }
  if (id === WORKFLOWS_FIELD && !draft.delegation.subagents) return "Needs subagents on.";
  return null;
}

// GATE says why the viewer may not save.
export const GATE: Record<string, string> = {
  "no-github-identity": "Your sign-in carries no GitHub account id, so maul cannot match you to an approver.",
  "not-approver": "Only the Project's approvers can change its settings.",
  "no-write-access": "GitHub gives your account no write access to the intent repository, which an approver needs.",
  "approvers-unresolved":
    "intent-controller has not yet matched the approvers to GitHub accounts. Try again in a few minutes.",
  "approvers-stale":
    "intent-controller has not checked the approvers with GitHub for over 30 minutes, so nobody can change settings until it does.",
  "read-only": "This server shows the settings read-only.",
  "cluster-refused": "The cluster refused the change: the status server may not write this Project.",
};

// valueWords is a value as a sentence says it.
export function valueWords(f: ProjectSettingsField | undefined, v: SettingValue | undefined): string {
  if (Array.isArray(v)) return v.length ? v.join(", ") : "none";
  if (typeof v === "boolean") return v ? "on" : "off";
  if (v === null || v === undefined || v === "") return "unset";
  if (typeof v === "string") return v;
  if (!f) return String(v);
  if (isUnset(f, v)) return "unset";
  return formatValue(f, v);
}

// refusalText is a refusal as the page says it, beside its input or, with
// the field named, on the page.
export function refusalText(r: ProjectSettingsRefusal, f?: ProjectSettingsField): string {
  const limit = f && (f.kind === "microUSD" || f.kind === "seconds" || f.kind === "count") ? formatValue(f, r.limit) : String(r.limit);
  switch (r.reason) {
    case "locked":
      return LOCKS.locked;
    case "out-of-range":
      return `Out of range (bound ${limit}).`;
    case "invalid":
      return r.field === "" ? "The cluster refused the save as invalid." : "The cluster refused this value as invalid.";
    case "approvers-not-allowed":
      return "The operator does not let this page change the approver list.";
    case "approvers-invalid":
      return `${r.value || "The list"} is not allowed on the approver list.`;
    case "approvers-one-at-a-time":
      return "Add or remove one approver per save.";
    case "approvers-self-removal":
      return "You cannot remove yourself: another approver can.";
    case "evaluator-off-not-allowed":
      return "The operator does not let this page turn the evaluator off.";
    case "handoff-early-not-allowed":
      return "The operator does not let this page hand previews over before the evaluation.";
    case "below-floor":
      return `Under the operator's floor of ${limit}.`;
    case "above-ceiling":
      return `Above the operator's ceiling of ${limit}.`;
    case "above-controller-ceiling":
      return `Above what intent-controller grants (${limit}).`;
    case "admits-unpublished":
      return LOCKS["admits-unpublished"];
    case "model-not-admitted":
      return `${r.value} is not a model intent-controller admits for this Project.`;
    case "class-not-admitted":
      return `${r.value} is not a machine size the operator defines.`;
    case "checks-empty-not-allowed":
      return "The operator does not let this page empty the list.";
    case "delegation-not-admitted":
      return "intent-controller does not allow this for this Project, so it cannot be turned on.";
    case "workflows-need-subagents":
      return "Workflows need subagents on.";
    case "run-budget-over-hourly":
      return `A run's budget would pass half the hourly spend limit (${limit}).`;
    case "run-budget-under-reserve":
      return `A run's budget would be under what ${r.value || "its model"} needs per request (${limit}).`;
    case "unknown-repository":
      return `There is no repository ${r.item || r.value}.`;
    case "duplicate-repository":
      return `Repository ${r.item || r.value} is named twice.`;
    case "not-a-duration":
      return "Not a duration.";
  }
  return `Refused (${r.reason}).`;
}

// Routed are a save's refusals: by input key, and those the page shows on
// its own (no field, or one the page has no input for).
export interface Routed {
  byKey: Record<string, string[]>;
  page: string[];
}

// routeRefusals puts each refusal beside the input it is about.
export function routeRefusals(refusals: ProjectSettingsRefusal[], fields: ProjectSettingsField[]): Routed {
  const out: Routed = { byKey: {}, page: [] };
  for (const r of refusals) {
    const f = fields.find((x) => x.id === r.field);
    const text = refusalText(r, f);
    if (!f) {
      out.page.push(r.field ? `${r.field}: ${text}` : text);
      continue;
    }
    const key = fieldKey(r.field, f.id === CLASS_FIELD ? r.item : "");
    (out.byKey[key] ??= []).push(text);
  }
  return out;
}

// confirmationText is what a person confirms: the change and what it does.
export function confirmationText(
  c: ProjectSettingsConfirmation,
  fields: ProjectSettingsField[],
  facts: Pick<ProjectSettingsFacts, "activeIntents">,
): string {
  const open = facts.activeIntents === 1 ? "1 open intent" : `${facts.activeIntents} open intents`;
  switch (c.kind) {
    case "approver-add":
      return `${c.value} becomes an approver: once GitHub reports their write access they can approve plans and change these settings.`;
    case "approver-remove":
      return `${c.value} is no longer an approver: their commands and edits stop counting at once.`;
    case "evaluator-off":
      return `maul stops verifying this Project's intents. ${open} get a final error on maul/acceptance and their previews are handed over unverified.`;
    case "handoff":
      return `Previews may be handed over before the evaluation ends (hand-off ${c.value || "the install's default"}), for new intents.`;
    case "below-floor": {
      const f = fields.find((x) => x.id === c.field);
      const n = Number(c.value);
      const v = f && Number.isFinite(n) ? formatValue(f, n) : c.value;
      return `${fieldLabel(c.field)} goes to ${v}, under the operator's floor: evaluations may end unconfirmed. ${open} now.`;
    }
    case "checks-empty":
      return `maul no longer waits for or fixes any CI check. ${open} now.`;
  }
  return `${fieldLabel(c.field)}: ${c.value || c.key}`;
}

// SaveOutcome is what the page does with a save's answer.
export type SaveOutcome =
  | { kind: "saved"; result: ProjectSettingsResult }
  | { kind: "stale" }
  | { kind: "reload" }
  | { kind: "confirm"; result: ProjectSettingsResult }
  | { kind: "retry" }
  | { kind: "refused"; result: ProjectSettingsResult }
  | { kind: "denied"; message: string }
  | { kind: "missing" }
  | { kind: "error"; message: string };

// SaveAnswer is a save's HTTP answer: the result when the server sent one.
export interface SaveAnswer {
  status: number;
  result: ProjectSettingsResult | null;
  message: string;
}

// classifySave reads a save's answer.
export function classifySave(a: SaveAnswer): SaveOutcome {
  const r = a.result;
  if (a.status === 200 && r) return { kind: "saved", result: r };
  if (a.status === 404) return { kind: "missing" };
  if (a.status === 403) {
    return { kind: "denied", message: (r && GATE[r.reason]) || a.message || "You may not change these settings." };
  }
  if (r && a.status === 409) {
    if (r.reason === "generation-changed") return { kind: "stale" };
    if (r.reason === "schema-changed") return { kind: "reload" };
    if (r.reason === "confirm-required") return { kind: "confirm", result: r };
    return { kind: "retry" };
  }
  if (r && (a.status === 422 || a.status === 400)) return { kind: "refused", result: r };
  return { kind: "error", message: a.message || `The save failed (HTTP ${a.status}).` };
}

// ---- Copy as values --------------------------------------------------------------------------------

const q = (s: string) => JSON.stringify(s);

// valuesYAML is the Project's editable settings as its entry under
// `projects` in the maul-config values file, so that file states what the
// page saved and its next apply neither conflicts nor puts values back.
// Unset settings are left out; strings are quoted.
export function valuesYAML(project: string, s: ProjectSettings): string {
  const lines: string[] = [
    "# Merge into this Project's entry in the maul-config values file. A setting",
    "# this page leaves unset is not listed; repositories carry only name and",
    "# machine size here, so merge them into your own entries by name.",
    "projects:",
    `  - name: ${q(project)}`,
    "    spec:",
  ];
  const block = (indent: number, name: string, body: string[]) => {
    if (body.length) lines.push(`${" ".repeat(indent)}${name}:`, ...body);
  };
  const pad = (indent: number) => " ".repeat(indent);
  const num = (indent: number, key: string, n: number | null) => (n ? [`${pad(indent)}${key}: ${n}`] : []);
  const dur = (indent: number, key: string, n: number) => (n ? [`${pad(indent)}${key}: ${q(secondsToText(n))}`] : []);
  const str = (indent: number, key: string, v: string) => (v ? [`${pad(indent)}${key}: ${q(v)}`] : []);
  const list = (v: string[]) => `[${v.map(q).join(", ")}]`;

  block(6, "approvers", [`${pad(8)}logins: ${list(s.approvers)}`]);
  block(6, "verification", [
    `${pad(8)}enabled: ${s.verification.enabled}`,
    ...str(8, "handoff", s.verification.handoff),
    ...dur(8, "handoffMax", s.verification.handoffMaxSeconds),
    ...num(8, "maxToolCalls", s.verification.maxToolCalls),
  ]);
  block(6, "models", [
    ...str(8, "plan", s.models.plan),
    ...str(8, "build", s.models.build),
    ...str(8, "evaluate", s.models.evaluate),
  ]);
  const l = s.limits;
  const round = (key: string, n: number | null) => (n === null ? [] : [`${pad(8)}${key}: ${n}`]);
  const stages: string[] = [];
  for (const [name, st] of Object.entries(s.stages)) {
    const body = [
      ...num(10, "maxTurns", st.maxTurns),
      ...num(10, "tokenBudget", st.tokenBudget),
      ...num(10, "maxCostMicroUSD", st.maxCostMicroUSD),
      ...dur(10, "timeout", st.timeoutSeconds),
    ];
    if (body.length) stages.push(`${pad(8)}${name}:`, ...body);
  }
  block(6, "limits", [
    ...num(8, "maxCostMicroUSD", l.maxCostMicroUSD),
    ...num(8, "maxEvaluateCostMicroUSD", l.maxEvaluateCostMicroUSD),
    ...round("maxRevisions", l.maxRevisions),
    ...round("maxCheckFixes", l.maxCheckFixes),
    ...round("maxVerifyFixes", l.maxVerifyFixes),
    ...num(8, "maxVerifications", l.maxVerifications),
    ...num(8, "maxActiveIntents", l.maxActiveIntents),
    ...stages,
  ]);
  block(6, "delegation", [
    `${pad(8)}subagents: ${s.delegation.subagents}`,
    `${pad(8)}workflows: ${s.delegation.workflows}`,
  ]);
  block(6, "checks", [
    ...(s.checks.fix.length ? [`${pad(8)}fix: ${list(s.checks.fix)}`] : []),
    ...dur(8, "timeout", s.checks.timeoutSeconds),
    `${pad(8)}rerunFailed: ${s.checks.rerunFailed}`,
  ]);
  block(
    6,
    "repositories",
    s.repositories.flatMap((r) => [`${pad(8)}- name: ${q(r.name)}`, ...str(10, "agentResourceClass", r.agentResourceClass)]),
  );
  return `${lines.join("\n")}\n`;
}
