// The Project settings page: an approver of a Project changes its editable
// settings (approvers, evaluator and hand-off, models, spend, rounds, the
// stage limits, CI checks and each repository's machine size) and sees the
// rest read-only. Edits stay in a draft until "Review changes" lists each
// one, old to new, with when it takes effect and what another field
// manager's next apply would do to it; one Save posts the whole document.
// The server judges every save again, asks for confirmation of weakening
// changes and refuses what the operator does not allow. Everything shown is
// plain text: operator- and GitHub-written strings are never markup.

import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useMemo, useRef, useState } from "preact/hooks";
import type {
  ProjectSettings,
  ProjectSettingsConfirmation,
  ProjectSettingsField,
  ProjectSettingsView,
} from "../types";
import { AuthRequiredError, ForbiddenError, fetchProjectSettings, saveProjectSettings } from "../api";
import { signInURL } from "../auth";
import { formatAgo, formatDate } from "../format";
import {
  APPROVERS_FIELD,
  CHECKS_FIELD,
  CLASS_FIELD,
  EFFECTS,
  GATE,
  LOCKS,
  type DraftChange,
  type Moved,
  type Routed,
  WORKFLOWS_FIELD,
  approverProblem,
  classifySave,
  confirmationText,
  delegationNote,
  lockText,
  draftChanges,
  fieldKey,
  fieldLabel,
  fieldValue,
  followView,
  formatValue,
  loginManagerNote,
  managerNotes,
  modelRows,
  parseFieldText,
  rebaseDraft,
  routeRefusals,
  shownText,
  stageRows,
  uniqueManagers,
  valueText,
  valueWords,
  valuesYAML,
  withDraftValue,
  withFieldValue,
} from "../projectSettings";
import { hrefForIntents } from "../router";
import { Combobox } from "./Combobox";
import { Icon } from "./icons";
import { Pill, type Tone } from "./Pills";

type Loaded =
  | { kind: "loading" }
  | { kind: "auth" }
  | { kind: "missing" }
  | { kind: "error"; message: string }
  | { kind: "view"; view: ProjectSettingsView };

interface Note {
  tone: "green" | "red" | "amber";
  text: string;
}

// How often, and how many times, the page re-reads the Project after a save
// until intent-controller has observed it.
const OBSERVE_POLL_MS = 2000;
const OBSERVE_POLLS = 30;

const NO_ROUTES: Routed = { byKey: {}, page: [] };

const INPUT =
  "w-full min-w-0 rounded-lg border border-line-2 bg-transparent px-2.5 py-1.5 font-mono text-[12px] text-fg placeholder:text-faint disabled:opacity-60";

const APPROVER_STATES: Record<string, [Tone, string]> = {
  ok: ["green", "write access"],
  "no-write-access": ["red", "no write access"],
  "no-account": ["red", "no such GitHub account"],
  unresolved: ["amber", "not resolved yet"],
};

function Panel({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <div class="mx-auto my-20 max-w-[460px] rounded-xl border border-line-2 bg-surface p-7 text-center shadow-card">
      <h1 class="mx-0 mt-0 mb-2 text-[19px] tracking-tight">{title}</h1>
      <div class="text-muted">{children}</div>
    </div>
  );
}

function Card({ title, note, children }: { title: string; note?: string; children: ComponentChildren }) {
  return (
    <section class="rounded-xl border border-line-2 bg-surface p-5 shadow-card">
      <h2 class="mx-0 mt-0 mb-1 text-[15px] tracking-tight">{title}</h2>
      {note ? <p class="mt-0 mb-3 text-[12.5px] text-muted">{note}</p> : <div class="mb-3" />}
      <div class="flex flex-col gap-4">{children}</div>
    </section>
  );
}

// Warnings are what another field manager's next apply does to an edit.
function Warnings({ lines }: { lines: string[] }) {
  if (lines.length === 0) return null;
  return (
    <ul class="m-0 flex list-none flex-col gap-1 p-0 text-[11.5px] text-amber">
      {lines.map((l) => (
        <li key={l} class="flex gap-1.5">
          <Icon name="alertTriangle" size={12} class="mt-[2px] flex-none" />
          <span>{l}</span>
        </li>
      ))}
    </ul>
  );
}

function Errors({ lines }: { lines: string[] }) {
  if (lines.length === 0) return null;
  return (
    <ul class="m-0 list-none p-0 text-[12px] text-red">
      {lines.map((l) => (
        <li key={l}>{l}</li>
      ))}
    </ul>
  );
}

// boundsText is a number field's range, default and the operator's and
// controller's bounds, in its own unit.
function boundsText(f: ProjectSettingsField): string {
  const parts: string[] = [];
  const isNum = f.kind === "count" || f.kind === "microUSD" || f.kind === "seconds";
  if (!isNum) return "";
  if (f.default) parts.push(`unset: ${formatValue(f, f.default)}`);
  if (f.ceiling) parts.push(`at most ${formatValue(f, f.ceiling)}`);
  if (f.floor) parts.push(`floor ${formatValue(f, f.floor)}`);
  return parts.join(" · ");
}

interface FieldProps {
  f: ProjectSettingsField;
  item?: string;
  label?: string;
  view: ProjectSettingsView;
  draft: ProjectSettings;
  texts: Record<string, string>;
  errors: string[];
  editable: boolean;
  onText: (key: string, f: ProjectSettingsField, text: string, item: string) => void;
  onValue: (f: ProjectSettingsField, value: string | boolean, item: string) => void;
}

// FieldInput is one setting's control, by kind.
function FieldInput({ f, item = "", label, view, draft, texts, errors, editable, onText, onValue }: FieldProps) {
  const key = fieldKey(f.id, item);
  const value = fieldValue(draft, f.id, item);
  // The label shown may be short (a stage table's "Turns"); the input's
  // accessible name is the field's whole one.
  const full = fieldLabel(f.id, item);
  const name = label ?? full;
  const loaded = fieldValue(view.settings, f.id, item);
  const changed = JSON.stringify(loaded) !== JSON.stringify(value);
  // Workflows need subagents: the switch waits for subagents in the draft.
  const disabled = !editable || f.locked !== "" || (f.id === WORKFLOWS_FIELD && !draft.delegation.subagents);
  const note = delegationNote(view, draft, f.id);

  let control: ComponentChildren;
  switch (f.kind) {
    case "bool":
      control = (
        <label class="inline-flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={value === true}
            disabled={disabled}
            onChange={(e) => onValue(f, (e.target as HTMLInputElement).checked, item)}
          />
          {value === true ? "on" : "off"}
        </label>
      );
      break;
    case "handoff":
    case "model":
    case "class": {
      const current = String(value ?? "");
      let options: { value: string; label: string }[];
      if (f.kind === "handoff") {
        options = [
          { value: "", label: "install default" },
          { value: "gated", label: "gated: after the evaluation" },
          { value: "early", label: "early: before the evaluation" },
        ];
      } else if (f.kind === "model") {
        const stage = f.id.slice("models.".length) as keyof typeof view.options.defaultModels;
        const def = view.options.defaultModels[stage];
        options = [
          { value: "", label: def ? `install default (${def})` : "install default" },
          ...view.options.models.map((m) => ({ value: m, label: m })),
        ];
      } else {
        options = [
          { value: "", label: "cluster default" },
          ...view.options.resourceClasses.map((c) => ({
            value: c.name,
            label: `${c.name} · ${c.requestCPU} CPU, ${c.requestMemory} (up to ${c.limitCPU}, ${c.limitMemory})`,
          })),
        ];
      }
      if (current !== "" && !options.some((o) => o.value === current)) {
        options.push({ value: current, label: `${current} (not offered)` });
      }
      control = disabled ? (
        <span class="font-mono text-[12px]">{options.find((o) => o.value === current)?.label ?? current}</span>
      ) : (
        <Combobox value={current} options={options} ariaLabel={full} onChange={(v) => onValue(f, v, item)} />
      );
      break;
    }
    default: {
      const placeholder = f.default ? formatValue(f, f.default).replace(/^\$/, "") : "unset";
      control = (
        <input
          type="text"
          inputMode={f.kind === "count" ? "numeric" : "text"}
          aria-label={full}
          class={INPUT}
          placeholder={placeholder}
          value={texts[key] ?? valueText(f, value)}
          disabled={disabled}
          onInput={(e) => onText(key, f, (e.target as HTMLInputElement).value, item)}
        />
      );
    }
  }

  const bounds = boundsText(f);
  return (
    <div class="flex flex-col gap-1">
      <div class="flex flex-wrap items-baseline justify-between gap-x-3">
        <span class="text-[13px] font-semibold">
          {name}
          {changed ? <span class="ml-1.5 text-amber" title="changed, not saved">●</span> : null}
        </span>
        {f.kind === "microUSD" ? <span class="text-[11px] text-faint">dollars</span> : null}
        {f.kind === "seconds" ? <span class="text-[11px] text-faint">e.g. 45m, 1h30m</span> : null}
      </div>
      {control}
      <span class="text-[11px] text-faint">
        {bounds ? `${bounds} · ` : ""}
        {EFFECTS[f.effect] ?? f.effect}
      </span>
      {f.locked ? <span class="text-[11.5px] text-muted">{lockText(f.locked, view.facts.harness)}</span> : null}
      {note && !f.locked ? <span class="text-[11.5px] text-muted">{note}</span> : null}
      <Warnings lines={managerNotes(item ? classManagers(view, item) : f.managers, false)} />
      <Errors lines={errors} />
    </div>
  );
}

function classManagers(view: ProjectSettingsView, repo: string) {
  return view.facts.repositories.find((r) => r.name === repo)?.classManagers ?? [];
}

// textsOf are the number inputs' texts for a document.
function textsOf(fields: ProjectSettingsField[], doc: ProjectSettings): Record<string, string> {
  const out: Record<string, string> = {};
  for (const f of fields) {
    if (f.kind === "count" || f.kind === "microUSD" || f.kind === "seconds") {
      out[f.id] = valueText(f, fieldValue(doc, f.id));
    }
  }
  return out;
}

function ApproversCard({
  view,
  draft,
  editable,
  errors,
  onChange,
}: {
  view: ProjectSettingsView;
  draft: ProjectSettings;
  editable: boolean;
  errors: string[];
  onChange: (logins: string[]) => void;
}) {
  const f = view.fields.find((x) => x.id === APPROVERS_FIELD);
  const [login, setLogin] = useState("");
  if (!f) return null;
  const you = view.approvers.find((a) => a.you)?.login ?? "";
  const loaded = view.settings.approvers;
  const canChange = editable && f.locked === "";
  const pending = loaded.filter((l) => !draft.approvers.includes(l)).length + draft.approvers.filter((l) => !loaded.includes(l)).length;
  const problem = approverProblem(loaded, draft.approvers, you);
  const candidate = login.trim();
  const addProblem = candidate ? approverProblem(loaded, [...draft.approvers, candidate], you) : null;
  const rows = [...loaded, ...draft.approvers.filter((l) => !loaded.includes(l))];

  return (
    <Card
      title="Approvers"
      note={
        "The only GitHub accounts whose approvals and /maul commands count, and who may change these settings. " +
        "One login is added or removed per save."
      }
    >
      <ul class="m-0 flex list-none flex-col gap-2 p-0">
        {rows.map((l) => {
          const a = view.approvers.find((x) => x.login === l);
          const removed = !draft.approvers.includes(l);
          const added = !loaded.includes(l);
          const [tone, words] = a ? (APPROVER_STATES[a.state] ?? ["neutral", a.state]) : ["seedling", "new"];
          const managers = uniqueManagers(a?.managers ?? []);
          const note = loginManagerNote(a?.managers ?? [], removed ? "remove" : "keep");
          return (
            <li key={l} class="flex flex-col gap-1">
              <div class="flex flex-wrap items-center gap-2">
                <span class={`font-mono text-[13px] ${removed ? "text-faint line-through" : ""}`}>{l}</span>
                <Pill tone={tone as Tone}>{words}</Pill>
                {a?.you ? <span class="ps-chip">you</span> : null}
                {managers.map((m) => (
                  <span key={m.name} class="ps-chip" title="another field manager that applies this login">
                    {m.name} · {m.operation}
                  </span>
                ))}
                {canChange && !a?.you ? (
                  removed || added ? (
                    <button
                      type="button"
                      class="ps-action ml-auto min-h-7 px-2.5 text-[12px]"
                      onClick={() =>
                        onChange(added ? draft.approvers.filter((x) => x !== l) : [...draft.approvers, l])
                      }
                    >
                      Undo
                    </button>
                  ) : (
                    <button
                      type="button"
                      class="ps-action ml-auto min-h-7 px-2.5 text-[12px]"
                      disabled={pending > 0}
                      title={pending > 0 ? "One approver change per save" : `Remove ${l}`}
                      onClick={() => onChange(draft.approvers.filter((x) => x !== l))}
                    >
                      Remove
                    </button>
                  )
                ) : null}
              </div>
              {note ? <Warnings lines={[note]} /> : null}
            </li>
          );
        })}
      </ul>
      {canChange ? (
        <form
          class="flex flex-wrap items-start gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (!candidate || addProblem) return;
            onChange([...draft.approvers, candidate]);
            setLogin("");
          }}
        >
          <input
            type="text"
            class={`${INPUT} max-w-[260px]`}
            aria-label="GitHub login to add"
            placeholder="GitHub login"
            spellcheck={false}
            autocomplete="off"
            value={login}
            disabled={pending > 0}
            onInput={(e) => setLogin((e.target as HTMLInputElement).value)}
          />
          <button type="submit" class="ps-action" disabled={pending > 0 || !candidate || addProblem !== null}>
            Add
          </button>
          {addProblem ? <span class="w-full text-[12px] text-red">{addProblem}</span> : null}
        </form>
      ) : null}
      <span class="text-[11px] text-faint">
        {view.approversResolvedAt
          ? `GitHub last asked ${formatAgo(view.approversResolvedAt)}`
          : "intent-controller has not asked GitHub about these logins yet"}
        {` · ${EFFECTS[f.effect] ?? f.effect}`}
      </span>
      {view.approversStale ? (
        <p class="ps-note m-0">The approvers were last checked with GitHub over 30 minutes ago.</p>
      ) : null}
      {f.locked ? <span class="text-[11.5px] text-muted">{LOCKS[f.locked] ?? f.locked}</span> : null}
      <Warnings lines={managerNotes(f.managers, true)} />
      <Errors lines={[...(problem ? [problem] : []), ...errors]} />
    </Card>
  );
}

function ChecksList({
  view,
  draft,
  editable,
  errors,
  onChange,
}: {
  view: ProjectSettingsView;
  draft: ProjectSettings;
  editable: boolean;
  errors: string[];
  onChange: (fix: string[]) => void;
}) {
  const f = view.fields.find((x) => x.id === CHECKS_FIELD);
  const [name, setName] = useState("");
  if (!f) return null;
  const canChange = editable && f.locked === "";
  const fix = draft.checks.fix;
  const candidate = name.trim();
  const full = f.max > 0 && fix.length >= f.max;
  return (
    <div class="flex flex-col gap-1.5">
      <span class="text-[13px] font-semibold">{fieldLabel(CHECKS_FIELD)}</span>
      <span class="text-[12px] text-muted">
        The CI checks maul waits for and fixes on an intent's pull request. None: maul fixes no check and waits for
        none before the hand-off.
      </span>
      <ul class="m-0 flex list-none flex-wrap gap-1.5 p-0">
        {fix.length === 0 ? <li class="text-[12px] text-faint">none</li> : null}
        {fix.map((c) => (
          <li key={c} class="ps-chip">
            <span class="text-fg">{shownText(c)}</span>
            {canChange ? (
              <button
                type="button"
                class="cursor-pointer border-0 bg-transparent p-0 text-muted"
                aria-label={`Remove check ${shownText(c)}`}
                onClick={() => onChange(fix.filter((x) => x !== c))}
              >
                <Icon name="x" size={11} />
              </button>
            ) : null}
          </li>
        ))}
      </ul>
      {canChange ? (
        <form
          class="flex flex-wrap gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (!candidate || fix.includes(candidate) || full) return;
            onChange([...fix, candidate]);
            setName("");
          }}
        >
          <input
            type="text"
            class={`${INPUT} max-w-[260px]`}
            aria-label="Check name to add"
            placeholder="check name"
            spellcheck={false}
            autocomplete="off"
            maxLength={128}
            value={name}
            onInput={(e) => setName((e.target as HTMLInputElement).value)}
          />
          <button type="submit" class="ps-action" disabled={!candidate || fix.includes(candidate) || full}>
            Add
          </button>
        </form>
      ) : null}
      <span class="text-[11px] text-faint">{EFFECTS[f.effect] ?? f.effect}</span>
      {f.locked ? <span class="text-[11.5px] text-muted">{LOCKS[f.locked] ?? f.locked}</span> : null}
      <Warnings lines={managerNotes(f.managers, true)} />
      <Errors lines={errors} />
    </div>
  );
}

// ReviewList is the draft's changes, old to new, with when each takes
// effect and what another manager's next apply does to it.
function ReviewList({ view, changes }: { view: ProjectSettingsView; changes: DraftChange[] }) {
  return (
    <ul class="m-0 flex list-none flex-col gap-3 p-0">
      {changes.map((c) => {
        const f = view.fields.find((x) => x.id === c.field);
        const list = f?.kind === "logins" || f?.kind === "checks";
        const warnings =
          c.field === CLASS_FIELD
            ? managerNotes(classManagers(view, c.item), false)
            : f
              ? managerNotes(f.managers, list)
              : [];
        if (c.field === APPROVERS_FIELD) {
          for (const l of c.removed) {
            const note = loginManagerNote(view.approvers.find((a) => a.login === l)?.managers ?? [], "remove");
            if (note) warnings.push(note);
          }
        }
        return (
          <li key={c.key} class="flex flex-col gap-1 border-b border-line pb-3 last:border-b-0 last:pb-0">
            <span class="text-[13px] font-semibold">{fieldLabel(c.field, c.item)}</span>
            {list ? (
              <span class="font-mono text-[12px]">
                {c.added.length ? `+ ${c.added.map(shownText).join(", ")}` : ""}
                {c.added.length && c.removed.length ? "   " : ""}
                {c.removed.length ? `− ${c.removed.map(shownText).join(", ")}` : ""}
              </span>
            ) : (
              <span class="font-mono text-[12px]">
                {valueWords(f, c.from)} → {valueWords(f, c.to)}
              </span>
            )}
            {f ? <span class="text-[11px] text-faint">{EFFECTS[f.effect] ?? f.effect}</span> : null}
            <Warnings lines={warnings} />
          </li>
        );
      })}
    </ul>
  );
}

export function ProjectSettingsPage({ name }: { name: string }) {
  const [loaded, setLoaded] = useState<Loaded>({ kind: "loading" });
  const [draft, setDraft] = useState<ProjectSettings | null>(null);
  const [texts, setTexts] = useState<Record<string, string>>({});
  const [inputErrors, setInputErrors] = useState<Record<string, string>>({});
  const [routed, setRouted] = useState<Routed>(NO_ROUTES);
  const [notes, setNotes] = useState<Note[]>([]);
  const [moved, setMoved] = useState<Moved[]>([]);
  const [reviewing, setReviewing] = useState(false);
  const [confirm, setConfirm] = useState<ProjectSettingsConfirmation[]>([]);
  const [armed, setArmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [waitFor, setWaitFor] = useState(0);
  const [polls, setPolls] = useState(0);
  const [showValues, setShowValues] = useState(false);

  const view = loaded.kind === "view" ? loaded.view : null;
  // The draft as last rendered, for the observe poll, which must not
  // restart its timer on every keystroke.
  const draftRef = useRef(draft);
  draftRef.current = draft;
  // The view that draft is built on, read when a poll answers: a save may
  // have reset both while the poll was in flight.
  const viewRef = useRef(view);
  viewRef.current = view;

  const reset = useCallback((v: ProjectSettingsView, doc: ProjectSettings) => {
    setDraft(doc);
    setTexts(textsOf(v.fields, doc));
    setInputErrors({});
  }, []);

  // fetchView re-reads the Project. follow, when given, runs in the same
  // tick as the view is replaced, so the draft is never rendered against
  // settings it was not built on; it returns false to keep the view (a
  // re-read older than the one the draft is built on).
  const fetchView = useCallback(async (follow?: (v: ProjectSettingsView) => boolean): Promise<ProjectSettingsView | null> => {
    try {
      const v = await fetchProjectSettings(name);
      if (!v) {
        setLoaded({ kind: "missing" });
        return null;
      }
      if (follow && !follow(v)) return v;
      setLoaded({ kind: "view", view: v });
      return v;
    } catch (e) {
      if (e instanceof AuthRequiredError) setLoaded({ kind: "auth" });
      else setLoaded({ kind: "error", message: e instanceof Error ? e.message : String(e) });
      return null;
    }
  }, [name]);

  useEffect(() => {
    setLoaded({ kind: "loading" });
    setNotes([]);
    setRouted(NO_ROUTES);
    setMoved([]);
    setReviewing(false);
    setConfirm([]);
    setWaitFor(0);
    void fetchView().then((v) => {
      if (v) reset(v, structuredClone(v.settings));
    });
  }, [fetchView, reset]);

  // After a save, re-read until intent-controller has observed it. A
  // re-read showing someone else's save moves the draft onto it
  // (followView), so the draft, the changes listed and the generation a
  // save posts stay those of one snapshot.
  useEffect(() => {
    if (!view || waitFor === 0) return;
    if (view.observedGeneration >= waitFor) {
      setWaitFor(0);
      return;
    }
    if (polls >= OBSERVE_POLLS) return;
    const t = setTimeout(() => {
      setPolls((p) => p + 1);
      void fetchView((fresh) => {
        const base = viewRef.current;
        const d = draftRef.current;
        if (!base || !d) return true;
        if (fresh.generation < base.generation) return false;
        const f = followView(base, d, fresh);
        if (!f.rebased) return true;
        reset(fresh, f.draft);
        setMoved(f.moved);
        setReviewing(false);
        setConfirm([]);
        setArmed(false);
        const lines = [
          "Someone else saved this Project after you. Their change is shown now, and any edits you had not saved are applied to it: review them before saving.",
          ...f.dropped.map((k) => `${k}: that repository is gone, so the edit was dropped.`),
        ];
        setNotes(lines.map((text) => ({ tone: "amber", text })));
        return true;
      });
    }, OBSERVE_POLL_MS);
    return () => clearTimeout(t);
  }, [view, waitFor, polls, fetchView, reset]);

  const changes = useMemo(
    () => (view && draft ? draftChanges(view.settings, draft, view.fields) : []),
    [view, draft],
  );

  if (loaded.kind === "loading" || (view && !draft)) {
    return <div class="px-5 py-11 text-center text-muted">Loading…</div>;
  }
  if (loaded.kind === "auth") {
    return (
      <Panel title="Authentication required">
        <p class="mt-0">Sign in to see this Project's settings.</p>
        <a class="ps-action ps-action--primary no-underline" href={signInURL()}>
          Sign in
        </a>
      </Panel>
    );
  }
  if (loaded.kind === "missing") {
    return (
      <Panel title="Not found">
        <p class="mt-0">No Project by that name has a settings page you can see.</p>
        <a href={hrefForIntents()}>Back to the board</a>
      </Panel>
    );
  }
  if (loaded.kind === "error") return <Panel title="Cannot load the settings">{loaded.message}</Panel>;
  if (!view || !draft) return null;

  const editable = view.canEdit;
  const you = view.approvers.find((a) => a.you)?.login ?? "";
  const blocked =
    Object.keys(inputErrors).length > 0 || approverProblem(view.settings.approvers, draft.approvers, you) !== null;
  const errorsFor = (key: string) => [
    ...(inputErrors[key] ? [inputErrors[key]] : []),
    ...(routed.byKey[key] ?? []),
  ];
  const byID = (id: string) => view.fields.find((f) => f.id === id);

  const edited = () => {
    setReviewing(false);
    setConfirm([]);
    setArmed(false);
  };
  const onText = (key: string, f: ProjectSettingsField, text: string, item: string) => {
    setTexts((t) => ({ ...t, [key]: text }));
    const parsed = parseFieldText(f, text);
    if ("error" in parsed) {
      setInputErrors((e) => ({ ...e, [key]: parsed.error }));
      return;
    }
    setInputErrors((e) => {
      const rest = { ...e };
      delete rest[key];
      return rest;
    });
    setDraft((d) => (d ? withFieldValue(d, f.id, parsed.value, item) : d));
    edited();
  };
  const onValue = (f: ProjectSettingsField, value: string | boolean, item: string) => {
    setDraft((d) => (d ? withDraftValue(d, f.id, value, item) : d));
    edited();
  };
  const props = { view, draft, texts, editable, onText, onValue };
  const input = (id: string, item = "", label?: string) => {
    const f = byID(id);
    return f ? (
      <FieldInput key={fieldKey(id, item)} f={f} item={item} label={label} errors={errorsFor(fieldKey(id, item))} {...props} />
    ) : null;
  };

  const discard = () => {
    reset(view, structuredClone(view.settings));
    setRouted(NO_ROUTES);
    setMoved([]);
    setNotes([]);
    edited();
  };

  const save = async (confirmed: string[]) => {
    setBusy(true);
    setNotes([]);
    setRouted(NO_ROUTES);
    try {
      const answer = await saveProjectSettings(name, {
        generation: view.generation,
        schema: view.schema,
        settings: draft,
        confirm: confirmed,
      });
      const outcome = classifySave(answer);
      switch (outcome.kind) {
        case "saved": {
          const n = outcome.result.changes.length;
          const fresh = await fetchView();
          if (fresh) reset(fresh, structuredClone(fresh.settings));
          setReviewing(false);
          setConfirm([]);
          setArmed(false);
          setMoved([]);
          setNotes([{ tone: "green", text: n ? `Saved ${n === 1 ? "1 change" : `${n} changes`}.` : "Nothing changed." }]);
          if (n) {
            setPolls(0);
            setWaitFor(outcome.result.generation);
          }
          break;
        }
        case "stale": {
          const fresh = await fetchView();
          if (fresh) {
            const r = rebaseDraft(view.settings, draft, fresh.settings, fresh.fields);
            reset(fresh, r.draft);
            setMoved(r.moved);
            const lines = [
              "Someone changed this Project since you loaded it. Your edits are applied to its current settings: review them and save again.",
              ...r.dropped.map((k) => `${k}: that repository is gone, so the edit was dropped.`),
            ];
            setNotes(lines.map((text) => ({ tone: "amber", text })));
          }
          setReviewing(false);
          setConfirm([]);
          setArmed(false);
          break;
        }
        case "reload":
          setNotes([{ tone: "red", text: "maul was updated since this page loaded. Reload the page, then make your edits again." }]);
          break;
        case "confirm":
          setConfirm(outcome.result.confirm);
          setArmed(false);
          break;
        case "retry":
          setNotes([{ tone: "red", text: "The Project kept changing while saving. Save again." }]);
          break;
        case "refused": {
          const r = routeRefusals(outcome.result.refusals, view.fields);
          setRouted(r);
          setReviewing(false);
          setConfirm([]);
          setNotes([
            { tone: "red", text: "Not saved: see the settings marked below." },
            ...r.page.map((text) => ({ tone: "red" as const, text })),
          ]);
          break;
        }
        case "denied":
          setNotes([{ tone: "red", text: `Not saved. ${outcome.message}` }]);
          break;
        case "missing":
          setLoaded({ kind: "missing" });
          break;
        case "error":
          setNotes([{ tone: "red", text: `Not saved. ${outcome.message}` }]);
          break;
      }
    } catch (e) {
      if (e instanceof AuthRequiredError) {
        setNotes([{ tone: "red", text: "Your session ended: sign in again. Your edits are still here." }]);
      } else {
        const msg = e instanceof ForbiddenError || e instanceof Error ? e.message : String(e);
        setNotes([{ tone: "red", text: `Not saved. ${msg}` }]);
      }
    } finally {
      setBusy(false);
    }
  };

  const stages = stageRows(view.fields);
  const models = modelRows(view.fields);
  const facts = view.facts;
  const pendingObserve = waitFor > 0 && view.observedGeneration < waitFor;
  const ready = view.conditions.find((c) => c.type === "Ready");

  return (
    <div class="mx-auto flex max-w-[880px] flex-col gap-5 pb-24">
      <a href={hrefForIntents()} class="inline-flex items-center gap-1.5 text-[12px] text-muted no-underline">
        <Icon name="arrowLeft" size={13} /> Intents
      </a>
      <header class="flex flex-col gap-2">
        <div class="flex flex-wrap items-center gap-3">
          <h1 class="m-0 text-[21px] tracking-tight">
            Settings · <span class="font-mono">{view.project}</span>
          </h1>
          <span class="ps-chip" title="metadata.generation">
            generation {view.generation}
          </span>
          {ready ? <Pill tone={ready.status === "True" ? "green" : "red"}>{ready.status === "True" ? "ready" : "not ready"}</Pill> : null}
          <button type="button" class="ps-action ml-auto" onClick={() => setShowValues(!showValues)}>
            <Icon name="copy" size={14} /> Copy as values
          </button>
        </div>
        {view.lastEdit ? (
          <p class="m-0 text-[12.5px] text-muted" title={formatDate(view.lastEdit.at)}>
            Last changed here by <span class="font-mono">{view.lastEdit.login || `GitHub id ${view.lastEdit.githubID}`}</span>{" "}
            {formatAgo(view.lastEdit.at)}: {view.lastEdit.fields.map((id) => fieldLabel(id)).join(", ")}
            {view.lastEdit.more ? ` and ${view.lastEdit.more} more` : ""}.
          </p>
        ) : null}
        <p class="m-0 text-[12.5px] text-muted">
          A setting another tool also applies (Helm, kubectl) says so below, with what that tool's next apply does to an
          edit made here. Set each setting from one place.
        </p>
        {view.conditions.length ? (
          <ul class="m-0 flex list-none flex-col gap-0.5 p-0 text-[12px] text-muted">
            {view.conditions.map((c) => (
              <li key={c.type}>
                <span class={c.status === "True" && c.type !== "VerificationOff" ? "text-ink" : "text-amber"}>●</span> {c.text}
              </li>
            ))}
          </ul>
        ) : null}
        {!editable ? <p class="ps-note m-0">{GATE[view.reason] ?? "You may not change these settings."} The settings below are read-only for you.</p> : null}
        {showValues ? (
          <div class="flex flex-col gap-2 rounded-xl border border-line-2 bg-surface p-4">
            <p class="m-0 text-[12.5px] text-muted">
              The settings as the Project holds them now, for its entry in the maul-config values file, so that file's
              next apply neither fails on them nor puts other values back.
            </p>
            <pre class="m-0 max-h-80 overflow-auto rounded-lg bg-code-2 p-3 font-mono text-[11.5px]">{valuesYAML(view.project, view.settings)}</pre>
            <div>
              <button
                type="button"
                class="ps-action"
                onClick={() => void navigator.clipboard?.writeText(valuesYAML(view.project, view.settings))}
              >
                <Icon name="copy" size={14} /> Copy
              </button>
            </div>
          </div>
        ) : null}
      </header>

      {notes.map((n) => (
        <p key={n.text} class={`m-0 ${n.tone === "green" ? "ps-note ps-note--green" : n.tone === "red" ? "ps-note ps-note--red" : "ps-note"}`} role="status">
          {n.text}
        </p>
      ))}
      {pendingObserve ? (
        <p class="ps-note m-0" role="status">
          {polls >= OBSERVE_POLLS
            ? `intent-controller has not yet applied generation ${waitFor}. Reload later to see its conditions.`
            : `Waiting for intent-controller to apply generation ${waitFor}…`}
        </p>
      ) : null}
      {moved.length ? (
        <div class="ps-note m-0 flex-col items-start">
          <span>Also changed by someone else meanwhile; your value is kept, check it:</span>
          <ul class="m-0 pl-5">
            {moved.map((m) => {
              const f = byID(m.field);
              return (
                <li key={m.key}>
                  {fieldLabel(m.field, m.item)}: was {valueWords(f, m.was)}, now {valueWords(f, m.now)}, yours{" "}
                  {valueWords(f, m.yours)}
                </li>
              );
            })}
          </ul>
        </div>
      ) : null}

      <ApproversCard
        view={view}
        draft={draft}
        editable={editable}
        errors={errorsFor(APPROVERS_FIELD)}
        onChange={(logins) => {
          setDraft((d) => (d ? withFieldValue(d, APPROVERS_FIELD, logins) : d));
          edited();
        }}
      />

      <Card
        title="Evaluator and hand-off"
        note={view.options.evaluator ? undefined : "The cluster runs without the agent evaluator: these settings do nothing until it does."}
      >
        <div class="grid gap-4 sm:grid-cols-2">
          {input("verification.enabled")}
          {input("verification.maxToolCalls")}
          {input("verification.handoff")}
          {input("verification.handoffMax")}
        </div>
      </Card>

      <Card title="Models" note="Each stage's model, among those intent-controller admits for this Project.">
        <div class="grid gap-4 sm:grid-cols-3">{models.map((m) => input(`models.${m}`))}</div>
      </Card>

      <Card title="Spend" note="What one intent may cost in model spend, all its runs together.">
        <div class="grid gap-4 sm:grid-cols-2">
          {input("limits.maxCostMicroUSD")}
          {input("limits.maxEvaluateCostMicroUSD")}
        </div>
      </Card>

      <Card title="Rounds" note="How many times maul tries again for one intent, and how many intents run at once.">
        <div class="grid gap-4 sm:grid-cols-3">
          {input("limits.maxRevisions")}
          {input("limits.maxCheckFixes")}
          {input("limits.maxVerifyFixes")}
          {input("limits.maxVerifications")}
          {input("limits.maxActiveIntents")}
        </div>
      </Card>

      <Card title="Stage limits" note="Per run of each stage. Unset is intent-controller's own ceiling, shown as the placeholder.">
        {stages.map((s) => (
          <div key={s} class="flex flex-col gap-2">
            <span class="ps-heading">{fieldLabel(`limits.${s}.maxTurns`).replace(/ turns$/, "")}</span>
            <div class="grid gap-4 sm:grid-cols-4">
              {input(`limits.${s}.maxTurns`, "", "Turns")}
              {input(`limits.${s}.tokenBudget`, "", "Tokens")}
              {input(`limits.${s}.maxCostMicroUSD`, "", "Cost")}
              {input(`limits.${s}.timeout`, "", "Time limit")}
            </div>
          </div>
        ))}
      </Card>

      <Card
        title="Delegation"
        note="Lets new build and revise runs hand work to subagents and, beside them, workflows. Each subagent spends from the run's own cost budget."
      >
        <div class="grid gap-4 sm:grid-cols-2">
          {input("delegation.subagents")}
          {input("delegation.workflows")}
        </div>
      </Card>

      <Card title="CI checks">
        <ChecksList
          view={view}
          draft={draft}
          editable={editable}
          errors={errorsFor(CHECKS_FIELD)}
          onChange={(fix) => {
            setDraft((d) => (d ? withFieldValue(d, CHECKS_FIELD, fix) : d));
            edited();
          }}
        />
        <div class="grid gap-4 sm:grid-cols-2">
          {input("checks.timeout")}
          {input("checks.rerunFailed")}
        </div>
      </Card>

      <Card title="Machine size" note="The resources each repository's agent runs get.">
        {draft.repositories.map((r) => input(CLASS_FIELD, r.name, `${r.name}`))}
      </Card>

      <Card title="Set by the operator" note="Read-only here: the operator sets these in the Project itself.">
        <dl class="ps-kv">
          <div>
            <dt>Intent repository</dt>
            <dd class="break-all">{facts.intentRepository}</dd>
          </div>
          <div>
            <dt>Labels</dt>
            <dd>
              trigger <span class="font-mono">{facts.triggerLabel}</span>, approve <span class="font-mono">{facts.approveLabel}</span>
            </dd>
          </div>
          <div>
            <dt>Harness</dt>
            <dd>{facts.harness}</dd>
          </div>
          <div>
            <dt>Surfaces</dt>
            <dd>{facts.surfaces.length ? facts.surfaces.join(", ") : "none"}</dd>
          </div>
          <div>
            <dt>Suspended</dt>
            <dd>{facts.suspend ? "yes" : "no"}</dd>
          </div>
          <div>
            <dt>Repository image required</dt>
            <dd>{facts.requireRepositoryImage ? "yes" : "no"}</dd>
          </div>
          <div>
            <dt>Manual gates status</dt>
            <dd>{facts.manualGatesStatus ? "on" : "off"}</dd>
          </div>
          <div>
            <dt>Post log tails</dt>
            <dd>{facts.postLogs ? "on" : "off"}</dd>
          </div>
          <div>
            <dt>Open intents</dt>
            <dd>{facts.activeIntents}</dd>
          </div>
        </dl>
        <ul class="m-0 flex list-none flex-col gap-2 p-0 text-[12.5px]">
          {facts.repositories.map((r) => (
            <li key={r.name}>
              <span class="font-mono font-semibold">{r.name}</span> <span class="break-all text-muted">{r.url}</span>
              {r.previewed ? (
                <span class="block text-[11.5px] text-faint">
                  preview at {r.previewPath} · {r.previewImage}:{r.previewPort} · ready at {r.previewReadinessPath}
                </span>
              ) : null}
            </li>
          ))}
        </ul>
      </Card>

      {editable && (changes.length > 0 || reviewing) ? (
        <div class="sticky bottom-4 z-20 flex flex-col gap-3 rounded-xl border border-line-2 bg-surface p-4 shadow-strong">
          {reviewing ? (
            <>
              <h2 class="m-0 text-[15px] tracking-tight">Review changes</h2>
              <ReviewList view={view} changes={changes} />
              {confirm.length ? (
                <div class="ps-note m-0 flex-col items-start">
                  <span class="font-semibold">Confirm what these changes do:</span>
                  <ul class="m-0 pl-5">
                    {confirm.map((c) => (
                      <li key={c.key}>{confirmationText(c, view.fields, facts)}</li>
                    ))}
                  </ul>
                </div>
              ) : null}
            </>
          ) : null}
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-[12.5px] text-muted">
              {changes.length === 1 ? "1 unsaved change" : `${changes.length} unsaved changes`}
            </span>
            <span class="ml-auto" />
            {reviewing ? (
              confirm.length ? (
                <button
                  type="button"
                  class={`ps-action ${armed ? "is-confirming" : "ps-action--primary"}`}
                  disabled={busy}
                  onClick={() => (armed ? void save(confirm.map((c) => c.key)) : setArmed(true))}
                >
                  {busy ? "Saving…" : armed ? "Click again to confirm and save" : "Confirm and save"}
                </button>
              ) : (
                <button
                  type="button"
                  class="ps-action ps-action--primary"
                  disabled={busy || changes.length === 0}
                  onClick={() => void save([])}
                >
                  {busy ? "Saving…" : "Save"}
                </button>
              )
            ) : (
              <button
                type="button"
                class="ps-action ps-action--primary"
                disabled={blocked || changes.length === 0}
                onClick={() => setReviewing(true)}
              >
                Review changes
              </button>
            )}
            {reviewing ? (
              <button type="button" class="ps-action" disabled={busy} onClick={edited}>
                Keep editing
              </button>
            ) : (
              <button type="button" class="ps-action" onClick={discard}>
                Discard
              </button>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}
