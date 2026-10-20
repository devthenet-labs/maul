// The Account page (per-user credentials design L3, "Dashboard"): the
// signed-in person sets, replaces or deletes their own model credential and
// sees its fingerprint, health and when it was last used. Write-only: the
// credential is typed into a password field, sent once in a same-origin
// POST, and never shown again; nothing the server answers carries it. It
// gates on its own answers, never on the findings grant.

import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useState } from "preact/hooks";
import type { Account, AccountCredential, CredentialKind } from "../types";
import { AuthRequiredError, ForbiddenError, deleteCredential, fetchAccount, putCredential } from "../api";
import { signInURL } from "../auth";
import {
  credentialProblem,
  guessKind,
  healthOf,
  healthText,
  kindLabel,
  offered,
  placeholder,
  providerLabel,
} from "../account";
import { formatAgo, formatDate } from "../format";

type Loaded =
  | { kind: "loading" }
  | { kind: "auth" }
  | { kind: "error"; message: string }
  | { kind: "account"; account: Account };

function Panel({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <div class="mx-auto my-20 max-w-[460px] rounded-xl border border-line-2 bg-surface p-7 text-center shadow-card">
      <h1 class="mx-0 mt-0 mb-2 text-[19px] tracking-tight">{title}</h1>
      <div class="text-muted">{children}</div>
    </div>
  );
}

const HEALTH_CLASS: Record<string, string> = {
  Valid: "ps-note ps-note--green",
  Rejected: "ps-note ps-note--red",
  Unknown: "ps-note",
};

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// CredentialCard is one provider's credential: what is stored, and the form
// that sets, replaces or deletes it.
function CredentialCard({
  credential,
  onChanged,
}: {
  credential: AccountCredential;
  onChanged: (c: AccountCredential) => void;
}) {
  const takes = offered(credential);
  const kinds: CredentialKind[] = takes ? credential.kinds : ["api-key"];
  const [kind, setKind] = useState<CredentialKind>(
    credential.kind !== "" && kinds.includes(credential.kind) ? credential.kind : kinds[0],
  );
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const problem = value === "" ? null : credentialProblem(credential.provider, kinds, value, kind);
  const provider = providerLabel(credential.provider);
  const health = healthOf(credential);

  const save = async (e: Event) => {
    e.preventDefault();
    const p = credentialProblem(credential.provider, kinds, value, kind);
    if (p) {
      setError(p);
      return;
    }
    setBusy(true);
    setError(null);
    setSaved(null);
    try {
      const stored = await putCredential(credential.provider, kind, value);
      setValue("");
      setSaved(credential.stored ? "Replaced." : "Stored.");
      onChanged(stored);
    } catch (err) {
      setError(err instanceof AuthRequiredError ? "Your session ended: sign in again." : messageOf(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirmDelete) {
      setConfirmDelete(true);
      return;
    }
    setConfirmDelete(false);
    setBusy(true);
    setError(null);
    setSaved(null);
    try {
      await deleteCredential(credential.provider);
      setSaved("Deleted.");
      onChanged({
        provider: credential.provider,
        kinds: credential.kinds,
        stored: false,
        kind: "",
        fingerprint: "",
        health: "",
        lastUsedAt: "",
        lastRejectedAt: "",
      });
    } catch (err) {
      setError(err instanceof AuthRequiredError ? "Your session ended: sign in again." : messageOf(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section class="rounded-xl border border-line-2 bg-surface p-5 shadow-card">
      <h2 class="mx-0 mt-0 mb-3 text-[15px] tracking-tight">{provider}</h2>
      {credential.stored ? (
        <dl class="m-0 mb-4 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-[13px]">
          <dt class="text-muted">Kind</dt>
          <dd class="m-0">{credential.kind ? kindLabel(credential.provider, credential.kind) : "—"}</dd>
          <dt class="text-muted">Fingerprint</dt>
          <dd class="m-0 font-mono text-[12px]">{credential.fingerprint || "—"}</dd>
          <dt class="text-muted">Health</dt>
          <dd class="m-0">
            <span class={`${HEALTH_CLASS[health]} m-0 inline-block px-2 py-0.5`}>{health}</span>{" "}
            <span class="text-muted">{healthText(credential.provider, health)}</span>
          </dd>
          <dt class="text-muted">Last used</dt>
          <dd class="m-0" title={formatDate(credential.lastUsedAt)}>
            {credential.lastUsedAt ? formatAgo(credential.lastUsedAt) : "never"}
          </dd>
          {credential.lastRejectedAt ? (
            <>
              <dt class="text-muted">Last rejected</dt>
              <dd class="m-0" title={formatDate(credential.lastRejectedAt)}>
                {formatAgo(credential.lastRejectedAt)}
              </dd>
            </>
          ) : null}
        </dl>
      ) : takes ? (
        <p class="mt-0 mb-4 text-[13px] text-muted">
          No credential stored. Runs attributed to you (builds you approve, plans you trigger) wait until you add
          one.
        </p>
      ) : null}
      {takes ? (
        <form class="flex flex-col gap-3" onSubmit={(e) => void save(e)}>
          {kinds.length > 1 ? (
            <fieldset class="m-0 flex flex-wrap gap-4 border-0 p-0 text-[13px]" disabled={busy}>
              <legend class="mb-1.5 p-0 text-muted">{credential.stored ? "Replace with" : "Add"}</legend>
              {kinds.map((k) => (
                <label key={k} class="inline-flex items-center gap-1.5">
                  <input
                    type="radio"
                    name={`kind-${credential.provider}`}
                    value={k}
                    checked={kind === k}
                    onChange={() => setKind(k)}
                  />
                  {kindLabel(credential.provider, k)}
                </label>
              ))}
            </fieldset>
          ) : (
            <p class="m-0 text-[13px] text-muted">
              {credential.stored ? "Replace with" : "Add"} your {kindLabel(credential.provider, kind)}
            </p>
          )}
          <input
            type="password"
            autocomplete="off"
            spellcheck={false}
            aria-label={`${provider} credential`}
            class="rounded-lg border border-line-2 bg-transparent px-2.5 py-1.5 font-mono text-[12px] text-fg placeholder:text-faint"
            placeholder={placeholder(credential.provider, kind)}
            value={value}
            disabled={busy}
            onInput={(e) => {
              const v = (e.target as HTMLInputElement).value;
              setValue(v);
              setSaved(null);
              const g = guessKind(credential.provider, v);
              if (g && kinds.includes(g)) setKind(g);
            }}
          />
          {kinds.includes("oauth-token") ? (
            <p class={`m-0 text-[12.5px] ${kind === "oauth-token" ? "ps-note" : "text-muted"}`}>
              A setup-token, made with claude setup-token, belongs to a Claude subscription. Store only your own: it
              is used only for runs attributed to you, and they count against your subscription's limits.
            </p>
          ) : (
            <p class="m-0 text-[12.5px] text-muted">
              Store an API key of your own: it is used only for runs attributed to you, and they are billed to its
              account.
            </p>
          )}
          {problem ? <p class="m-0 text-[12.5px] text-red">{problem}</p> : null}
          {error ? <p class="ps-note ps-note--red m-0">{error}</p> : null}
          {saved ? <p class="ps-note ps-note--green m-0">{saved}</p> : null}
          <div class="flex flex-wrap gap-2">
            <button type="submit" class="ps-action ps-action--primary" disabled={busy || value === "" || problem !== null}>
              {busy ? "Saving…" : credential.stored ? "Replace" : "Save"}
            </button>
            {credential.stored ? (
              <button
                type="button"
                class={`ps-action ${confirmDelete ? "is-confirming" : ""}`}
                disabled={busy}
                onClick={() => void remove()}
              >
                {confirmDelete ? "Confirm delete?" : "Delete"}
              </button>
            ) : null}
          </div>
        </form>
      ) : (
        <div class="flex flex-col gap-3">
          {credential.stored ? (
            <p class="m-0 text-[13px] text-muted">
              maul no longer runs {provider} agents, so it no longer uses this credential and it cannot be
              replaced here. Delete it if you no longer need it stored.
            </p>
          ) : null}
          {error ? <p class="ps-note ps-note--red m-0">{error}</p> : null}
          {saved ? <p class="ps-note ps-note--green m-0">{saved}</p> : null}
          {credential.stored ? (
            <div class="flex flex-wrap gap-2">
              <button
                type="button"
                class={`ps-action ${confirmDelete ? "is-confirming" : ""}`}
                disabled={busy}
                onClick={() => void remove()}
              >
                {confirmDelete ? "Confirm delete?" : "Delete"}
              </button>
            </div>
          ) : null}
        </div>
      )}
    </section>
  );
}

export function AccountView() {
  const [loaded, setLoaded] = useState<Loaded>({ kind: "loading" });

  const load = useCallback(async () => {
    try {
      const account = await fetchAccount();
      if (!account) {
        setLoaded({ kind: "error", message: "The Account page is not available." });
        return;
      }
      setLoaded({ kind: "account", account });
    } catch (e) {
      if (e instanceof AuthRequiredError) setLoaded({ kind: "auth" });
      else setLoaded({ kind: "error", message: e instanceof ForbiddenError ? e.message : messageOf(e) });
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);

  if (loaded.kind === "loading") return <div class="px-5 py-11 text-center text-muted">Loading…</div>;
  if (loaded.kind === "auth") {
    return (
      <Panel title="Authentication required">
        <p class="mt-0">Sign in to manage your model credentials.</p>
        <a class="ps-action ps-action--primary no-underline" href={signInURL()}>
          Sign in
        </a>
      </Panel>
    );
  }
  if (loaded.kind === "error") return <Panel title="Account unavailable">{loaded.message}</Panel>;
  const account = loaded.account;
  if (!account.self || !account.allowed) return <Panel title="Account unavailable">{account.reason}</Panel>;

  const replace = (c: AccountCredential) =>
    setLoaded({
      kind: "account",
      account: { ...account, credentials: account.credentials.map((x) => (x.provider === c.provider ? c : x)) },
    });

  return (
    <div class="mx-auto flex max-w-[720px] flex-col gap-5">
      <header>
        <h1 class="mx-0 mt-0 mb-1.5 text-[21px] tracking-tight">Account</h1>
        <p class="m-0 text-[13px] text-muted">
          Signed in as {account.name}. Runs attributed to you, the builds you approve and the plans you trigger, are
          paid for with your own model credential. maul never shows a stored credential again, to you or anyone:
          only its last four characters and a fingerprint.
        </p>
      </header>
      {account.credentials.map((c) => (
        <CredentialCard key={c.provider} credential={c} onChanged={replace} />
      ))}
    </div>
  );
}
