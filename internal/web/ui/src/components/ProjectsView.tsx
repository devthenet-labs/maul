// The Projects page: the Projects whose settings page the server serves the
// signed-in person, as /api/me lists them, each with its intents tier and a
// link to its settings. A Project /api/me lists without a settings page is
// left out: this page is where settings are found, and the intents board
// already shows every Project. Who may save is the settings page's own
// answer, not this list's.

import type { IntentTier, ProjectAccess } from "../types";
import { hrefForProjectSettings } from "../router";
import { Icon } from "./icons";

const TIER_TEXT: Record<IntentTier, string> = {
  intents: "board and numbers",
  transcripts: "plans and transcripts too",
};

export function ProjectsView({ projects }: { projects: ProjectAccess[] | null }) {
  if (projects === null) return <div class="px-5 py-11 text-center text-muted">Loading…</div>;
  if (projects.length === 0) {
    return (
      <div class="mx-auto my-20 max-w-[460px] rounded-xl border border-line-2 bg-surface p-7 text-center shadow-card">
        <h1 class="mx-0 mt-0 mb-2 text-[19px] tracking-tight">No Project settings to show</h1>
        <p class="m-0 text-muted">
          The server serves no Project's settings page to you: the operator has not turned it on, or no Project you may
          see is one it is on for.
        </p>
      </div>
    );
  }
  return (
    <div class="mx-auto max-w-[760px]">
      <h1 class="mx-0 mt-0 mb-1 text-[19px] tracking-tight">Projects</h1>
      <p class="mx-0 mt-0 mb-4 text-[13px] text-muted">
        Each Project's settings: its limits, models, evaluator and checks. Anyone who may see a Project can read them;
        its approvers can change them.
      </p>
      <ul class="m-0 flex list-none flex-col gap-2.5 p-0">
        {projects.map((p) => (
          <li
            key={p.name}
            class="flex flex-wrap items-center gap-3 rounded-xl border border-line-2 bg-surface px-4.5 py-3 shadow-card"
          >
            <div class="min-w-0 flex-1">
              <div class="truncate font-mono text-[13.5px] font-semibold text-fg">{p.name}</div>
              <div class="font-mono text-[10.5px] text-muted">
                {p.tier} · {TIER_TEXT[p.tier] ?? p.tier}
              </div>
            </div>
            <a class="ps-action no-underline" href={hrefForProjectSettings(p.name)} aria-label={`${p.name} settings`}>
              <Icon name="sliders" size={13} />
              Settings
            </a>
          </li>
        ))}
      </ul>
    </div>
  );
}
