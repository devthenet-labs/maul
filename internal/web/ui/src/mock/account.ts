// Demo data of the Account page: one stored credential, so the committed
// kit build shows the stored state. Writes change only this in-memory copy.

import type { Account, AccountCredential, CredentialKind } from "../types";

let demo: Account | null = null;

export function mockAccount(): Account {
  if (!demo) {
    demo = {
      name: "demo",
      self: true,
      allowed: true,
      reason: "",
      credentials: [
        {
          provider: "anthropic",
          stored: true,
          kind: "api-key",
          fingerprint: "x9Qa:3f1c09ab27de",
          health: "Valid",
          lastUsedAt: new Date(Date.now() - 3 * 3_600_000).toISOString(),
          lastRejectedAt: "",
        },
      ],
    };
  }
  return demo;
}

export function mockPutCredential(provider: string, kind: CredentialKind, value: string): AccountCredential {
  const tail = value.trim().slice(-4);
  const c: AccountCredential = {
    provider,
    stored: true,
    kind,
    fingerprint: `${tail.padStart(4, "*")}:000000000000`,
    health: "Unknown",
    lastUsedAt: "",
    lastRejectedAt: "",
  };
  const a = mockAccount();
  a.credentials = a.credentials.map((x) => (x.provider === provider ? c : x));
  return c;
}

export function mockDeleteCredential(provider: string): void {
  const a = mockAccount();
  a.credentials = a.credentials.map((x) =>
    x.provider === provider
      ? { provider, stored: false, kind: "", fingerprint: "", health: "", lastUsedAt: "", lastRejectedAt: "" }
      : x,
  );
}
