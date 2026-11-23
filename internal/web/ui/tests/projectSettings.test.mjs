// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  approverProblem,
  classifySave,
  confirmationText,
  dollarsToMicro,
  draftChanges,
  fieldKey,
  fieldValue,
  followView,
  loginManagerNote,
  managerNotes,
  microToDollars,
  modelRows,
  parseFieldText,
  rebaseDraft,
  routeRefusals,
  secondsToText,
  shownText,
  stageRows,
  textToSeconds,
  uniqueManagers,
  valuesYAML,
  valueText,
  withFieldValue,
} from "../src/projectSettings.ts";
import { INTENTS_VIEW_FILES } from "../src/intents.ts";
import { mockProjectSettings, mockSaveProjectSettings } from "../src/mock/projectSettings.ts";
import { hrefForProjectSettings, isIntentsRoute, parseRoute } from "../src/router.ts";

// ---- fixtures ---------------------------------------------------------------

function field(id, kind, extra = {}) {
  return {
    id,
    group: "rounds",
    kind,
    min: 0,
    max: 0,
    default: 0,
    ceiling: 0,
    floor: 0,
    locked: "",
    effect: "live",
    managers: [],
    ...extra,
  };
}

const FIELDS = [
  field("approvers.logins", "logins", { group: "approvers", min: 1, max: 20 }),
  field("verification.enabled", "bool", { group: "evaluator" }),
  field("verification.handoff", "handoff", { group: "handoff", effect: "new-intents" }),
  field("verification.handoffMax", "seconds", { group: "handoff", min: 600, max: 14400, default: 3600 }),
  field("models.plan", "model", { group: "models", effect: "new-runs" }),
  field("limits.maxCostMicroUSD", "microUSD", { group: "cost", min: 1, max: 1e9, default: 10_000_000 }),
  field("limits.maxEvaluateCostMicroUSD", "microUSD", { group: "cost", min: 1, max: 1e9, default: 6_000_000 }),
  field("limits.maxRevisions", "count", { max: 20, default: 3 }),
  field("limits.maxVerifyFixes", "count", { max: 10, default: 2 }),
  field("limits.maxActiveIntents", "count", { group: "activeIntents", min: 1, max: 20, default: 2 }),
  field("limits.plan.maxTurns", "count", { group: "stages", max: 1000, default: 40, effect: "new-runs" }),
  field("limits.plan.timeout", "seconds", { group: "stages", min: 60, max: 14400, default: 1200, effect: "new-runs" }),
  field("limits.evaluate.maxCostMicroUSD", "microUSD", { group: "stages", max: 1e9, effect: "new-runs" }),
  field("checks.fix", "checks", { group: "checks", max: 32 }),
  field("checks.timeout", "seconds", { group: "checks", min: 60, max: 21600, default: 1800 }),
  field("checks.rerunFailed", "bool", { group: "checks" }),
  field("repositories.agentResourceClass", "class", { group: "resources", effect: "new-runs" }),
];

const byID = (id) => FIELDS.find((f) => f.id === id);

function settings() {
  const stage = () => ({ maxTurns: 0, tokenBudget: 0, maxCostMicroUSD: 0, timeoutSeconds: 0 });
  return {
    approvers: ["alice", "bob"],
    verification: { enabled: true, handoff: "", handoffMaxSeconds: 0, maxToolCalls: 0 },
    models: { plan: "", build: "", evaluate: "" },
    limits: {
      maxCostMicroUSD: 10_000_000,
      maxEvaluateCostMicroUSD: 0,
      maxRevisions: 3,
      maxCheckFixes: 2,
      maxVerifyFixes: null,
      maxVerifications: 0,
      maxActiveIntents: 2,
    },
    stages: { plan: stage(), build: stage(), revise: stage(), evaluate: stage() },
    checks: { fix: ["test", "lint"], timeoutSeconds: 1800, rerunFailed: false },
    repositories: [
      { name: "web", agentResourceClass: "small" },
      { name: "api", agentResourceClass: "" },
    ],
  };
}

const set = (doc, id, value, item) => withFieldValue(doc, id, value, item);

// ---- route ------------------------------------------------------------------

test("the project settings route round-trips", () => {
  assert.deepEqual(parseRoute("#/projects/storefront/settings"), { view: "projectSettings", name: "storefront" });
  for (const name of ["storefront", "a.b-c", "odd name/x"]) {
    assert.deepEqual(parseRoute(hrefForProjectSettings(name)), { view: "projectSettings", name });
  }
  assert.equal(hrefForProjectSettings("storefront"), "#/projects/storefront/settings");
  assert.deepEqual(parseRoute("#/projects/storefront"), { view: "list" });
  assert.deepEqual(parseRoute("#/projects"), { view: "list" });
  // It gates itself, outside the intents area's own routes.
  assert.equal(isIntentsRoute(parseRoute(hrefForProjectSettings("p"))), false);
});

// ---- dollars and durations ------------------------------------------------------

test("dollars convert to micro-USD and back", () => {
  assert.equal(microToDollars(12_500_000), "12.50");
  assert.equal(microToDollars(1_000_000), "1.00");
  assert.equal(microToDollars(1_234_567), "1.234567");
  assert.equal(microToDollars(1_500), "0.0015");
  assert.equal(microToDollars(1_000_000_000), "1000.00");
  for (const micro of [1, 10, 1_500, 999_999, 1_000_000, 12_500_000, 25_000_000, 1_000_000_000]) {
    assert.equal(dollarsToMicro(microToDollars(micro)), micro, String(micro));
  }
  assert.equal(dollarsToMicro("12.5"), 12_500_000);
  assert.equal(dollarsToMicro(" $25 "), 25_000_000);
  assert.equal(dollarsToMicro("0.000001"), 1);
  assert.equal(dollarsToMicro("1,000"), 1_000_000_000);
  assert.equal(dollarsToMicro(""), 0, "empty is unset");
  for (const bad of ["abc", "-1", "1.0000001", "1e3", "1.2.3", "$", "NaN", "Infinity", "0x10"]) {
    assert.equal(dollarsToMicro(bad), null, bad);
  }
});

test("durations convert to whole seconds and back", () => {
  assert.equal(secondsToText(5400), "1h30m");
  assert.equal(secondsToText(3600), "1h");
  assert.equal(secondsToText(600), "10m");
  assert.equal(secondsToText(45), "45s");
  assert.equal(secondsToText(3661), "1h1m1s");
  assert.equal(secondsToText(0), "");
  for (const s of [1, 59, 60, 61, 600, 1200, 3599, 3600, 5400, 14400, 21600]) {
    assert.equal(textToSeconds(secondsToText(s)), s, String(s));
  }
  assert.equal(textToSeconds("90m"), 5400);
  assert.equal(textToSeconds(" 1h 30m "), 5400);
  assert.equal(textToSeconds("2H"), 7200);
  assert.equal(textToSeconds(""), 0, "empty is unset");
  for (const bad of ["10", "1.5h", "1d", "m", "h1", "-5m", "1h-1m", "1m1h"]) {
    assert.equal(textToSeconds(bad), null, bad);
  }
});

test("an input's text reads back as its value, per kind", () => {
  const cost = byID("limits.maxCostMicroUSD");
  assert.equal(valueText(cost, 25_000_000), "25.00");
  assert.deepEqual(parseFieldText(cost, "25"), { value: 25_000_000 });
  // A schema-defaulted field cleared lands on its default.
  assert.deepEqual(parseFieldText(cost, ""), { value: 10_000_000 });
  assert.match(parseFieldText(cost, "lots").error, /dollars/);

  const evalCost = byID("limits.maxEvaluateCostMicroUSD");
  assert.equal(valueText(evalCost, 0), "", "unset shows empty");
  assert.deepEqual(parseFieldText(evalCost, ""), { value: 0 });

  const revisions = byID("limits.maxRevisions");
  assert.deepEqual(parseFieldText(revisions, "0"), { value: 0 }, "zero is a value of its own");
  assert.deepEqual(parseFieldText(revisions, ""), { value: 3 }, "cleared lands on the schema default");
  const verifyFixes = byID("limits.maxVerifyFixes");
  assert.deepEqual(parseFieldText(verifyFixes, ""), { value: null }, "unset is null");
  assert.equal(valueText(verifyFixes, null), "");
  assert.match(parseFieldText(revisions, "2.5").error, /whole number/);
  assert.match(parseFieldText(revisions, "-1").error, /whole number/);

  const timeout = byID("limits.plan.timeout");
  assert.equal(valueText(timeout, 1200), "20m");
  assert.deepEqual(parseFieldText(timeout, "45m"), { value: 2700 });
  assert.deepEqual(parseFieldText(timeout, ""), { value: 0 });
  assert.match(parseFieldText(timeout, "45").error, /45m/);
});

// ---- the diff -------------------------------------------------------------------

test("draftChanges lists what the draft changes against what was loaded", () => {
  const loaded = settings();
  assert.deepEqual(draftChanges(loaded, structuredClone(loaded), FIELDS), []);

  let draft = set(loaded, "limits.maxRevisions", 5);
  draft = set(draft, "approvers.logins", ["bob", "alice", "carol"]);
  draft = set(draft, "checks.fix", ["lint", "test"]); // same set, other order: no change
  draft = set(draft, "limits.plan.timeout", 2700);
  draft = set(draft, "repositories.agentResourceClass", "large", "api");
  draft = set(draft, "verification.enabled", false);
  const changes = draftChanges(loaded, draft, FIELDS);
  assert.deepEqual(
    changes.map((c) => [c.key, c.from, c.to]),
    [
      ["approvers.logins", ["alice", "bob"], ["bob", "alice", "carol"]],
      ["verification.enabled", true, false],
      ["limits.maxRevisions", 3, 5],
      ["limits.plan.timeout", 0, 2700],
      ["repositories.agentResourceClass[api]", "", "large"],
    ],
  );
  assert.deepEqual(changes[0].added, ["carol"]);
  assert.deepEqual(changes[0].removed, []);
  assert.equal(changes[4].item, "api");
  assert.equal(fieldValue(loaded, "limits.maxRevisions"), 3, "the loaded document is never changed");

  // A cleared schema-defaulted field equal to its default is no change; a
  // stated value equal to an unset one's landing default is one.
  assert.deepEqual(draftChanges(loaded, set(loaded, "limits.maxCostMicroUSD", 0), FIELDS), []);
  assert.equal(draftChanges(loaded, set(loaded, "limits.maxVerifyFixes", 2), FIELDS).length, 1);
  assert.equal(draftChanges(loaded, set(loaded, "limits.maxVerifyFixes", 0), FIELDS).length, 1);
});

test("fieldValue and withFieldValue reach every document slot by field id", () => {
  const doc = settings();
  assert.equal(fieldValue(doc, "verification.handoffMax"), 0);
  assert.equal(fieldValue(doc, "checks.timeout"), 1800);
  assert.equal(fieldValue(doc, "limits.evaluate.maxCostMicroUSD"), 0);
  assert.equal(fieldValue(doc, "repositories.agentResourceClass", "web"), "small");
  assert.equal(fieldValue(doc, "repositories.agentResourceClass", "gone"), undefined);
  const next = withFieldValue(doc, "verification.handoffMax", 1800);
  assert.equal(next.verification.handoffMaxSeconds, 1800);
  assert.equal(doc.verification.handoffMaxSeconds, 0);
  assert.equal(withFieldValue(doc, "limits.build.tokenBudget", 5).stages.build.tokenBudget, 5);
  assert.throws(() => fieldValue(doc, "suspend"));
});

// ---- after a 409 ------------------------------------------------------------------

test("rebaseDraft re-applies the person's edits onto the Project as it now is", () => {
  const old = settings();
  let draft = set(old, "limits.maxRevisions", 5);
  draft = set(draft, "approvers.logins", ["alice", "bob", "carol"]);
  draft = set(draft, "checks.fix", ["test"]);
  draft = set(draft, "limits.maxActiveIntents", 4);
  draft = set(draft, "repositories.agentResourceClass", "large", "api");

  // Meanwhile someone else raised maxActiveIntents to 3, added dave, added
  // the e2e check and changed the hand-off; nobody touched maxRevisions.
  let now = set(old, "limits.maxActiveIntents", 3);
  now = set(now, "approvers.logins", ["alice", "bob", "dave"]);
  now = set(now, "checks.fix", ["test", "lint", "e2e"]);
  now = set(now, "verification.handoff", "early");

  const { draft: rebased, moved, dropped } = rebaseDraft(old, draft, now, FIELDS);
  assert.equal(fieldValue(rebased, "limits.maxRevisions"), 5, "an edit nobody else made is kept");
  assert.equal(fieldValue(rebased, "verification.handoff"), "early", "their edit is kept");
  assert.deepEqual(fieldValue(rebased, "approvers.logins"), ["alice", "bob", "dave", "carol"]);
  assert.deepEqual(fieldValue(rebased, "checks.fix"), ["test", "e2e"], "lint stays removed, e2e stays");
  assert.equal(fieldValue(rebased, "limits.maxActiveIntents"), 4, "the person's value is kept for review");
  assert.equal(fieldValue(rebased, "repositories.agentResourceClass", "api"), "large");
  assert.deepEqual(moved, [{ key: "limits.maxActiveIntents", field: "limits.maxActiveIntents", item: "", was: 2, now: 3, yours: 4 }]);
  assert.deepEqual(dropped, []);
  // What the review then lists is against the new Project.
  assert.deepEqual(
    draftChanges(now, rebased, FIELDS).map((c) => c.key),
    ["approvers.logins", "limits.maxRevisions", "limits.maxActiveIntents", "checks.fix", "repositories.agentResourceClass[api]"],
  );

  // A repository the operator removed takes its edit with it.
  const fewer = structuredClone(now);
  fewer.repositories = fewer.repositories.filter((r) => r.name !== "api");
  const second = rebaseDraft(old, draft, fewer, FIELDS);
  assert.deepEqual(second.dropped, ["repositories.agentResourceClass[api]"]);
  assert.equal(second.draft.repositories.length, 1);

  // The same edit made by both is no conflict.
  const same = rebaseDraft(old, draft, set(now, "limits.maxActiveIntents", 4), FIELDS);
  assert.deepEqual(same.moved, []);
});

// After a save the page re-reads the Project until intent-controller has
// observed it. A re-read showing another approver's save must move the
// draft onto it, as a stale save would, never keep the old draft against
// the new settings: that showed their change as the person's own edit
// putting the old value back, and the next save, posting the new
// generation, reverted it with no 409.
test("followView rebases the draft when someone else saved while the page waited", () => {
  const view = (generation, doc) => ({ generation, observedGeneration: 7, settings: doc, fields: FIELDS });
  // A saved maxRevisions 3 -> 5 (generation 8); the draft is reset to it.
  const saved = set(settings(), "limits.maxRevisions", 5);
  const base = view(8, saved);
  // B then saves maxActiveIntents 2 -> 3 (generation 9).
  const fresh = view(9, set(saved, "limits.maxActiveIntents", 3));

  const unedited = followView(base, structuredClone(saved), fresh);
  assert.equal(unedited.rebased, true);
  assert.equal(fieldValue(unedited.draft, "limits.maxActiveIntents"), 3, "B's value survives");
  assert.deepEqual(draftChanges(fresh.settings, unedited.draft, FIELDS), [], "nothing is attributed to A");
  assert.deepEqual(unedited.moved, []);

  // A had started another edit: it is kept, and only it is A's.
  const edited = followView(base, set(saved, "limits.maxRevisions", 6), fresh);
  assert.equal(fieldValue(edited.draft, "limits.maxActiveIntents"), 3);
  assert.deepEqual(
    draftChanges(fresh.settings, edited.draft, FIELDS).map((c) => [c.key, c.from, c.to]),
    [["limits.maxRevisions", 5, 6]],
  );

  // A and B edited the same setting: A's value stays, listed as moved.
  const both = followView(base, set(saved, "limits.maxActiveIntents", 4), fresh);
  assert.deepEqual(both.moved.map((m) => [m.key, m.was, m.now, m.yours]), [["limits.maxActiveIntents", 2, 3, 4]]);

  // The same generation (only observedGeneration moved): the draft stays.
  const draft = set(saved, "limits.maxRevisions", 6);
  const same = followView(base, draft, { ...base, observedGeneration: 8 });
  assert.equal(same.rebased, false);
  assert.equal(same.draft, draft);

  // A re-read older than the draft's base (a poll answering after the
  // person's own save reset the draft) moves nothing either.
  const older = followView(view(9, fresh.settings), draft, base);
  assert.equal(older.rebased, false);
  assert.equal(older.draft, draft);
});

// ---- refusals -----------------------------------------------------------------------

test("routeRefusals puts each refusal beside its input, the rest on the page", () => {
  const routed = routeRefusals(
    [
      { field: "limits.maxRevisions", item: "", reason: "above-ceiling", limit: 10, value: "12" },
      { field: "repositories.agentResourceClass", item: "api", reason: "class-not-admitted", limit: 0, value: "huge" },
      { field: "limits.maxCostMicroUSD", item: "", reason: "invalid", limit: 0, value: "" },
      { field: "", item: "", reason: "invalid", limit: 0, value: "" },
      { field: "surfaces", item: "", reason: "invalid", limit: 0, value: "" },
      { field: "approvers.logins", item: "", reason: "approvers-self-removal", limit: 0, value: "alice" },
      { field: "limits.plan.timeout", item: "", reason: "above-controller-ceiling", limit: 1200, value: "1500" },
    ],
    FIELDS,
  );
  assert.deepEqual(Object.keys(routed.byKey).sort(), [
    "approvers.logins",
    "limits.maxCostMicroUSD",
    "limits.maxRevisions",
    "limits.plan.timeout",
    "repositories.agentResourceClass[api]",
  ]);
  assert.match(routed.byKey["limits.maxRevisions"][0], /10/);
  assert.match(routed.byKey["repositories.agentResourceClass[api]"][0], /huge/);
  assert.match(routed.byKey["approvers.logins"][0], /yourself/);
  assert.match(routed.byKey["limits.plan.timeout"][0], /20m/, "a duration limit reads as a duration");
  assert.equal(routed.page.length, 2, "a spec-wide refusal and one for a field not on the page");
  assert.match(routed.page[0], /cluster|refused/i);
  assert.match(routed.page[1], /surfaces/);
  assert.equal(fieldKey("repositories.agentResourceClass", "api"), "repositories.agentResourceClass[api]");
  assert.equal(fieldKey("limits.maxRevisions", ""), "limits.maxRevisions");
});

test("classifySave reads every answer the save can get", () => {
  const result = (reason, extra = {}) => ({ reason, generation: 4, changes: [], refusals: [], confirm: [], ...extra });
  assert.equal(classifySave({ status: 200, result: result(""), message: "" }).kind, "saved");
  assert.equal(classifySave({ status: 409, result: result("generation-changed"), message: "" }).kind, "stale");
  assert.equal(classifySave({ status: 409, result: result("schema-changed"), message: "" }).kind, "reload");
  assert.equal(classifySave({ status: 409, result: result("confirm-required"), message: "" }).kind, "confirm");
  assert.equal(classifySave({ status: 409, result: result("conflict"), message: "" }).kind, "retry");
  assert.equal(classifySave({ status: 422, result: result("refused"), message: "" }).kind, "refused");
  assert.equal(classifySave({ status: 400, result: result("invalid-document"), message: "" }).kind, "refused");
  const denied = classifySave({ status: 403, result: result("not-approver"), message: "" });
  assert.equal(denied.kind, "denied");
  assert.match(denied.message, /approver/);
  assert.match(classifySave({ status: 403, result: result("cluster-refused"), message: "" }).message, /cluster/);
  assert.equal(classifySave({ status: 404, result: null, message: "not found" }).kind, "missing");
  const limited = classifySave({ status: 429, result: null, message: "too many saves" });
  assert.equal(limited.kind, "error");
  assert.match(limited.message, /too many saves/);
  assert.equal(classifySave({ status: 403, result: null, message: "Snapshot is read-only." }).message, "Snapshot is read-only.");
});

// ---- approvers --------------------------------------------------------------------------

test("approverProblem allows one approver added or removed per save", () => {
  const loaded = ["alice", "bob"];
  assert.equal(approverProblem(loaded, ["alice", "bob"], "alice"), null);
  assert.equal(approverProblem(loaded, ["alice", "bob", "carol"], "alice"), null);
  assert.equal(approverProblem(loaded, ["alice"], "alice"), null);
  assert.match(approverProblem(loaded, ["alice", "bob", "carol", "dave"], "alice"), /one/);
  assert.match(approverProblem(loaded, ["alice", "carol"], "alice"), /one/, "a swap is two changes");
  assert.match(approverProblem(loaded, ["bob"], "alice"), /yourself/);
  assert.match(approverProblem(loaded, ["bob"], "ALICE"), /yourself/, "logins compare ignoring case");
  assert.match(approverProblem(loaded, ["alice", "bob", "Alice"], "bob"), /twice|case/);
  assert.match(approverProblem(loaded, ["alice", "bob", "-x"], "bob"), /login/);
  assert.match(approverProblem(loaded, ["alice", "bob", "a".repeat(65)], "bob"), /64/);
  assert.match(approverProblem(["alice"], [], ""), /at least one/);
  const twenty = Array.from({ length: 20 }, (_, i) => `u${i}`);
  assert.match(approverProblem(twenty, [...twenty, "u20"], "u0"), /20/);
});

// ---- other field managers (owner decision 9) ----------------------------------------------

test("managerNotes say what each operation's next apply does", () => {
  const helm = { name: "helm", operation: "Apply" };
  const kubectl = { name: "kubectl-client-side-apply", operation: "Update" };
  const [applied] = managerNotes([helm], false);
  assert.match(applied, /helm/);
  assert.match(applied, /conflict/, "an Apply manager's next helm upgrade stops on the edit");
  assert.match(applied, /Copy as values/);
  const [appliedList] = managerNotes([helm], true);
  assert.doesNotMatch(appliedList, /conflict/, "a set element is no conflict");
  assert.match(appliedList, /back/, "a removed element it still applies comes back");
  const [updated] = managerNotes([kubectl], false);
  assert.match(updated, /kubectl-client-side-apply/);
  assert.doesNotMatch(updated, /conflict/);
  assert.match(updated, /its own value/);
  const [updatedList] = managerNotes([kubectl], true);
  assert.match(updatedList, /whole list/);
  assert.deepEqual(managerNotes([], false), []);

  // A manager under both operations is one manager, Apply.
  assert.deepEqual(uniqueManagers([kubectl, { name: "helm", operation: "Update" }, helm]), [
    helm,
    kubectl,
  ]);
  assert.equal(managerNotes([{ name: "helm", operation: "Update" }, helm], false).length, 1);

  assert.match(loginManagerNote([helm], "remove"), /comes back/);
  assert.match(loginManagerNote([kubectl], "remove"), /whole list/);
  assert.equal(loginManagerNote([], "remove"), null);
  assert.equal(loginManagerNote([helm], "keep"), null);
});

// ---- rows ---------------------------------------------------------------------------------

test("the stage table and the models card never show the check reviewer", () => {
  const stageFields = ["plan", "build", "revise", "evaluate", "checkReview"].flatMap((s) =>
    ["maxTurns", "tokenBudget", "maxCostMicroUSD", "timeout"].map((leaf) =>
      field(`limits.${s}.${leaf}`, "count", { group: "stages" }),
    ),
  );
  const models = ["plan", "build", "evaluate", "checkReview"].map((s) => field(`models.${s}`, "model", { group: "models" }));
  assert.deepEqual(stageRows(stageFields), ["plan", "build", "revise", "evaluate"]);
  assert.deepEqual(modelRows(models), ["plan", "build", "evaluate"]);
  // The demo view is the server's shape.
  const view = mockProjectSettings("storefront");
  assert.deepEqual(stageRows(view.fields), ["plan", "build", "revise", "evaluate"]);
  assert.deepEqual(modelRows(view.fields), ["plan", "build", "evaluate"]);
  assert.equal(JSON.stringify(view).includes("checkReview"), false);
});

// ---- confirmations and values -------------------------------------------------------------

test("confirmationText states the consequence and the open intents", () => {
  const facts = { activeIntents: 3 };
  const c = (key, kind, fieldID, value) => ({ key, kind, field: fieldID, value });
  assert.match(confirmationText(c("evaluator-off", "evaluator-off", "verification.enabled", ""), FIELDS, facts), /3 open intents/);
  assert.match(confirmationText(c("approver-add:carol", "approver-add", "approvers.logins", "carol"), FIELDS, facts), /carol/);
  assert.match(confirmationText(c("approver-remove:bob", "approver-remove", "approvers.logins", "bob"), FIELDS, facts), /bob/);
  assert.match(confirmationText(c("checks-empty", "checks-empty", "checks.fix", ""), FIELDS, facts), /CI check/);
  assert.match(confirmationText(c("handoff:early", "handoff", "verification.handoff", "early"), FIELDS, facts), /before/);
  assert.match(
    confirmationText(c("below-floor:verification.handoffMax=900", "below-floor", "verification.handoffMax", "900"), FIELDS, facts),
    /15m/,
  );
});

test("valuesYAML states what the Project holds, for the maul-config values file", () => {
  let doc = set(settings(), "verification.handoffMax", 5400);
  doc = set(doc, "limits.plan.timeout", 1200);
  doc = set(doc, "models.plan", "anthropic/claude-x");
  const yaml = valuesYAML("storefront", doc);
  assert.match(yaml, /^projects:\n {2}- name: "storefront"\n {4}spec:\n/m);
  assert.match(yaml, /\n {6}approvers:\n {8}logins: \["alice", "bob"\]\n/);
  assert.match(yaml, /\n {8}handoffMax: "1h30m"\n/);
  assert.match(yaml, /\n {8}maxCostMicroUSD: 10000000\n/);
  assert.match(yaml, /\n {8}plan:\n {10}timeout: "20m"\n/);
  assert.match(yaml, /\n {8}plan: "anthropic\/claude-x"\n/);
  assert.match(yaml, /\n {10}agentResourceClass: "small"\n/);
  assert.doesNotMatch(yaml, /maxVerifyFixes|maxToolCalls|handoff: ""|build: ""/, "unset settings are left out");
  // Strings are quoted, so nothing a login or check holds is read as YAML.
  const odd = set(settings(), "checks.fix", ["a: b", 'q"x', "#c"]);
  assert.match(valuesYAML("p", odd), /fix: \["a: b", "q\\"x", "#c"\]/);
});

// ---- demo ---------------------------------------------------------------------------------

test("the demo Project names other field managers under both operations", () => {
  const view = mockProjectSettings("storefront");
  const ops = new Set(view.fields.flatMap((f) => f.managers.map((m) => m.operation)));
  assert.deepEqual([...ops].sort(), ["Apply", "Update"]);
  assert.ok(view.approvers.some((a) => a.managers.length > 0));
  assert.equal(mockProjectSettings("nope"), null);
});

test("the demo save asks for confirmation, refuses, and saves", () => {
  const view = structuredClone(mockProjectSettings("storefront"));
  const req = (s, confirm = [], generation = view.generation) => ({ generation, schema: view.schema, settings: s, confirm });

  const stale = mockSaveProjectSettings("storefront", req(view.settings, [], view.generation - 1));
  assert.equal(stale.status, 409);
  assert.equal(stale.result.reason, "generation-changed");

  const off = set(view.settings, "verification.enabled", false);
  const ask = mockSaveProjectSettings("storefront", req(off));
  assert.equal(ask.status, 409);
  assert.equal(ask.result.reason, "confirm-required");
  assert.deepEqual(ask.result.confirm.map((c) => c.key), ["evaluator-off"]);

  const ceiling = view.fields.find((f) => f.id === "limits.maxRevisions").ceiling;
  const high = set(view.settings, "limits.maxRevisions", ceiling + 1);
  const refused = mockSaveProjectSettings("storefront", req(high));
  assert.equal(refused.status, 422);
  assert.deepEqual(refused.result.refusals.map((r) => [r.field, r.reason]), [["limits.maxRevisions", "above-ceiling"]]);

  const saved = mockSaveProjectSettings("storefront", req(off, ["evaluator-off"]));
  assert.equal(saved.status, 200);
  assert.equal(saved.result.reason, "");
  assert.equal(saved.result.generation, view.generation + 1);
  assert.deepEqual(saved.result.changes.map((c) => c.field), ["verification.enabled"]);
  assert.equal(mockProjectSettings("storefront").settings.verification.enabled, false);
});

test("shownText shows what renders as nothing", () => {
  assert.equal(shownText("test"), "test");
  assert.equal(shownText("te\u202est"), "te[U+202E]st");
  assert.equal(shownText("a\u200bb\nc"), "a[U+200B]b[U+000A]c");
});

// ---- plain text ---------------------------------------------------------------------------

test("the settings page is on the plain-text guard list", () => {
  assert.ok(INTENTS_VIEW_FILES.includes("src/components/ProjectSettingsView.tsx"));
  const src = readFileSync(new URL("../src/components/ProjectSettingsView.tsx", import.meta.url), "utf8");
  assert.doesNotMatch(src, /Markdown|dangerouslySetInnerHTML|innerHTML/);
});
