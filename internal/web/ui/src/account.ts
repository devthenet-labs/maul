// Pure helpers of the Account page: what the form accepts before it is
// sent, which kind a pasted credential looks like, and how health reads.
// No DOM, so tests/account.test.mjs runs them. The server holds every value
// to the same rules again (v1alpha1.ModelCredentialValue).

import type { AccountCredential, CredentialHealth, CredentialKind } from "./types";

// MAX_CREDENTIAL_BYTES mirrors v1alpha1.ModelCredentialSecretMaxBytes.
export const MAX_CREDENTIAL_BYTES = 3072;

const API_KEY_PREFIX = "sk-ant-api";
const OAUTH_TOKEN_PREFIX = "sk-ant-oat";

export const KIND_LABELS: Record<CredentialKind, string> = {
  "api-key": "Anthropic API key",
  "oauth-token": "Claude setup-token (subscription)",
};

export const PROVIDER_LABELS: Record<string, string> = {
  anthropic: "Anthropic",
};

// guessKind is the kind a pasted credential's prefix names, or null when
// it names none.
export function guessKind(value: string): CredentialKind | null {
  const v = value.trim();
  if (v.startsWith(API_KEY_PREFIX)) return "api-key";
  if (v.startsWith(OAUTH_TOKEN_PREFIX)) return "oauth-token";
  return null;
}

// credentialProblem says why value cannot be stored as kind, or null when
// it can: one token of printable ASCII, at most MAX_CREDENTIAL_BYTES, not
// the other kind's.
export function credentialProblem(value: string, kind: CredentialKind): string | null {
  const v = value.trim();
  if (v === "") return "Paste the credential.";
  if (value.length > MAX_CREDENTIAL_BYTES) return `A credential is at most ${MAX_CREDENTIAL_BYTES} characters.`;
  for (let i = 0; i < v.length; i++) {
    const c = v.charCodeAt(i);
    if (c < 0x21 || c > 0x7e) return "A credential is one token: no spaces, line breaks or other characters.";
  }
  const guessed = guessKind(v);
  if (guessed && guessed !== kind) {
    return `This looks like a ${KIND_LABELS[guessed].toLowerCase()}; choose that kind.`;
  }
  return null;
}

// HEALTH_TEXT is what each health means to the person.
export const HEALTH_TEXT: Record<CredentialHealth, string> = {
  Valid: "Anthropic accepted it.",
  Rejected: "Anthropic rejected it. Replace it: runs attributed to you wait until it is valid.",
  Unknown: "Not checked yet. patchy checks a new credential within moments.",
};

// healthOf is the credential's health, Unknown when none is recorded.
export function healthOf(c: AccountCredential): CredentialHealth {
  return c.health === "" ? "Unknown" : c.health;
}
