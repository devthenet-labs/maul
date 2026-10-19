// Pure helpers of the Account page: what the form accepts before it is
// sent, which kind a pasted credential looks like, and how health reads,
// per provider (Anthropic for claude, OpenAI for codex). No DOM, so
// tests/account.test.mjs runs them. The server holds every value to the same
// rules again (v1alpha1.ModelCredentialValue).

import type { AccountCredential, CredentialHealth, CredentialKind } from "./types";

// MAX_CREDENTIAL_BYTES mirrors v1alpha1.ModelCredentialSecretMaxBytes.
export const MAX_CREDENTIAL_BYTES = 3072;

// The prefixes the providers issue, mirroring v1alpha1.ModelCredentialValue:
// every Anthropic credential starts sk-ant-, every OpenAI key sk- (and never
// sk-ant-).
const ANTHROPIC_PREFIX = "sk-ant-";
const API_KEY_PREFIX = "sk-ant-api";
const OAUTH_TOKEN_PREFIX = "sk-ant-oat";
const OPENAI_PREFIX = "sk-";

// ProviderUI is how the page presents one provider.
interface ProviderUI {
  label: string;
  kinds: Record<string, string>;
  placeholders: Record<string, string>;
  health: Record<CredentialHealth, string>;
}

const UNKNOWN_HEALTH = "Not checked yet. patchy checks a new credential within moments.";

const PROVIDERS: Record<string, ProviderUI> = {
  anthropic: {
    label: "Anthropic",
    kinds: { "api-key": "Anthropic API key", "oauth-token": "Claude setup-token (subscription)" },
    placeholders: { "api-key": "sk-ant-api…", "oauth-token": "sk-ant-oat…" },
    health: {
      Valid: "Anthropic accepted it.",
      Rejected: "Anthropic rejected it. Replace it: runs attributed to you wait until it is valid.",
      Unknown: UNKNOWN_HEALTH,
    },
  },
  openai: {
    label: "OpenAI",
    kinds: { "api-key": "OpenAI API key" },
    placeholders: { "api-key": "sk-proj-…" },
    health: {
      Valid: "OpenAI accepted it.",
      Rejected: "OpenAI rejected it. Replace it: runs attributed to you wait until it is valid.",
      Unknown: UNKNOWN_HEALTH,
    },
  },
};

function ui(provider: string): ProviderUI | undefined {
  return Object.prototype.hasOwnProperty.call(PROVIDERS, provider) ? PROVIDERS[provider] : undefined;
}

// providerLabel is the provider's name as the page shows it.
export function providerLabel(provider: string): string {
  return ui(provider)?.label ?? provider;
}

// kindLabel is a kind's name for provider.
export function kindLabel(provider: string, kind: CredentialKind): string {
  return ui(provider)?.kinds[kind] ?? kind;
}

// placeholder is the input's placeholder for provider and kind.
export function placeholder(provider: string, kind: CredentialKind): string {
  return ui(provider)?.placeholders[kind] ?? "";
}

// healthText is what a health means to the person, for provider.
export function healthText(provider: string, health: CredentialHealth): string {
  return ui(provider)?.health[health] ?? (health === "Unknown" ? UNKNOWN_HEALTH : health);
}

// guessKind is the kind a pasted credential's prefix names for provider, or
// null when it names none (or names another provider's).
export function guessKind(provider: string, value: string): CredentialKind | null {
  const v = value.trim();
  if (provider === "openai") {
    return v.startsWith(OPENAI_PREFIX) && !v.startsWith(ANTHROPIC_PREFIX) ? "api-key" : null;
  }
  if (v.startsWith(API_KEY_PREFIX)) return "api-key";
  if (v.startsWith(OAUTH_TOKEN_PREFIX)) return "oauth-token";
  return null;
}

// otherProvider is the provider whose prefix value carries when it is not
// provider's own, or null.
function otherProvider(provider: string, value: string): string | null {
  const anthropic = value.startsWith(ANTHROPIC_PREFIX);
  if (provider === "openai" && anthropic) return "anthropic";
  if (provider === "anthropic" && value.startsWith(OPENAI_PREFIX) && !anthropic) return "openai";
  return null;
}

// credentialProblem says why value cannot be stored for provider as kind,
// or null when it can: one token of printable ASCII, at most
// MAX_CREDENTIAL_BYTES, a kind the provider takes, not the other kind's and
// not another provider's.
export function credentialProblem(
  provider: string,
  kinds: readonly CredentialKind[],
  value: string,
  kind: CredentialKind,
): string | null {
  const v = value.trim();
  if (v === "") return "Paste the credential.";
  if (value.length > MAX_CREDENTIAL_BYTES) return `A credential is at most ${MAX_CREDENTIAL_BYTES} characters.`;
  for (let i = 0; i < v.length; i++) {
    const c = v.charCodeAt(i);
    if (c < 0x21 || c > 0x7e) return "A credential is one token: no spaces, line breaks or other characters.";
  }
  if (!kinds.includes(kind)) return `${providerLabel(provider)} takes no ${kind}.`;
  const other = otherProvider(provider, v);
  if (other) {
    return `This looks like an ${providerLabel(other)} credential; store it under ${providerLabel(other)}.`;
  }
  const guessed = guessKind(provider, v);
  if (guessed && guessed !== kind) {
    return `This looks like a ${kindLabel(provider, guessed).toLowerCase()}; choose that kind.`;
  }
  return null;
}

// healthOf is the credential's health, Unknown when none is recorded.
export function healthOf(c: AccountCredential): CredentialHealth {
  return c.health === "" ? "Unknown" : c.health;
}
